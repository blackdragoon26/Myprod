package web

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/blackdragoon26/Myprod/internal/pool"
)

const defaultAddr = "127.0.0.1:8088"

type server struct {
	addr     string
	store    pool.Store
	exe      string
	cwd      string
	password string

	mu       sync.Mutex
	sessions map[string]string
}

type viewData struct {
	PoolName    string
	Nodes       []nodeView
	Apps        []appView
	Smokes      []smokeView
	NextOverlay string
	Output      string
	Error       string
	Authed      bool
	NeedsLogin  bool
	CSRF        string
	Addr        string
	PasswordSet bool
	GeneratedAt string
}

type nodeView struct {
	Name      string
	Role      string
	Provider  string
	PublicIP  string
	OverlayIP string
	State     string
	Joined    bool
}

type appView struct {
	Name   string
	Image  string
	Domain string
	Port   int
	Node   string
	Status string
}

type smokeView struct {
	Label  string
	URL    string
	Status string
	OK     bool
}

func Serve(storeDir string, args []string) error {
	addr := defaultAddr
	if len(args) > 0 {
		if len(args) != 2 || args[0] != "--addr" {
			return errors.New("usage: poolctl web [--addr host:port]")
		}
		addr = args[1]
	}

	exe, err := os.Executable()
	if err != nil {
		return err
	}
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}

	s := &server{
		addr:     addr,
		store:    pool.NewStore(storeDir),
		exe:      exe,
		cwd:      cwd,
		password: os.Getenv("POOLCTL_WEB_PASSWORD"),
		sessions: make(map[string]string),
	}
	if s.password == "" && !isLoopbackAddr(addr) {
		return errors.New("POOLCTL_WEB_PASSWORD is required when binding poolctl web outside localhost")
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleDashboard)
	mux.HandleFunc("/login", s.handleLogin)
	mux.HandleFunc("/logout", s.handleLogout)
	mux.HandleFunc("/action", s.handleAction)

	fmt.Printf("poolctl web listening on http://%s\n", addr)
	if s.password == "" {
		fmt.Println("auth: disabled for localhost development")
	} else {
		fmt.Println("auth: password protected")
	}
	return http.ListenAndServe(addr, securityHeaders(mux))
}

func (s *server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	authed, csrf := s.auth(r)
	if !authed {
		s.render(w, viewData{NeedsLogin: true, PasswordSet: s.password != "", Addr: s.addr})
		return
	}
	s.renderDashboard(w, r, "", "", csrf)
}

func (s *server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	if s.password == "" {
		s.createSession(w)
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	if subtle.ConstantTimeCompare([]byte(r.FormValue("password")), []byte(s.password)) != 1 {
		s.render(w, viewData{NeedsLogin: true, Error: "invalid password", PasswordSet: true, Addr: s.addr})
		return
	}
	s.createSession(w)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *server) handleLogout(w http.ResponseWriter, r *http.Request) {
	cookie, err := r.Cookie("poolctl_session")
	if err == nil {
		s.mu.Lock()
		delete(s.sessions, cookie.Value)
		s.mu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{Name: "poolctl_session", Value: "", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteLaxMode})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *server) handleAction(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	authed, csrf := s.auth(r)
	if !authed {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	if r.FormValue("csrf") != csrf {
		if wantsJSON(r) {
			writeActionJSON(w, "", "invalid form token; refresh and try again")
			return
		}
		s.renderDashboard(w, r, "", "invalid form token; refresh and try again", csrf)
		return
	}

	if r.FormValue("action") == "node-add" {
		node, err := nodeFromForm(r)
		if err != nil {
			if wantsJSON(r) {
				writeActionJSON(w, "", err.Error())
				return
			}
			s.renderDashboard(w, r, "", err.Error(), csrf)
			return
		}
		if err := s.store.AddNode(node); err != nil {
			if wantsJSON(r) {
				writeActionJSON(w, "", err.Error())
				return
			}
			s.renderDashboard(w, r, "", err.Error(), csrf)
			return
		}
		output := fmt.Sprintf("registered node %s in .poolctl/config.yaml\n", node.Name)
		if wantsJSON(r) {
			writeActionJSON(w, output, "")
			return
		}
		s.renderDashboard(w, r, output, "", csrf)
		return
	}

	args, err := actionArgs(r)
	if err != nil {
		if wantsJSON(r) {
			writeActionJSON(w, "", err.Error())
			return
		}
		s.renderDashboard(w, r, "", err.Error(), csrf)
		return
	}
	output, err := s.runCommand(r.Context(), args...)
	if err != nil {
		if wantsJSON(r) {
			writeActionJSON(w, output, err.Error())
			return
		}
		s.renderDashboard(w, r, output, err.Error(), csrf)
		return
	}
	if wantsJSON(r) {
		writeActionJSON(w, output, "")
		return
	}
	s.renderDashboard(w, r, output, "", csrf)
}

type actionResponse struct {
	Output string `json:"output"`
	Error  string `json:"error"`
}

func wantsJSON(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Accept"), "application/json")
}

func writeActionJSON(w http.ResponseWriter, output, errText string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(actionResponse{Output: output, Error: errText})
}

func actionArgs(r *http.Request) ([]string, error) {
	action := r.FormValue("action")
	switch action {
	case "control-status":
		return []string{"control-plane", "status"}, nil
	case "guard":
		return []string{"guard", "check"}, nil
	case "render":
		return []string{"render"}, nil
	case "app-render":
		app := strings.TrimSpace(r.FormValue("app"))
		if app == "" {
			return nil, errors.New("missing app")
		}
		return []string{"app", "render", app}, nil
	case "app-deploy":
		app := strings.TrimSpace(r.FormValue("app"))
		if app == "" {
			return nil, errors.New("missing app")
		}
		return []string{"app", "deploy", app}, nil
	case "node-freeze", "node-unfreeze", "node-drain", "node-cancel-drain", "node-join":
		node := strings.TrimSpace(r.FormValue("node"))
		if node == "" {
			return nil, errors.New("missing node")
		}
		return []string{"node", strings.TrimPrefix(action, "node-"), node}, nil
	default:
		return nil, fmt.Errorf("unknown action %q", action)
	}
}

func (s *server) renderDashboard(w http.ResponseWriter, r *http.Request, output, errText, csrf string) {
	cfg, state, err := s.store.Load()
	if err != nil {
		s.render(w, viewData{Error: err.Error(), Authed: true, CSRF: csrf, Addr: s.addr, PasswordSet: s.password != ""})
		return
	}

	data := viewData{
		PoolName:    cfg.Name,
		Nodes:       nodeViews(cfg, state),
		Apps:        appViews(cfg, state),
		Smokes:      s.smokes(r.Context(), cfg),
		NextOverlay: nextOverlayIP(cfg),
		Output:      output,
		Error:       errText,
		Authed:      true,
		CSRF:        csrf,
		Addr:        s.addr,
		PasswordSet: s.password != "",
		GeneratedAt: time.Now().Format("15:04:05"),
	}
	s.render(w, data)
}

func nodeViews(cfg pool.Config, state pool.State) []nodeView {
	out := make([]nodeView, 0, len(cfg.Nodes))
	for _, node := range cfg.Nodes {
		st := state.Nodes[node.Name]
		label := "ready"
		if st.Draining {
			label = "draining"
		} else if st.Frozen {
			label = "frozen"
		}
		out = append(out, nodeView{
			Name: node.Name, Role: node.Role, Provider: node.Provider,
			PublicIP: node.PublicIP, OverlayIP: node.OverlayIP, State: label, Joined: st.Joined,
		})
	}
	return out
}

func nodeFromForm(r *http.Request) (pool.Node, error) {
	node := pool.Node{
		Name:      strings.TrimSpace(r.FormValue("name")),
		Role:      "worker",
		Provider:  strings.TrimSpace(r.FormValue("provider")),
		CostMode:  strings.TrimSpace(r.FormValue("cost_mode")),
		Placement: strings.TrimSpace(r.FormValue("placement")),
		PublicIP:  strings.TrimSpace(r.FormValue("public_ip")),
		SSHUser:   strings.TrimSpace(r.FormValue("ssh_user")),
		SSHKey:    strings.TrimSpace(r.FormValue("ssh_key")),
		OverlayIP: strings.TrimSpace(r.FormValue("overlay_ip")),
		Guard: pool.Guard{
			Enabled:          true,
			MaxDiskPercent:   80,
			MaxMemoryPercent: 85,
			MaxLoad1:         3.5,
		},
	}
	if node.Provider == "" {
		node.Provider = "digitalocean"
	}
	if node.CostMode == "" {
		node.CostMode = "credit_temporary"
	}
	if node.Placement == "" {
		node.Placement = "burst"
	}
	if net.ParseIP(node.PublicIP) == nil {
		return pool.Node{}, errors.New("public IP must be a valid IP address")
	}
	if net.ParseIP(node.OverlayIP) == nil {
		return pool.Node{}, errors.New("overlay IP must be a valid IP address")
	}
	return node, nil
}

func nextOverlayIP(cfg pool.Config) string {
	used := map[string]bool{}
	for _, node := range cfg.Nodes {
		used[node.OverlayIP] = true
	}
	for i := 2; i < 255; i++ {
		candidate := fmt.Sprintf("10.44.0.%d", i)
		if !used[candidate] {
			return candidate
		}
	}
	return "10.44.0.254"
}

func appViews(cfg pool.Config, state pool.State) []appView {
	out := make([]appView, 0, len(cfg.Apps))
	for _, app := range cfg.Apps {
		st := state.Apps[app.Name]
		status := st.Status
		if status == "" {
			status = "not-deployed"
		}
		node := st.Node
		if node == "" {
			node = "-"
		}
		out = append(out, appView{
			Name: app.Name, Image: app.Image, Domain: app.Domain, Port: app.Port,
			Node: node, Status: status,
		})
	}
	return out
}

func (s *server) smokes(ctx context.Context, cfg pool.Config) []smokeView {
	var checks []smokeView
	for _, app := range cfg.Apps {
		if app.Domain == "" {
			continue
		}
		checks = append(checks, s.smoke(ctx, "http", "http://"+app.Domain+"/"))
		checks = append(checks, s.smoke(ctx, "https", "https://"+app.Domain+"/"))
	}
	return checks
}

func (s *server) smoke(ctx context.Context, label, url string) smokeView {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return smokeView{Label: label, URL: url, Status: err.Error()}
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return smokeView{Label: label, URL: url, Status: err.Error()}
	}
	defer resp.Body.Close()
	return smokeView{Label: label, URL: url, Status: resp.Status, OK: resp.StatusCode >= 200 && resp.StatusCode < 400}
}

func (s *server) runCommand(ctx context.Context, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, s.exe, args...)
	cmd.Dir = s.cwd
	cmd.Env = os.Environ()
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	if ctx.Err() == context.DeadlineExceeded {
		return out.String(), errors.New("command timed out")
	}
	return out.String(), err
}

func (s *server) auth(r *http.Request) (bool, string) {
	if s.password == "" {
		return true, "local"
	}
	cookie, err := r.Cookie("poolctl_session")
	if err != nil {
		return false, ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	csrf, ok := s.sessions[cookie.Value]
	return ok, csrf
}

func (s *server) createSession(w http.ResponseWriter) {
	token := randomToken()
	csrf := randomToken()
	s.mu.Lock()
	s.sessions[token] = csrf
	s.mu.Unlock()
	http.SetCookie(w, &http.Cookie{
		Name:     "poolctl_session",
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   false,
		MaxAge:   int((12 * time.Hour).Seconds()),
	})
}

func randomToken() string {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

func isLoopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "localhost" || host == "" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "same-origin")
		next.ServeHTTP(w, r)
	})
}

func (s *server) render(w http.ResponseWriter, data viewData) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := dashboardTemplate.Execute(w, data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

var dashboardTemplate = template.Must(template.New("dashboard").Funcs(template.FuncMap{
	"stateClass": func(s string) string {
		switch s {
		case "ready", "deployed", "200 OK":
			return "ok"
		case "frozen", "draining":
			return "warn"
		default:
			return "muted"
		}
	},
	"base": filepath.Base,
}).Parse(`<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>poolctl</title>
  <style>
    :root { color-scheme: dark; --bg:#0d1117; --panel:#151b23; --line:#30363d; --text:#e6edf3; --muted:#8b949e; --ok:#3fb950; --warn:#d29922; --bad:#f85149; --accent:#58a6ff; }
    * { box-sizing: border-box; }
    body { margin:0; background:var(--bg); color:var(--text); font:14px/1.45 ui-sans-serif, system-ui, -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif; }
    main { max-width:1180px; margin:0 auto; padding:28px; }
    header { display:flex; align-items:flex-end; justify-content:space-between; gap:16px; margin-bottom:22px; }
    h1 { margin:0; font-size:28px; }
    h2 { margin:0 0 14px; font-size:18px; }
    a { color:var(--accent); }
    .muted { color:var(--muted); }
    .grid { display:grid; grid-template-columns: repeat(12, 1fr); gap:14px; }
    section, .login { background:var(--panel); border:1px solid var(--line); border-radius:8px; padding:18px; }
    .span-4 { grid-column: span 4; } .span-6 { grid-column: span 6; } .span-8 { grid-column: span 8; } .span-12 { grid-column: span 12; }
    table { width:100%; border-collapse:collapse; }
    th, td { padding:10px 8px; border-bottom:1px solid var(--line); text-align:left; vertical-align:top; }
    th { color:var(--muted); font-weight:600; font-size:12px; text-transform:uppercase; }
    tr:last-child td { border-bottom:0; }
    .pill { display:inline-flex; align-items:center; min-height:24px; padding:2px 8px; border:1px solid var(--line); border-radius:999px; color:var(--muted); }
    .pill.ok { color:var(--ok); border-color:rgba(63,185,80,.4); }
    .pill.warn { color:var(--warn); border-color:rgba(210,153,34,.4); }
    .pill.bad { color:var(--bad); border-color:rgba(248,81,73,.4); }
    form.inline { display:inline; }
    button, input { font:inherit; }
    button { min-height:34px; border:1px solid var(--line); border-radius:6px; background:#21262d; color:var(--text); padding:6px 10px; cursor:pointer; }
    button.primary { background:#1f6feb; border-color:#1f6feb; }
    button.danger { color:#ffb4ad; }
    button:hover { border-color:var(--accent); }
    input { width:100%; min-height:38px; border:1px solid var(--line); border-radius:6px; background:#0d1117; color:var(--text); padding:8px 10px; }
    label { display:grid; gap:6px; color:var(--muted); font-size:12px; font-weight:600; text-transform:uppercase; }
    .actions { display:flex; flex-wrap:wrap; gap:8px; }
    .node-form { display:grid; grid-template-columns: repeat(3, minmax(0, 1fr)); gap:12px; align-items:end; }
    pre { margin:0; white-space:pre-wrap; word-break:break-word; background:#0d1117; border:1px solid var(--line); border-radius:6px; padding:12px; max-height:480px; overflow:auto; }
    .output-head { display:flex; align-items:center; justify-content:space-between; gap:12px; margin-bottom:14px; }
    .output-head h2 { margin:0; }
    .notice { margin-bottom:14px; padding:10px 12px; border-radius:6px; border:1px solid var(--line); }
    .notice.err { border-color:rgba(248,81,73,.55); color:#ffb4ad; }
    .notice.run { border-color:rgba(88,166,255,.55); color:#9ecbff; }
    .login { max-width:420px; margin:12vh auto; }
    .top-actions { display:flex; gap:8px; align-items:center; }
    body.busy button { opacity:.65; }
    body.busy button.active-action { opacity:1; border-color:var(--accent); }
    @media (max-width: 820px) { main { padding:16px; } .span-4,.span-6,.span-8,.span-12 { grid-column: span 12; } header { align-items:flex-start; flex-direction:column; } .node-form { grid-template-columns:1fr; } }
  </style>
</head>
<body>
{{if .NeedsLogin}}
  <div class="login">
    <h1>poolctl</h1>
    <p class="muted">{{if .PasswordSet}}Sign in to manage the pool.{{else}}Local development auth is disabled.{{end}}</p>
    {{if .Error}}<div class="notice err">{{.Error}}</div>{{end}}
    <form method="post" action="/login">
      {{if .PasswordSet}}<p><input type="password" name="password" placeholder="Admin password" autofocus></p>{{end}}
      <button class="primary" type="submit">Open Dashboard</button>
    </form>
  </div>
{{else}}
  <main>
    <header>
      <div>
        <h1>{{.PoolName}}</h1>
        <div class="muted">poolctl dashboard · {{.Addr}} · refreshed {{.GeneratedAt}}</div>
      </div>
      <div class="top-actions">
        <form method="post" action="/action" class="inline"><input type="hidden" name="csrf" value="{{.CSRF}}"><input type="hidden" name="action" value="control-status"><button class="primary">Refresh Control Plane</button></form>
        <form method="post" action="/logout" class="inline"><button>Logout</button></form>
      </div>
    </header>
    {{if .Error}}<div class="notice err">{{.Error}}</div>{{end}}
    <div id="runNotice" class="notice run" hidden>Running action. Leave this tab open; command output will appear below when it finishes.</div>
    <div id="asyncError" class="notice err" hidden></div>
    <div class="grid">
      <section class="span-8">
        <h2>Apps</h2>
        <table>
          <thead><tr><th>Name</th><th>Domain</th><th>Status</th><th>Node</th><th>Actions</th></tr></thead>
          <tbody>{{range .Apps}}
            <tr>
              <td><strong>{{.Name}}</strong><div class="muted">{{.Image}}</div></td>
              <td><a href="https://{{.Domain}}/" target="_blank" rel="noreferrer">{{.Domain}}</a><div class="muted">port {{.Port}}</div></td>
              <td><span class="pill {{stateClass .Status}}">{{.Status}}</span></td>
              <td>{{.Node}}</td>
              <td class="actions">
                <form method="post" action="/action" class="inline"><input type="hidden" name="csrf" value="{{$.CSRF}}"><input type="hidden" name="action" value="app-render"><input type="hidden" name="app" value="{{.Name}}"><button>Render</button></form>
                <form method="post" action="/action" class="inline" data-confirm="Are you sure? Deploying updates a live Nomad workload and can affect public traffic."><input type="hidden" name="csrf" value="{{$.CSRF}}"><input type="hidden" name="action" value="app-deploy"><input type="hidden" name="app" value="{{.Name}}"><button class="primary">Deploy</button></form>
              </td>
            </tr>
          {{end}}</tbody>
        </table>
      </section>
      <section class="span-4">
        <h2>Public Smoke</h2>
        <table>
          <tbody>{{range .Smokes}}<tr><td>{{.Label}}</td><td><a href="{{.URL}}" target="_blank" rel="noreferrer">{{.URL}}</a></td><td><span class="pill {{if .OK}}ok{{else}}bad{{end}}">{{.Status}}</span></td></tr>{{end}}</tbody>
        </table>
      </section>
      <section class="span-8">
        <h2>Nodes</h2>
        <table>
          <thead><tr><th>Name</th><th>Role</th><th>Public</th><th>Overlay</th><th>State</th><th>Actions</th></tr></thead>
          <tbody>{{range .Nodes}}
            <tr>
              <td><strong>{{.Name}}</strong><div class="muted">{{.Provider}}</div></td><td>{{.Role}}</td><td>{{.PublicIP}}</td><td>{{.OverlayIP}}</td>
              <td><span class="pill {{stateClass .State}}">{{.State}}</span></td>
              <td class="actions">
                <form method="post" action="/action" class="inline" data-confirm="Are you sure? Freeze makes the real Nomad node ineligible for new workloads."><input type="hidden" name="csrf" value="{{$.CSRF}}"><input type="hidden" name="action" value="node-freeze"><input type="hidden" name="node" value="{{.Name}}"><button>Freeze</button></form>
                <form method="post" action="/action" class="inline" data-confirm="Are you sure? Unfreeze lets Nomad schedule workloads on this node immediately."><input type="hidden" name="csrf" value="{{$.CSRF}}"><input type="hidden" name="action" value="node-unfreeze"><input type="hidden" name="node" value="{{.Name}}"><button>Unfreeze</button></form>
                {{if eq .State "draining"}}<form method="post" action="/action" class="inline" data-confirm="Are you sure? Cancelling drain leaves the node frozen until you explicitly unfreeze it."><input type="hidden" name="csrf" value="{{$.CSRF}}"><input type="hidden" name="action" value="node-cancel-drain"><input type="hidden" name="node" value="{{.Name}}"><button class="danger">Cancel Drain</button></form>{{else}}<form method="post" action="/action" class="inline" data-confirm="Are you sure? Drain can migrate or stop live allocations and interrupt workloads."><input type="hidden" name="csrf" value="{{$.CSRF}}"><input type="hidden" name="action" value="node-drain"><input type="hidden" name="node" value="{{.Name}}"><button class="danger">Drain</button></form>{{end}}
                {{if ne .Role "control-plane"}}{{if .Joined}}<button disabled>Joined</button>{{else}}<form method="post" action="/action" class="inline" data-confirm="Are you sure? Join installs system services, networking, Docker, and Nomad on the VPS."><input type="hidden" name="csrf" value="{{$.CSRF}}"><input type="hidden" name="action" value="node-join"><input type="hidden" name="node" value="{{.Name}}"><button class="primary">Join</button></form>{{end}}{{end}}
              </td>
            </tr>
          {{end}}</tbody>
        </table>
      </section>
      <section class="span-4">
        <h2>Pool Actions</h2>
        <div class="actions">
          <form method="post" action="/action"><input type="hidden" name="csrf" value="{{.CSRF}}"><input type="hidden" name="action" value="render"><button>Render Bundle</button></form>
          <form method="post" action="/action"><input type="hidden" name="csrf" value="{{.CSRF}}"><input type="hidden" name="action" value="guard"><button>Run Guard</button></form>
          <form method="post" action="/action"><input type="hidden" name="csrf" value="{{.CSRF}}"><input type="hidden" name="action" value="control-status"><button>Control Status</button></form>
        </div>
      </section>
      <section class="span-12">
        <h2>Add VPS Node</h2>
        <form method="post" action="/action" class="node-form" data-confirm="Are you sure? This records a new infrastructure node in the operator state.">
          <input type="hidden" name="csrf" value="{{.CSRF}}">
          <input type="hidden" name="action" value="node-add">
          <label>Name<input name="name" placeholder="do-worker-1" autocomplete="off"></label>
          <label>Provider<input name="provider" value="digitalocean" autocomplete="off"></label>
          <label>Public IP<input name="public_ip" placeholder="203.0.113.10" autocomplete="off"></label>
          <label>SSH User<input name="ssh_user" value="ubuntu" autocomplete="off"></label>
          <label>SSH Key<input name="ssh_key" placeholder="~/.ssh/keys/digitalocean-worker.key" autocomplete="off"></label>
          <label>Overlay IP<input name="overlay_ip" value="{{.NextOverlay}}" autocomplete="off"></label>
          <input type="hidden" name="cost_mode" value="credit_temporary">
          <input type="hidden" name="placement" value="burst">
          <button class="primary" type="submit">Register Node</button>
        </form>
        <p class="muted">This records an SSH-ready VPS in poolctl. Use the Join action in the Nodes table to install WireGuard, Docker, and Nomad on a worker.</p>
      </section>
      <section class="span-12">
        <div class="output-head">
          <h2>Command Output</h2>
          <button id="copyOutput" type="button">Copy Output</button>
        </div>
        <pre id="commandOutput">{{if .Output}}{{.Output}}{{else}}Run an action to see CLI output here.{{end}}</pre>
      </section>
    </div>
  </main>
  <script>
    const output = document.getElementById("commandOutput");
    const runNotice = document.getElementById("runNotice");
    const asyncError = document.getElementById("asyncError");
    const copyButton = document.getElementById("copyOutput");
    const idleText = "Run an action to see CLI output here.";

    function setRunning(button) {
      document.body.classList.add("busy");
      runNotice.hidden = false;
      asyncError.hidden = true;
      asyncError.textContent = "";
      output.textContent = "Running " + (button ? button.textContent.trim() : "action") + "...\n";
      document.querySelectorAll("button").forEach((btn) => {
        if (btn !== copyButton) btn.disabled = true;
      });
      if (button) button.classList.add("active-action");
    }

    function clearRunning(button) {
      document.body.classList.remove("busy");
      runNotice.hidden = true;
      document.querySelectorAll("button").forEach((btn) => {
        btn.disabled = false;
        btn.classList.remove("active-action");
      });
    }

    document.querySelectorAll('form[action="/action"]').forEach((form) => {
      form.addEventListener("submit", async (event) => {
        if (!window.fetch || !window.FormData) return;
        event.preventDefault();
        if (form.dataset.confirm && !window.confirm(form.dataset.confirm)) return;
        const button = event.submitter || form.querySelector("button");
        setRunning(button);
        try {
          const response = await fetch("/action", {
            method: "POST",
            headers: { "Accept": "application/json", "X-Requested-With": "poolctl-web" },
            body: new FormData(form)
          });
          const contentType = response.headers.get("content-type") || "";
          let data;
          if (contentType.includes("application/json")) {
            data = await response.json();
          } else {
            const text = await response.text();
            data = { output: text, error: response.ok ? "" : "server returned a non-JSON response" };
          }
          output.textContent = data.output || "";
          if (!output.textContent) output.textContent = data.error ? "" : idleText;
          if (data.error) {
            asyncError.textContent = data.error;
            asyncError.hidden = false;
            if (!output.textContent) output.textContent = "Action failed before producing command output.";
          } else if (form.querySelector('input[name="action"]')?.value === "node-add") {
            window.location.href = "/";
            return;
          }
        } catch (error) {
          asyncError.textContent = error && error.message ? error.message : String(error);
          asyncError.hidden = false;
          output.textContent = "Action failed before producing command output.";
        } finally {
          clearRunning(button);
        }
      });
    });

    copyButton.addEventListener("click", async () => {
      const text = output.textContent || "";
      if (!text || text === idleText) return;
      try {
        await navigator.clipboard.writeText(text);
        const previous = copyButton.textContent;
        copyButton.textContent = "Copied";
        setTimeout(() => { copyButton.textContent = previous; }, 1200);
      } catch (_) {
        const range = document.createRange();
        range.selectNodeContents(output);
        const selection = window.getSelection();
        selection.removeAllRanges();
        selection.addRange(range);
      }
    });
  </script>
{{end}}
</body>
</html>`))
