package web

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
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
		s.renderDashboard(w, r, "", "invalid form token; refresh and try again", csrf)
		return
	}

	args, err := actionArgs(r)
	if err != nil {
		s.renderDashboard(w, r, "", err.Error(), csrf)
		return
	}
	output, err := s.runCommand(r.Context(), args...)
	if err != nil {
		s.renderDashboard(w, r, output, err.Error(), csrf)
		return
	}
	s.renderDashboard(w, r, output, "", csrf)
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
	case "node-freeze", "node-unfreeze", "node-drain":
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
			PublicIP: node.PublicIP, OverlayIP: node.OverlayIP, State: label,
		})
	}
	return out
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
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
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
    .actions { display:flex; flex-wrap:wrap; gap:8px; }
    pre { margin:0; white-space:pre-wrap; word-break:break-word; background:#0d1117; border:1px solid var(--line); border-radius:6px; padding:12px; max-height:480px; overflow:auto; }
    .notice { margin-bottom:14px; padding:10px 12px; border-radius:6px; border:1px solid var(--line); }
    .notice.err { border-color:rgba(248,81,73,.55); color:#ffb4ad; }
    .login { max-width:420px; margin:12vh auto; }
    .top-actions { display:flex; gap:8px; align-items:center; }
    @media (max-width: 820px) { main { padding:16px; } .span-4,.span-6,.span-8,.span-12 { grid-column: span 12; } header { align-items:flex-start; flex-direction:column; } }
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
                <form method="post" action="/action" class="inline"><input type="hidden" name="csrf" value="{{$.CSRF}}"><input type="hidden" name="action" value="app-deploy"><input type="hidden" name="app" value="{{.Name}}"><button class="primary">Deploy</button></form>
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
                <form method="post" action="/action" class="inline"><input type="hidden" name="csrf" value="{{$.CSRF}}"><input type="hidden" name="action" value="node-freeze"><input type="hidden" name="node" value="{{.Name}}"><button>Freeze</button></form>
                <form method="post" action="/action" class="inline"><input type="hidden" name="csrf" value="{{$.CSRF}}"><input type="hidden" name="action" value="node-unfreeze"><input type="hidden" name="node" value="{{.Name}}"><button>Unfreeze</button></form>
                <form method="post" action="/action" class="inline"><input type="hidden" name="csrf" value="{{$.CSRF}}"><input type="hidden" name="action" value="node-drain"><input type="hidden" name="node" value="{{.Name}}"><button class="danger">Drain</button></form>
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
        <h2>Command Output</h2>
        <pre>{{if .Output}}{{.Output}}{{else}}Run an action to see CLI output here.{{end}}</pre>
      </section>
    </div>
  </main>
{{end}}
</body>
</html>`))
