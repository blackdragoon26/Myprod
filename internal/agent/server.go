package agent

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/blackdragoon26/Myprod/internal/pool"
)

const defaultAddr = "127.0.0.1:8790"

type server struct {
	addr  string
	store pool.Store
	token string
}

type response struct {
	OK      bool        `json:"ok"`
	Error   string      `json:"error,omitempty"`
	Output  string      `json:"output,omitempty"`
	Config  any         `json:"config,omitempty"`
	State   any         `json:"state,omitempty"`
	Checks  []check     `json:"checks,omitempty"`
	System  systemState `json:"system,omitempty"`
	Updated string      `json:"updated,omitempty"`
}

type check struct {
	Label     string `json:"label"`
	URL       string `json:"url"`
	OK        bool   `json:"ok"`
	Status    string `json:"status"`
	LatencyMS int64  `json:"latencyMs"`
}

type systemState struct {
	Nomad     string `json:"nomad"`
	Traefik   string `json:"traefik"`
	WireGuard string `json:"wireguard"`
}

func Serve(args []string) error {
	addr := defaultAddr
	storeDir := ".poolctl"
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--addr":
			i++
			if i >= len(args) {
				return errors.New("missing value for --addr")
			}
			addr = args[i]
		case "--store":
			i++
			if i >= len(args) {
				return errors.New("missing value for --store")
			}
			storeDir = args[i]
		default:
			return errors.New("usage: poolctl agent [--addr host:port] [--store dir]")
		}
	}
	token := strings.TrimSpace(os.Getenv("POOLCTL_AGENT_TOKEN"))
	if token == "" {
		return errors.New("POOLCTL_AGENT_TOKEN is required")
	}
	if !isLoopback(addr) && len(token) < 32 {
		return errors.New("POOLCTL_AGENT_TOKEN must be at least 32 characters when binding outside localhost")
	}
	s := &server{addr: addr, store: pool.NewStore(storeDir), token: token}

	mux := http.NewServeMux()
	mux.HandleFunc("/__poolctl/api/health", s.handleHealth)
	mux.HandleFunc("/__poolctl/api/status", s.handleStatus)
	mux.HandleFunc("/__poolctl/api/action", s.handleAction)
	mux.HandleFunc("/__poolctl/api/smoke", s.handleSmoke)

	fmt.Printf("poolctl agent listening on http://%s\n", addr)
	return http.ListenAndServe(addr, withHeaders(mux))
}

func (s *server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, response{OK: false, Error: "method not allowed"})
		return
	}
	writeJSON(w, http.StatusOK, response{OK: true, Updated: time.Now().UTC().Format(time.RFC3339)})
}

func (s *server) handleStatus(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(w, r) {
		return
	}
	cfg, state, err := s.store.Load()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, response{OK: false, Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, response{
		OK:      true,
		Config:  cfg,
		State:   state,
		System:  readSystemState(r.Context()),
		Checks:  runSmokes(r.Context(), cfg),
		Updated: time.Now().UTC().Format(time.RFC3339),
	})
}

func (s *server) handleSmoke(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(w, r) {
		return
	}
	cfg, _, err := s.store.Load()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, response{OK: false, Error: err.Error()})
		return
	}
	checks := runSmokes(r.Context(), cfg)
	ok := true
	for _, check := range checks {
		ok = ok && check.OK
	}
	writeJSON(w, http.StatusOK, response{OK: ok, Checks: checks, Updated: time.Now().UTC().Format(time.RFC3339)})
}

func (s *server) handleAction(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, response{OK: false, Error: "method not allowed"})
		return
	}
	var req struct {
		Action string `json:"action"`
		Name   string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, response{OK: false, Error: err.Error()})
		return
	}
	output, err := s.runAction(r.Context(), req.Action, req.Name)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, response{OK: false, Error: err.Error(), Output: output})
		return
	}
	writeJSON(w, http.StatusOK, response{OK: true, Output: output, Updated: time.Now().UTC().Format(time.RFC3339)})
}

func (s *server) runAction(ctx context.Context, action, name string) (string, error) {
	switch action {
	case "control-status":
		return run(ctx, "systemctl", "is-active", "nomad", "traefik", "wg-quick@wg0")
	case "app-deploy":
		if name == "" {
			return "", errors.New("missing app name")
		}
		cfg, state, err := s.store.Load()
		if err != nil {
			return "", err
		}
		app, ok := cfg.FindApp(name)
		if !ok {
			return "", fmt.Errorf("unknown app %q", name)
		}
		file, err := pool.RenderAppJob(cfg, name)
		if err != nil {
			return "", err
		}
		tmpDir := "/tmp/poolctl-agent-rendered"
		if err := pool.WriteRendered(tmpDir, []pool.RenderedFile{file}); err != nil {
			return "", err
		}
		out, err := runNomad(ctx, "job", "run", tmpDir+"/"+file.Path)
		if err != nil {
			return out, err
		}
		placement := app.PreferNode
		if placement == "" {
			placement = "oracle-main"
		}
		state.SetApp(app.Name, placement, "deployed")
		if err := s.store.SaveState(state); err != nil {
			return out, err
		}
		return out, nil
	case "app-render":
		if name == "" {
			return "", errors.New("missing app name")
		}
		cfg, _, err := s.store.Load()
		if err != nil {
			return "", err
		}
		file, err := pool.RenderAppJob(cfg, name)
		if err != nil {
			return "", err
		}
		if err := pool.WriteRendered("/tmp/poolctl-agent-rendered", []pool.RenderedFile{file}); err != nil {
			return "", err
		}
		return "rendered /tmp/poolctl-agent-rendered/" + file.Path + "\n", nil
	case "node-freeze", "node-unfreeze", "node-drain":
		if name == "" {
			return "", errors.New("missing node name")
		}
		cfg, state, err := s.store.Load()
		if err != nil {
			return "", err
		}
		if !cfg.HasNode(name) {
			return "", fmt.Errorf("unknown node %q", name)
		}
		switch action {
		case "node-freeze":
			state.SetFrozen(name, true)
		case "node-unfreeze":
			state.SetFrozen(name, false)
		case "node-drain":
			state.SetDraining(name, true)
		}
		if err := s.store.SaveState(state); err != nil {
			return "", err
		}
		return fmt.Sprintf("%s applied to %s\n", action, name), nil
	default:
		return "", fmt.Errorf("unknown action %q", action)
	}
}

func (s *server) authorized(w http.ResponseWriter, r *http.Request) bool {
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return false
	}
	got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if subtle.ConstantTimeCompare([]byte(got), []byte(s.token)) != 1 {
		writeJSON(w, http.StatusUnauthorized, response{OK: false, Error: "unauthorized"})
		return false
	}
	return true
}

func readSystemState(ctx context.Context) systemState {
	return systemState{
		Nomad:     firstLine(mustRun(ctx, "systemctl", "is-active", "nomad")),
		Traefik:   firstLine(mustRun(ctx, "systemctl", "is-active", "traefik")),
		WireGuard: firstLine(mustRun(ctx, "systemctl", "is-active", "wg-quick@wg0")),
	}
}

func runSmokes(ctx context.Context, cfg pool.Config) []check {
	var checks []check
	for _, app := range cfg.Apps {
		if app.Domain == "" {
			continue
		}
		checks = append(checks, smoke(ctx, "HTTP", "http://"+app.Domain+"/"))
		checks = append(checks, smoke(ctx, "HTTPS", "https://"+app.Domain+"/"))
	}
	return checks
}

func smoke(ctx context.Context, label, url string) check {
	started := time.Now()
	ctx, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return check{Label: label, URL: url, Status: err.Error(), LatencyMS: time.Since(started).Milliseconds()}
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return check{Label: label, URL: url, Status: err.Error(), LatencyMS: time.Since(started).Milliseconds()}
	}
	defer resp.Body.Close()
	return check{Label: label, URL: url, OK: resp.StatusCode >= 200 && resp.StatusCode < 400, Status: resp.Status, LatencyMS: time.Since(started).Milliseconds()}
}

func runNomad(ctx context.Context, args ...string) (string, error) {
	token := firstExistingToken()
	fullArgs := append([]string{"env", "NOMAD_ADDR=https://10.44.0.1:4646", "NOMAD_CACERT=/etc/nomad.d/tls/nomad-agent-ca.pem", "NOMAD_TOKEN=" + token, "nomad"}, args...)
	return run(ctx, "sudo", fullArgs...)
}

func firstExistingToken() string {
	for _, path := range []string{"/var/lib/poolctl/nomad-acl/bootstrap.token", "/etc/nomad.d/acl/bootstrap.token", "/etc/nomad.d/bootstrap.token"} {
		data, err := os.ReadFile(path)
		if err == nil && strings.TrimSpace(string(data)) != "" {
			return strings.Fields(string(data))[0]
		}
	}
	return ""
}

func mustRun(ctx context.Context, name string, args ...string) string {
	out, _ := run(ctx, name, args...)
	return out
}

func run(ctx context.Context, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	if ctx.Err() == context.DeadlineExceeded {
		return out.String(), errors.New("command timed out")
	}
	return out.String(), err
}

func firstLine(s string) string {
	fields := strings.Split(strings.TrimSpace(s), "\n")
	if len(fields) == 0 || fields[0] == "" {
		return "unknown"
	}
	return fields[0]
}

func writeJSON(w http.ResponseWriter, status int, body response) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func withHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin == "https://myprod-control.vercel.app" || origin == "http://localhost:3000" || origin == "http://127.0.0.1:3000" {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Vary", "Origin")
		}
		w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		next.ServeHTTP(w, r)
	})
}

func isLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return host == "localhost" || host == "" || (ip != nil && ip.IsLoopback())
}
