package agent

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/blackdragoon26/Myprod/internal/pool"
)

// Sandbox partitions give an LLM a disposable Ubuntu ARM64 box on a worker.
//
// Everything in this file is written around one rule: a sandbox may consume
// only the slice of one enrolled worker that an operator budgeted for it, and
// may not observe or change anything else in the pool. Creation, enrollment,
// and lifetime extension require full operator authentication. The credential
// handed to the sandbox user authorizes exec, logs, status, and destroy for
// exactly one sandbox ID and nothing else.

const (
	sandboxNodeMemoryCeiling = 85.0
	sandboxNodeDiskCeiling   = 85.0
	sandboxExecMaxOutput     = 64 << 10
	sandboxExecDefaultTimeut = 60 * time.Second
	sandboxExecMaxTimeout    = 120 * time.Second
	sandboxReapInterval      = 30 * time.Second
	sandboxStartAttempts     = 30
	sandboxCullDiskPercent   = 92.0
	sandboxCullMemoryPercent = 96.0
)

type sandboxPolicy struct {
	MemoryCeiling  float64
	DiskCeiling    float64
	CullMemory     float64
	CullDisk       float64
	ExecTimeout    time.Duration
	ExecMaxTimeout time.Duration
	ExecMaxOutput  int
	ReapInterval   time.Duration
	StartAttempts  int
}

func defaultSandboxPolicy() sandboxPolicy {
	return sandboxPolicy{
		MemoryCeiling:  sandboxNodeMemoryCeiling,
		DiskCeiling:    sandboxNodeDiskCeiling,
		CullMemory:     sandboxCullMemoryPercent,
		CullDisk:       sandboxCullDiskPercent,
		ExecTimeout:    sandboxExecDefaultTimeut,
		ExecMaxTimeout: sandboxExecMaxTimeout,
		ExecMaxOutput:  sandboxExecMaxOutput,
		ReapInterval:   sandboxReapInterval,
		StartAttempts:  sandboxStartAttempts,
	}
}

// clock exists so sandbox expiry can be exercised deterministically in tests.
func (s *server) clock() time.Time {
	if s.now == nil {
		return time.Now().UTC()
	}
	return s.now().UTC()
}

func (s *server) policy() sandboxPolicy {
	if s.sandbox.ExecTimeout == 0 {
		return defaultSandboxPolicy()
	}
	return s.sandbox
}

type issuedSandbox struct {
	pool.Sandbox
	Token string `json:"token"`
	Usage string `json:"usage"`
}

type sandboxExecResult struct {
	SandboxID  string `json:"sandboxId"`
	ExitCode   int    `json:"exitCode"`
	Output     string `json:"output"`
	Truncated  bool   `json:"truncated"`
	TimedOut   bool   `json:"timedOut"`
	DurationMS int64  `json:"durationMs"`
}

type sandboxRequest struct {
	Name       string `json:"name"`
	Node       string `json:"node"`
	Image      string `json:"image"`
	Profile    string `json:"profile"`
	Network    string `json:"network"`
	CPU        int    `json:"cpu"`
	MemoryMB   int    `json:"memoryMb"`
	DiskMB     int    `json:"diskMb"`
	PIDsLimit  int    `json:"pidsLimit"`
	TTLSeconds int    `json:"ttlSeconds"`
	Note       string `json:"note"`
}

// handleSandboxes serves the collection endpoint. Both methods require full
// operator authentication: a sandbox credential can neither enumerate the
// pool's sandboxes nor create another one.
func (s *server) handleSandboxes(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(w, r) {
		return
	}
	switch r.Method {
	case http.MethodGet:
		s.reapExpiredSandboxes(r.Context())
		sandboxes, err := s.store.ListSandboxes()
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, response{OK: false, Error: err.Error()})
			return
		}
		hosts, err := s.store.ListSandboxHosts()
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, response{OK: false, Error: err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, response{
			OK: true, Sandboxes: sandboxes, SandboxHosts: hosts,
			Updated: time.Now().UTC().Format(time.RFC3339),
		})
	case http.MethodPost:
		s.createSandbox(w, r)
	default:
		writeJSON(w, http.StatusMethodNotAllowed, response{OK: false, Error: "method not allowed"})
	}
}

// handleSandbox serves one sandbox. The sandbox's own scoped token is accepted
// for status, exec, logs, and destroy; everything else stays operator-only.
func (s *server) handleSandbox(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/__poolctl/api/sandboxes/")
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		writeJSON(w, http.StatusNotFound, response{OK: false, Error: "missing sandbox id"})
		return
	}
	id := parts[0]
	action := ""
	if len(parts) > 1 {
		action = parts[1]
	}
	if len(parts) > 2 {
		writeJSON(w, http.StatusNotFound, response{OK: false, Error: "unknown sandbox endpoint"})
		return
	}

	// Extension is a capacity decision, so it never accepts a sandbox token.
	if action == "extend" {
		if !s.authorized(w, r) {
			return
		}
	} else if !s.authorizedSandbox(w, r, id) {
		return
	}

	sandbox, found, err := s.store.FindSandbox(id)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, response{OK: false, Error: err.Error()})
		return
	}
	if !found {
		writeJSON(w, http.StatusNotFound, response{OK: false, Error: fmt.Sprintf("unknown sandbox %q", id)})
		return
	}

	switch {
	case action == "" && r.Method == http.MethodGet:
		writeJSON(w, http.StatusOK, response{OK: true, Sandbox: &sandbox, Output: s.sandboxLiveStatus(r.Context(), sandbox), Updated: time.Now().UTC().Format(time.RFC3339)})
	case action == "" && r.Method == http.MethodDelete:
		output, destroyErr := s.destroySandbox(r.Context(), sandbox, pool.SandboxStatusDestroyed, "destroyed by request")
		if destroyErr != nil {
			writeJSON(w, http.StatusInternalServerError, response{OK: false, Error: destroyErr.Error(), Output: output})
			return
		}
		writeJSON(w, http.StatusOK, response{OK: true, Output: output, Updated: time.Now().UTC().Format(time.RFC3339)})
	case action == "exec" && r.Method == http.MethodPost:
		s.execInSandbox(w, r, sandbox)
	case action == "logs" && r.Method == http.MethodGet:
		out, logErr := s.nomad(r.Context(), "alloc", "logs", "-job", "-task", pool.SandboxTaskName, "-tail", "-n=200", sandbox.JobID)
		if logErr != nil {
			writeJSON(w, http.StatusInternalServerError, response{OK: false, Error: logErr.Error(), Output: limitSandboxOutput(out, s.policy().ExecMaxOutput)})
			return
		}
		writeJSON(w, http.StatusOK, response{OK: true, Output: limitSandboxOutput(out, s.policy().ExecMaxOutput), Sandbox: &sandbox, Updated: time.Now().UTC().Format(time.RFC3339)})
	case action == "extend" && r.Method == http.MethodPost:
		s.extendSandbox(w, r, sandbox)
	default:
		writeJSON(w, http.StatusMethodNotAllowed, response{OK: false, Error: "method not allowed"})
	}
}

func (s *server) createSandbox(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	var req sandboxRequest
	if err := decoder.Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, response{OK: false, Error: "invalid sandbox request: " + err.Error()})
		return
	}

	// Reap first so an expired sandbox never blocks a new one on budget.
	s.reapExpiredSandboxes(r.Context())

	preflight, err := s.sandboxNodePreflight(r.Context(), req.Node)
	if err != nil {
		writeJSON(w, http.StatusConflict, response{OK: false, Error: err.Error()})
		return
	}

	token, sandbox, err := s.store.CreateSandbox(pool.Sandbox{
		Name: req.Name, Node: req.Node, Image: req.Image, Profile: req.Profile, Network: req.Network,
		CPU: req.CPU, MemoryMB: req.MemoryMB, DiskMB: req.DiskMB, PIDsLimit: req.PIDsLimit,
		TTLSeconds: req.TTLSeconds, Note: req.Note,
	})
	if err != nil {
		writeJSON(w, http.StatusBadRequest, response{OK: false, Error: err.Error()})
		return
	}

	output, startErr := s.startSandbox(r.Context(), sandbox)
	if startErr != nil {
		// A sandbox that did not start must not keep holding budget, and its
		// scoped token must stop authorizing anything.
		failed, _ := s.destroySandbox(r.Context(), sandbox, pool.SandboxStatusFailed, "sandbox failed to start")
		writeJSON(w, http.StatusInternalServerError, response{
			OK: false, Error: startErr.Error(), Output: output + "\n" + failed,
		})
		return
	}
	running, err := s.store.UpdateSandbox(sandbox.ID, pool.SandboxStatusRunning, preflight)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, response{OK: false, Error: err.Error(), Output: output})
		return
	}

	writeJSON(w, http.StatusCreated, response{
		OK: true,
		IssuedSandbox: &issuedSandbox{
			Sandbox: running,
			Token:   token,
			Usage:   sandboxUsage(running),
		},
		Output:  output,
		Updated: time.Now().UTC().Format(time.RFC3339),
	})
}

func (s *server) extendSandbox(w http.ResponseWriter, r *http.Request, sandbox pool.Sandbox) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<10)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	var req struct {
		Seconds int `json:"seconds"`
	}
	if err := decoder.Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, response{OK: false, Error: "invalid extension: " + err.Error()})
		return
	}
	if req.Seconds < pool.SandboxMinTTL || req.Seconds > pool.SandboxMaxTTL {
		writeJSON(w, http.StatusBadRequest, response{OK: false, Error: fmt.Sprintf("extension must be between %d and %d seconds", pool.SandboxMinTTL, pool.SandboxMaxTTL)})
		return
	}
	extended, err := s.store.ExtendSandbox(sandbox.ID, time.Duration(req.Seconds)*time.Second)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, response{OK: false, Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, response{
		OK: true, Sandbox: &extended,
		Output:  fmt.Sprintf("Sandbox %s now expires at %s. The container's own timer was not changed; it stops at its original deadline unless recreated.\n", extended.ID, extended.ExpiresAt.Format(time.RFC3339)),
		Updated: time.Now().UTC().Format(time.RFC3339),
	})
}

func (s *server) execInSandbox(w http.ResponseWriter, r *http.Request, sandbox pool.Sandbox) {
	if !sandbox.Active() {
		writeJSON(w, http.StatusConflict, response{OK: false, Error: fmt.Sprintf("sandbox %s is %s", sandbox.ID, sandbox.Status)})
		return
	}
	if sandbox.Expired(s.clock()) {
		s.reapExpiredSandboxes(r.Context())
		writeJSON(w, http.StatusConflict, response{OK: false, Error: fmt.Sprintf("sandbox %s expired at %s", sandbox.ID, sandbox.ExpiresAt.Format(time.RFC3339))})
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 32<<10)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	var req struct {
		Command        string   `json:"command"`
		Argv           []string `json:"argv"`
		TimeoutSeconds int      `json:"timeoutSeconds"`
	}
	if err := decoder.Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, response{OK: false, Error: "invalid exec request: " + err.Error()})
		return
	}
	argv, err := sandboxExecArgv(req.Command, req.Argv)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, response{OK: false, Error: err.Error()})
		return
	}
	policy := s.policy()
	timeout := policy.ExecTimeout
	if req.TimeoutSeconds > 0 {
		timeout = time.Duration(req.TimeoutSeconds) * time.Second
		if timeout > policy.ExecMaxTimeout {
			writeJSON(w, http.StatusBadRequest, response{OK: false, Error: fmt.Sprintf("timeout must be at most %d seconds", int(policy.ExecMaxTimeout.Seconds()))})
			return
		}
	}

	started := time.Now()
	execCtx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()
	args := append([]string{"alloc", "exec", "-i=false", "-t=false", "-task", pool.SandboxTaskName, "-job", sandbox.JobID}, argv...)
	out, execErr := s.nomad(execCtx, args...)
	truncated := len(out) > policy.ExecMaxOutput
	result := sandboxExecResult{
		SandboxID:  sandbox.ID,
		ExitCode:   sandboxExitCode(execErr),
		Output:     limitSandboxOutput(out, policy.ExecMaxOutput),
		Truncated:  truncated,
		TimedOut:   errors.Is(execCtx.Err(), context.DeadlineExceeded),
		DurationMS: time.Since(started).Milliseconds(),
	}
	// A non-zero exit code inside the sandbox is a normal result, not an agent
	// failure. Only an unusable exec channel is reported as an error.
	if result.ExitCode < 0 && !result.TimedOut {
		writeJSON(w, http.StatusInternalServerError, response{OK: false, Error: execError(execErr), Exec: &result, Output: result.Output})
		return
	}
	writeJSON(w, http.StatusOK, response{OK: true, Exec: &result, Output: result.Output, Updated: time.Now().UTC().Format(time.RFC3339)})
}

// startSandbox renders and submits the hardened job, then waits for a running
// allocation on the expected node.
//
// It deliberately does not take the application deploy mutex. Sandbox job
// names are unique, each render goes to its own temporary directory, and the
// budget check is already serialized inside the store, so a sandbox launch
// must never be able to delay a managed application deployment.
func (s *server) startSandbox(ctx context.Context, sandbox pool.Sandbox) (string, error) {
	cfg, _, err := s.store.Load()
	if err != nil {
		return "", err
	}
	file, err := pool.RenderSandboxJob(cfg, sandbox)
	if err != nil {
		return "", err
	}
	tmpDir, err := os.MkdirTemp("", "poolctl-agent-sandbox-*")
	if err != nil {
		return "", fmt.Errorf("create sandbox render directory: %w", err)
	}
	defer os.RemoveAll(tmpDir)
	if err := pool.WriteRendered(tmpDir, []pool.RenderedFile{file}); err != nil {
		return "", err
	}
	out, err := s.nomad(ctx, "job", "run", "-detach", tmpDir+"/"+file.Path)
	if err != nil {
		return out, err
	}
	evalID, err := submittedEvaluationID(out)
	if err != nil {
		return out, err
	}
	statusOut, err := s.waitForSandbox(ctx, sandbox, evalID)
	if err != nil {
		return out + "\nSandbox was submitted, but did not start:\n" + statusOut, err
	}
	return out + "\nSandbox allocation is running:\n" + statusOut, nil
}

func (s *server) waitForSandbox(ctx context.Context, sandbox pool.Sandbox, evalID string) (string, error) {
	attempts := s.policy().StartAttempts
	var lastOutput, lastReason string
	for attempt := 0; attempt < attempts; attempt++ {
		out, err := s.nomad(ctx, "job", "status", "-json", sandbox.JobID)
		lastOutput = out
		if err != nil {
			lastReason = fmt.Sprintf("read sandbox status: %v", err)
		} else {
			running, reason, parseErr := sandboxAllocationRunning([]byte(out), sandbox.JobID, sandbox.Node, evalID)
			if parseErr != nil {
				return out, parseErr
			}
			if running {
				return out, nil
			}
			lastReason = reason
		}
		if attempt < attempts-1 {
			select {
			case <-ctx.Done():
				return lastOutput, ctx.Err()
			case <-time.After(2 * time.Second):
			}
		}
	}
	return lastOutput, fmt.Errorf("sandbox %s did not start on %s: %s", sandbox.ID, sandbox.Node, lastReason)
}

func sandboxAllocationRunning(raw []byte, jobID, expectedNode, evalID string) (bool, string, error) {
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
			if allocation.JobID != jobID || allocation.EvalID != evalID {
				continue
			}
			seen++
			if allocation.NodeName != expectedNode {
				return false, fmt.Sprintf("allocation was placed on %s instead of %s", allocation.NodeName, expectedNode), nil
			}
			if allocation.ClientStatus == "failed" {
				return false, "allocation failed", nil
			}
			if allocation.DesiredStatus == "run" && allocation.ClientStatus == "running" {
				return true, "", nil
			}
		}
	}
	if seen == 0 {
		return false, fmt.Sprintf("no allocations were reported for evaluation %s", evalID), nil
	}
	return false, fmt.Sprintf("%d allocation(s) exist but none are running on %s", seen, expectedNode), nil
}

// destroySandbox purges the sandbox job and closes out its record. The prefix
// guard is what keeps this endpoint from being able to stop a managed app.
func (s *server) destroySandbox(ctx context.Context, sandbox pool.Sandbox, status, note string) (string, error) {
	if !pool.IsSandboxJobID(sandbox.JobID) {
		return "", fmt.Errorf("refusing to stop %q: not a sandbox job", sandbox.JobID)
	}
	output := ""
	if sandbox.Active() {
		out, err := s.nomad(ctx, "job", "stop", "-purge", sandbox.JobID)
		output = out
		if err != nil && !sandboxJobAlreadyGone(out) {
			return output, fmt.Errorf("stop sandbox job: %w", err)
		}
	}
	if _, err := s.store.UpdateSandbox(sandbox.ID, status, note); err != nil {
		return output, err
	}
	return output + fmt.Sprintf("\nSandbox %s is %s. No managed application or node state was changed.\n", sandbox.ID, status), nil
}

func sandboxJobAlreadyGone(output string) bool {
	lowered := strings.ToLower(output)
	return strings.Contains(lowered, "no job") || strings.Contains(lowered, "not found")
}

// reapExpiredSandboxes stops sandboxes that outlived their TTL. It is called
// on a timer and opportunistically before capacity decisions.
func (s *server) reapExpiredSandboxes(ctx context.Context) []string {
	expired, err := s.store.ExpiredSandboxes(s.clock())
	if err != nil || len(expired) == 0 {
		return nil
	}
	var reaped []string
	for _, sandbox := range expired {
		if _, err := s.destroySandbox(ctx, sandbox, pool.SandboxStatusExpired, "expired and reclaimed automatically"); err != nil {
			continue
		}
		reaped = append(reaped, sandbox.ID)
	}
	return reaped
}

// reclaimNodeSandboxes destroys every live sandbox on one node. It is used
// before a drain so sandbox capacity accounting cannot outlive the workload.
func (s *server) reclaimNodeSandboxes(ctx context.Context, node string) string {
	active, err := s.store.ActiveSandboxes()
	if err != nil || len(active) == 0 {
		return ""
	}
	var reclaimed []string
	for _, sandbox := range active {
		if sandbox.Node != node {
			continue
		}
		if _, err := s.destroySandbox(ctx, sandbox, pool.SandboxStatusDestroyed, "reclaimed before node drain"); err != nil {
			continue
		}
		reclaimed = append(reclaimed, sandbox.ID)
	}
	if len(reclaimed) == 0 {
		return ""
	}
	return fmt.Sprintf("Reclaimed sandbox partitions on %s before draining: %s\n", node, strings.Join(reclaimed, ", "))
}

// enforceSandboxNodePressure reclaims every sandbox on a node that has crossed
// a hard pressure threshold.
//
// Nomad's ephemeral_disk value is a scheduling reservation, not an enforced
// limit, so a sandbox writing outside its size-capped tmpfs work areas could
// still fill a worker's root disk. This is the backstop: sandboxes are
// disposable, managed applications are not, so the sandboxes go first and
// nothing else on the node is touched.
func (s *server) enforceSandboxNodePressure(ctx context.Context) []string {
	active, err := s.store.ActiveSandboxes()
	if err != nil || len(active) == 0 {
		return nil
	}
	hosting := make(map[string]bool, len(active))
	for _, sandbox := range active {
		hosting[sandbox.Node] = true
	}
	policy := s.policy()
	pressured := make(map[string]string)
	for _, resource := range s.readNodeResources(ctx) {
		if !hosting[resource.Name] || resource.Error != "" {
			continue
		}
		if resource.DiskUsedPercent >= policy.CullDisk {
			pressured[resource.Name] = fmt.Sprintf("node root disk reached %.1f%%", resource.DiskUsedPercent)
		} else if resource.MemoryUsedPercent >= policy.CullMemory {
			pressured[resource.Name] = fmt.Sprintf("node memory reached %.1f%%", resource.MemoryUsedPercent)
		}
	}
	if len(pressured) == 0 {
		return nil
	}
	var culled []string
	for _, sandbox := range active {
		reason, ok := pressured[sandbox.Node]
		if !ok {
			continue
		}
		if _, err := s.destroySandbox(ctx, sandbox, pool.SandboxStatusDestroyed, "reclaimed under node pressure: "+reason); err != nil {
			continue
		}
		culled = append(culled, sandbox.ID)
	}
	return culled
}

func (s *server) runSandboxReaper(ctx context.Context) {
	interval := s.policy().ReapInterval
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.reapExpiredSandboxes(ctx)
			s.enforceSandboxNodePressure(ctx)
		}
	}
}

// sandboxNodePreflight refuses to add sandbox load to a node that is already
// under memory or disk pressure. Telemetry gaps are reported, not fatal: the
// pool treats a client-stats failure as isolated to that node.
func (s *server) sandboxNodePreflight(ctx context.Context, node string) (string, error) {
	if node == "" {
		return "", errors.New("sandbox node is required")
	}
	policy := s.policy()
	for _, resource := range s.readNodeResources(ctx) {
		if resource.Name != node {
			continue
		}
		if resource.Error != "" {
			return "node telemetry unavailable; capacity was not verified before launch", nil
		}
		if resource.MemoryUsedPercent > policy.MemoryCeiling {
			return "", fmt.Errorf("node %s memory is %.1f%% used, above the %.0f%% sandbox ceiling", node, resource.MemoryUsedPercent, policy.MemoryCeiling)
		}
		if resource.DiskUsedPercent > policy.DiskCeiling {
			return "", fmt.Errorf("node %s root disk is %.1f%% used, above the %.0f%% sandbox ceiling", node, resource.DiskUsedPercent, policy.DiskCeiling)
		}
		return fmt.Sprintf("node %s was %.1f%% memory and %.1f%% disk used at launch", node, resource.MemoryUsedPercent, resource.DiskUsedPercent), nil
	}
	return "node telemetry unavailable; capacity was not verified before launch", nil
}

func (s *server) sandboxLiveStatus(ctx context.Context, sandbox pool.Sandbox) string {
	if !sandbox.Active() {
		return fmt.Sprintf("Sandbox %s is %s.\n", sandbox.ID, sandbox.Status)
	}
	out, err := s.nomad(ctx, "job", "status", sandbox.JobID)
	if err != nil {
		return fmt.Sprintf("Sandbox %s status could not be read: %v\n%s", sandbox.ID, err, limitDiagnostic(out))
	}
	return limitDiagnostic(out)
}

// authorizedSandbox accepts the operator credential or the scoped session
// token of this one sandbox. A token minted for another sandbox is rejected.
func (s *server) authorizedSandbox(w http.ResponseWriter, r *http.Request, id string) bool {
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return false
	}
	got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if s.token != "" && subtle.ConstantTimeCompare([]byte(got), []byte(s.token)) == 1 {
		return true
	}
	if s.operatorAuth != nil && s.operatorAuth.Authorize(r.Context(), got) {
		return true
	}
	authorized, err := s.store.AuthorizeSandboxToken(id, got)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, response{OK: false, Error: "sandbox authorization failed"})
		return false
	}
	if authorized {
		return true
	}
	writeJSON(w, http.StatusUnauthorized, response{OK: false, Error: "unauthorized"})
	return false
}

// runSandboxHostAction enrolls or withdraws a node as a sandbox host. It is
// reached from the operator-authenticated action endpoint only.
func (s *server) runSandboxHostAction(action, node, value string) (string, error) {
	if node == "" {
		return "", errors.New("missing node name")
	}
	switch action {
	case "sandbox-host-enroll":
		host, err := parseSandboxHostValue(node, value)
		if err != nil {
			return "", err
		}
		saved, err := s.store.EnrollSandboxHost(host)
		if err != nil {
			return "", err
		}
		output := fmt.Sprintf("Enrolled %s as a sandbox host: at most %d sandboxes, %d MHz, %d MB.\n",
			saved.Node, saved.MaxSandboxes, saved.MaxCPU, saved.MaxMemoryMB)
		if saved.EgressAllowed {
			output += "Network egress is permitted. This assumes the sandbox isolation bundle is installed on that node; verify with sandbox-isolation.sh --verify.\n"
		} else {
			output += "Network egress is not permitted. Sandboxes on this node get loopback only.\n"
		}
		return output, nil
	case "sandbox-host-remove":
		if err := s.store.RemoveSandboxHost(node); err != nil {
			return "", err
		}
		return fmt.Sprintf("Removed %s from the sandbox host list. Existing applications and node state were not changed.\n", node), nil
	}
	return "", fmt.Errorf("unknown sandbox action %q", action)
}

// parseSandboxHostValue reads "egress,max=2,cpu=1000,mem=1024" style options.
func parseSandboxHostValue(node, value string) (pool.SandboxHost, error) {
	host := pool.SandboxHost{Node: node}
	for _, field := range strings.Split(value, ",") {
		field = strings.TrimSpace(field)
		if field == "" {
			continue
		}
		key, raw, hasValue := strings.Cut(field, "=")
		key = strings.TrimSpace(key)
		raw = strings.TrimSpace(raw)
		if !hasValue {
			switch key {
			case "egress":
				host.EgressAllowed = true
				continue
			case "isolated", "none":
				host.EgressAllowed = false
				continue
			default:
				return pool.SandboxHost{}, fmt.Errorf("unknown sandbox host option %q", key)
			}
		}
		number, err := strconv.Atoi(raw)
		if err != nil {
			return pool.SandboxHost{}, fmt.Errorf("sandbox host option %q must be a number", key)
		}
		switch key {
		case "max":
			host.MaxSandboxes = number
		case "cpu":
			host.MaxCPU = number
		case "mem", "memory":
			host.MaxMemoryMB = number
		case "egress":
			host.EgressAllowed = number != 0
		default:
			return pool.SandboxHost{}, fmt.Errorf("unknown sandbox host option %q", key)
		}
	}
	return host, nil
}

// sandboxExecArgv turns a request into a host-safe argument vector. The host
// never runs a shell: argv is passed directly to the Nomad CLI, so quoting in
// the requested command cannot escape into the agent's own process.
func sandboxExecArgv(command string, argv []string) ([]string, error) {
	command = strings.TrimSpace(command)
	if command != "" && len(argv) > 0 {
		return nil, errors.New("send either command or argv, not both")
	}
	if command != "" {
		if len(command) > 8<<10 {
			return nil, errors.New("command must be 8 KiB or smaller")
		}
		if strings.ContainsRune(command, '\x00') {
			return nil, errors.New("command must not contain NUL")
		}
		return []string{"/bin/sh", "-c", command}, nil
	}
	if len(argv) == 0 {
		return nil, errors.New("command or argv is required")
	}
	if len(argv) > 64 {
		return nil, errors.New("argv may contain at most 64 elements")
	}
	for _, item := range argv {
		if item == "" {
			return nil, errors.New("argv elements must not be empty")
		}
		if len(item) > 4<<10 {
			return nil, errors.New("argv elements must be 4 KiB or smaller")
		}
		if strings.ContainsRune(item, '\x00') {
			return nil, errors.New("argv must not contain NUL")
		}
	}
	if strings.HasPrefix(argv[0], "-") {
		return nil, errors.New("argv[0] must be a program path, not a flag")
	}
	return argv, nil
}

func sandboxExitCode(err error) int {
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return -1
}

func execError(err error) string {
	if err == nil {
		return "sandbox exec failed"
	}
	return err.Error()
}

func limitSandboxOutput(raw string, max int) string {
	if max <= 0 {
		max = sandboxExecMaxOutput
	}
	if len(raw) <= max {
		return raw
	}
	return raw[:max] + "\n... sandbox output truncated ..."
}

func sandboxUsage(sandbox pool.Sandbox) string {
	return fmt.Sprintf(`Sandbox %s is scoped to itself. The token below authorizes exec, logs, status, and destroy for this sandbox only.

  curl -sS -X POST https://api.sankalpjha.dev/__poolctl/api/sandboxes/%s/exec \
    -H "Authorization: Bearer $SANDBOX_TOKEN" \
    -H "Content-Type: application/json" \
    -d '{"command":"uname -m && cat /etc/os-release"}'

It expires at %s and is reclaimed automatically.`, sandbox.ID, sandbox.ID, sandbox.ExpiresAt.Format(time.RFC3339))
}
