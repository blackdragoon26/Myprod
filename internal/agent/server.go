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
	addr     string
	store    pool.Store
	token    string
	runNomad func(context.Context, ...string) (string, error)
}

type nomadNode struct {
	ID                    string `json:"ID"`
	Name                  string `json:"Name"`
	Status                string `json:"Status"`
	SchedulingEligibility string `json:"SchedulingEligibility"`
	Drain                 bool   `json:"Drain"`
	Allocations           []struct {
		ID            string `json:"ID"`
		JobID         string `json:"JobID"`
		ClientStatus  string `json:"ClientStatus"`
		DesiredStatus string `json:"DesiredStatus"`
	} `json:"Allocations"`
}

type nomadJobStatus struct {
	Allocations []struct {
		ID               string `json:"ID"`
		JobID            string `json:"JobID"`
		NodeName         string `json:"NodeName"`
		ClientStatus     string `json:"ClientStatus"`
		DesiredStatus    string `json:"DesiredStatus"`
		DeploymentStatus *struct {
			Healthy bool `json:"Healthy"`
		} `json:"DeploymentStatus"`
	} `json:"Allocations"`
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
	s := &server{addr: addr, store: pool.NewStore(storeDir), token: token, runNomad: runNomad}

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
	if reconciled, reconcileErr := s.reconcileNodeState(r.Context(), cfg, state); reconcileErr == nil {
		state = reconciled
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
		Value  string `json:"value"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, response{OK: false, Error: err.Error()})
		return
	}
	output, err := s.runAction(r.Context(), req.Action, req.Name, req.Value)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, response{OK: false, Error: err.Error(), Output: output})
		return
	}
	writeJSON(w, http.StatusOK, response{OK: true, Output: output, Updated: time.Now().UTC().Format(time.RFC3339)})
}

func (s *server) runAction(ctx context.Context, action, name, value string) (string, error) {
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
		placement := app.PreferNode
		if placement == "" {
			placement = "oracle-main"
		}
		if nodeState := state.Nodes[placement]; nodeState.Frozen || nodeState.Draining || nodeState.ReservedFor != "" {
			return "", fmt.Errorf("target node %s is unavailable: frozen=%t draining=%t reserved_for=%q", placement, nodeState.Frozen, nodeState.Draining, nodeState.ReservedFor)
		}
		file, err := pool.RenderAppJob(cfg, name)
		if err != nil {
			return "", err
		}
		tmpDir := "/tmp/poolctl-agent-rendered"
		if err := pool.WriteRendered(tmpDir, []pool.RenderedFile{file}); err != nil {
			return "", err
		}
		out, err := s.nomad(ctx, "job", "run", tmpDir+"/"+file.Path)
		if err != nil {
			return out, err
		}
		statusOut, statusErr := s.verifyJobDeployment(ctx, name, placement)
		if statusErr != nil {
			return out + "\nDeployment was submitted, but verification failed:\n" + statusOut, statusErr
		}
		state.SetApp(app.Name, placement, "deployed")
		if err := s.store.SaveState(state); err != nil {
			return out, err
		}
		return out + "\nVerified Nomad job status:\n" + statusOut, nil
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
	case "node-freeze", "node-unfreeze", "node-drain", "node-cancel-drain", "node-reserve", "node-release":
		if name == "" {
			return "", errors.New("missing node name")
		}
		cfg, state, err := s.store.Load()
		if err != nil {
			return "", err
		}
		node, ok := cfg.FindNode(name)
		if !ok {
			return "", fmt.Errorf("unknown node %q", name)
		}
		liveNode, err := s.findNomadNode(ctx, name)
		if err != nil {
			return "", err
		}
		var output string
		switch action {
		case "node-freeze":
			output, err = s.nomad(ctx, "node", "eligibility", "-disable", liveNode.ID)
			if err != nil {
				return output, err
			}
			state.SetFrozen(name, true)
		case "node-unfreeze":
			current := state.Nodes[name]
			if current.ReservedFor != "" {
				return "", fmt.Errorf("node is reserved for %q; release the reservation before unfreezing", current.ReservedFor)
			}
			if current.Draining || liveNode.Drain {
				return "", errors.New("node is draining; cancel the drain before unfreezing")
			}
			output, err = s.nomad(ctx, "node", "eligibility", "-enable", liveNode.ID)
			if err != nil {
				return output, err
			}
			state.SetFrozen(name, false)
		case "node-drain":
			if current := state.Nodes[name]; current.ReservedFor != "" {
				return "", fmt.Errorf("node is reserved for %q; release the reservation before draining", current.ReservedFor)
			}
			output, err = s.nomad(ctx, "node", "drain", "-enable", "-detach", "-yes", "-m", "poolctl web drain", liveNode.ID)
			if err != nil {
				return output, err
			}
			state.SetDraining(name, true)
		case "node-cancel-drain":
			output, err = s.nomad(ctx, "node", "drain", "-disable", "-keep-ineligible", "-yes", "-m", "poolctl web drain cancelled", liveNode.ID)
			if err != nil {
				return output, err
			}
			state.SetDraining(name, false)
			state.SetFrozen(name, true)
		case "node-reserve":
			project := strings.TrimSpace(value)
			if !validProjectID(project) {
				return "", errors.New("project id must contain only letters, numbers, dash, or underscore")
			}
			if node.Role == "control-plane" {
				return "", errors.New("control-plane nodes cannot be reserved for projects")
			}
			current := state.Nodes[name]
			if current.ReservedFor != "" && current.ReservedFor != project {
				return "", fmt.Errorf("node is already reserved for %q", current.ReservedFor)
			}
			allocations, allocErr := s.activeAllocations(ctx, liveNode.ID)
			if allocErr != nil {
				return "", allocErr
			}
			if len(allocations) != 0 {
				return "", fmt.Errorf("node has active allocations (%s); drain it before reserving", strings.Join(allocations, ", "))
			}
			output, err = s.nomad(ctx, "node", "eligibility", "-disable", liveNode.ID)
			if err != nil {
				return output, err
			}
			state.SetReserved(name, project)
			output += fmt.Sprintf("\nReserved %s exclusively for project %s.\n", name, project)
		case "node-release":
			current := state.Nodes[name]
			if current.ReservedFor == "" {
				return "", errors.New("node has no project reservation")
			}
			project := current.ReservedFor
			state.SetReserved(name, "")
			state.SetFrozen(name, true)
			output = fmt.Sprintf("Released project %s from %s. The node remains ineligible until Unfreeze is explicitly confirmed.\n", project, name)
		}
		if err := s.store.SaveState(state); err != nil {
			return "", err
		}
		return output + fmt.Sprintf("%s applied to %s\n", action, name), nil
	default:
		return "", fmt.Errorf("unknown action %q", action)
	}
}

func (s *server) nomad(ctx context.Context, args ...string) (string, error) {
	if s.runNomad == nil {
		return runNomad(ctx, args...)
	}
	return s.runNomad(ctx, args...)
}

func (s *server) verifyJobDeployment(ctx context.Context, jobName, expectedNode string) (string, error) {
	var lastOutput string
	var lastReason string
	for attempt := 0; attempt < 15; attempt++ {
		out, err := s.nomad(ctx, "job", "status", "-json", jobName)
		lastOutput = out
		if err != nil {
			lastReason = fmt.Sprintf("read job status: %v", err)
		} else {
			verified, reason, parseErr := deploymentVerified([]byte(out), jobName, expectedNode)
			if parseErr != nil {
				return out, parseErr
			}
			if verified {
				return out, nil
			}
			lastReason = reason
		}
		if attempt < 14 {
			select {
			case <-ctx.Done():
				return lastOutput, ctx.Err()
			case <-time.After(2 * time.Second):
			}
		}
	}
	return lastOutput, fmt.Errorf("job %s did not become healthy on %s within 30 seconds: %s", jobName, expectedNode, lastReason)
}

func deploymentVerified(raw []byte, jobName, expectedNode string) (bool, string, error) {
	var statuses []nomadJobStatus
	if err := json.Unmarshal(raw, &statuses); err != nil {
		var status nomadJobStatus
		if objectErr := json.Unmarshal(raw, &status); objectErr != nil {
			return false, "", fmt.Errorf("parse Nomad job status: %w", err)
		}
		statuses = []nomadJobStatus{status}
	}
	seen := 0
	for _, status := range statuses {
		for _, allocation := range status.Allocations {
			if allocation.JobID != jobName {
				continue
			}
			seen++
			if allocation.NodeName == expectedNode && allocation.DesiredStatus == "run" && allocation.ClientStatus == "running" && allocation.DeploymentStatus != nil && allocation.DeploymentStatus.Healthy {
				return true, "", nil
			}
		}
	}
	if seen == 0 {
		return false, "no allocations were reported", nil
	}
	return false, fmt.Sprintf("%d allocation(s) exist but none are healthy and running on %s", seen, expectedNode), nil
}

func (s *server) listNomadNodes(ctx context.Context) ([]nomadNode, error) {
	out, err := s.nomad(ctx, "node", "status", "-json")
	if err != nil {
		return nil, fmt.Errorf("list Nomad nodes: %w: %s", err, strings.TrimSpace(out))
	}
	var nodes []nomadNode
	if err := json.Unmarshal([]byte(out), &nodes); err != nil {
		return nil, fmt.Errorf("parse Nomad node list: %w", err)
	}
	return nodes, nil
}

func (s *server) findNomadNode(ctx context.Context, name string) (nomadNode, error) {
	nodes, err := s.listNomadNodes(ctx)
	if err != nil {
		return nomadNode{}, err
	}
	for _, node := range nodes {
		if node.Name == name {
			return node, nil
		}
	}
	return nomadNode{}, fmt.Errorf("Nomad node %q is not registered", name)
}

func (s *server) activeAllocations(ctx context.Context, nodeID string) ([]string, error) {
	out, err := s.nomad(ctx, "node", "status", "-json", nodeID)
	if err != nil {
		return nil, fmt.Errorf("read Nomad node allocations: %w: %s", err, strings.TrimSpace(out))
	}
	var node nomadNode
	if err := json.Unmarshal([]byte(out), &node); err != nil {
		return nil, fmt.Errorf("parse Nomad node allocations: %w", err)
	}
	var active []string
	for _, allocation := range node.Allocations {
		if allocation.DesiredStatus == "run" && allocation.ClientStatus != "complete" && allocation.ClientStatus != "failed" {
			active = append(active, allocation.JobID+"/"+allocation.ID[:min(8, len(allocation.ID))])
		}
	}
	return active, nil
}

func (s *server) reconcileNodeState(ctx context.Context, cfg pool.Config, state pool.State) (pool.State, error) {
	nodes, err := s.listNomadNodes(ctx)
	if err != nil {
		return state, err
	}
	byName := make(map[string]nomadNode, len(nodes))
	for _, node := range nodes {
		byName[node.Name] = node
	}
	changed := false
	for _, configured := range cfg.Nodes {
		live, ok := byName[configured.Name]
		if !ok {
			continue
		}
		current := state.Nodes[configured.Name]
		frozen := live.SchedulingEligibility != "eligible"
		if current.Frozen != frozen || current.Draining != live.Drain {
			current.Frozen = frozen
			current.Draining = live.Drain
			state.Nodes[configured.Name] = current
			changed = true
		}
	}
	if changed {
		if err := s.store.SaveState(state); err != nil {
			return state, err
		}
	}
	return state, nil
}

func validProjectID(raw string) bool {
	if raw == "" {
		return false
	}
	for _, r := range raw {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			continue
		}
		return false
	}
	return true
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
	if token == "" {
		return "", errors.New("Nomad ACL token is missing")
	}
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
