package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/blackdragoon26/Myprod/internal/pool"
)

type sandboxHarness struct {
	server *server
	store  pool.Store
	mu     sync.Mutex
	calls  [][]string
	memory float64
	disk   float64
	failed bool
}

func (h *sandboxHarness) nomadCalls() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	joined := make([]string, 0, len(h.calls))
	for _, call := range h.calls {
		joined = append(joined, strings.Join(call, " "))
	}
	return joined
}

func (h *sandboxHarness) called(prefix string) bool {
	for _, call := range h.nomadCalls() {
		if strings.HasPrefix(call, prefix) {
			return true
		}
	}
	return false
}

func newSandboxHarness(t *testing.T) *sandboxHarness {
	t.Helper()
	harness := &sandboxHarness{store: testStore(t), memory: 30, disk: 10}
	runner := func(_ context.Context, args ...string) (string, error) {
		harness.mu.Lock()
		harness.calls = append(harness.calls, append([]string(nil), args...))
		failed := harness.failed
		memory := harness.memory
		disk := harness.disk
		harness.mu.Unlock()

		joined := strings.Join(args, " ")
		switch {
		case strings.HasPrefix(joined, "job run"):
			return "Evaluation ID: eval-sbx\n", nil
		case strings.HasPrefix(joined, "job status -json"):
			clientStatus := "running"
			if failed {
				clientStatus = "failed"
			}
			return fmt.Sprintf(`[{"Allocations":[{"ID":"alloc-1","EvalID":"eval-sbx","JobID":%q,"NodeName":"do-worker-1","ClientStatus":%q,"DesiredStatus":"run"}]}]`,
				args[len(args)-1], clientStatus), nil
		case strings.HasPrefix(joined, "job status"):
			return "sandbox job status\n", nil
		case strings.HasPrefix(joined, "job stop"):
			return "purged\n", nil
		case strings.HasPrefix(joined, "node status -json"):
			return nodeListJSON(t), nil
		case strings.HasPrefix(joined, "operator api /v1/client/stats"):
			return fmt.Sprintf(`{"CPU":[{"Total":11.5}],"Memory":{"Total":1000,"Used":%d,"Available":%d},"DiskStats":[{"Mountpoint":"/","Size":1000,"Used":%d,"Available":%d,"UsedPercent":%.1f}],"Uptime":900}`,
				int(memory*10), int((100-memory)*10), int(disk*10), int((100-disk)*10), disk), nil
		case strings.HasPrefix(joined, "alloc exec"):
			return "aarch64\n", nil
		case strings.HasPrefix(joined, "alloc logs"):
			return "sandbox log line\n", nil
		case strings.HasPrefix(joined, "node eligibility"), strings.HasPrefix(joined, "node drain"):
			return "ok\n", nil
		}
		return "", nil
	}
	harness.server = &server{
		store: harness.store, token: "test-token", runNomad: runner, sandbox: defaultSandboxPolicy(),
	}
	harness.server.sandbox.StartAttempts = 1
	return harness
}

func (h *sandboxHarness) enroll(t *testing.T, options pool.SandboxHost) {
	t.Helper()
	options.Node = "do-worker-1"
	if _, err := h.store.EnrollSandboxHost(options); err != nil {
		t.Fatal(err)
	}
}

func (h *sandboxHarness) create(t *testing.T, body string) (*http.Response, response) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/__poolctl/api/sandboxes", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-token")
	recorder := httptest.NewRecorder()
	h.server.handleSandboxes(recorder, req)
	var decoded response
	if err := json.Unmarshal(recorder.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("decode response: %v (%s)", err, recorder.Body.String())
	}
	return recorder.Result(), decoded
}

func sandboxRequestJSON(node string) string {
	return fmt.Sprintf(`{"name":"llm-box","node":%q,"profile":"strict","network":"none","cpu":500,"memoryMb":512,"ttlSeconds":900}`, node)
}

func TestCreateSandboxRequiresOperatorCredential(t *testing.T) {
	harness := newSandboxHarness(t)
	harness.enroll(t, pool.SandboxHost{})

	for _, credential := range []string{"", "poolctl_sbx_v1.someone.else", "wrong-token"} {
		req := httptest.NewRequest(http.MethodPost, "/__poolctl/api/sandboxes", strings.NewReader(sandboxRequestJSON("do-worker-1")))
		if credential != "" {
			req.Header.Set("Authorization", "Bearer "+credential)
		}
		recorder := httptest.NewRecorder()
		harness.server.handleSandboxes(recorder, req)
		if recorder.Code != http.StatusUnauthorized {
			t.Fatalf("credential %q returned %d, want 401", credential, recorder.Code)
		}
	}
	if harness.called("job run") {
		t.Fatal("an unauthorized request must not submit a Nomad job")
	}
}

func TestCreateSandboxLaunchesHardenedJobAndIssuesScopedToken(t *testing.T) {
	harness := newSandboxHarness(t)
	harness.enroll(t, pool.SandboxHost{})

	result, decoded := harness.create(t, sandboxRequestJSON("do-worker-1"))
	if result.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, body = %+v", result.StatusCode, decoded)
	}
	if decoded.IssuedSandbox == nil || decoded.IssuedSandbox.Token == "" {
		t.Fatalf("expected a one-time sandbox token, got %+v", decoded)
	}
	sandbox := decoded.IssuedSandbox.Sandbox
	if sandbox.Status != pool.SandboxStatusRunning || sandbox.Node != "do-worker-1" {
		t.Fatalf("unexpected sandbox %+v", sandbox)
	}
	if sandbox.JobID != pool.SandboxJobID(sandbox.ID) {
		t.Fatalf("sandbox job id %q is not derived from the sandbox id", sandbox.JobID)
	}
	if !harness.called("job run -detach") {
		t.Fatalf("expected the sandbox job to be submitted, calls = %v", harness.nomadCalls())
	}
	for _, call := range harness.nomadCalls() {
		if strings.HasPrefix(call, "job run") && !strings.HasSuffix(call, pool.SandboxJobID(sandbox.ID)+".nomad.hcl") {
			t.Fatalf("sandbox submitted an unexpected job file: %s", call)
		}
	}
	if result.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("the issued token response must not be cached: %q", result.Header.Get("Cache-Control"))
	}
}

func TestSandboxTokenIsScopedToItsOwnSandbox(t *testing.T) {
	harness := newSandboxHarness(t)
	harness.enroll(t, pool.SandboxHost{MaxSandboxes: 4, MaxCPU: 4000, MaxMemoryMB: 4096})

	_, first := harness.create(t, sandboxRequestJSON("do-worker-1"))
	_, second := harness.create(t, sandboxRequestJSON("do-worker-1"))
	if first.IssuedSandbox == nil || second.IssuedSandbox == nil {
		t.Fatalf("expected two sandboxes, got %+v %+v", first, second)
	}
	token := first.IssuedSandbox.Token
	ownID := first.IssuedSandbox.Sandbox.ID
	otherID := second.IssuedSandbox.Sandbox.ID

	get := func(path string) int {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		recorder := httptest.NewRecorder()
		harness.server.handleSandbox(recorder, req)
		return recorder.Code
	}
	if code := get("/__poolctl/api/sandboxes/" + ownID); code != http.StatusOK {
		t.Fatalf("a sandbox token must read its own sandbox, got %d", code)
	}
	if code := get("/__poolctl/api/sandboxes/" + otherID); code != http.StatusUnauthorized {
		t.Fatalf("a sandbox token must not read another sandbox, got %d", code)
	}

	// The scoped credential must be useless on every operator surface.
	operatorSurfaces := []struct {
		name    string
		request *http.Request
		handler func(http.ResponseWriter, *http.Request)
	}{
		{"list sandboxes", httptest.NewRequest(http.MethodGet, "/__poolctl/api/sandboxes", nil), harness.server.handleSandboxes},
		{"create sandbox", httptest.NewRequest(http.MethodPost, "/__poolctl/api/sandboxes", strings.NewReader(sandboxRequestJSON("do-worker-1"))), harness.server.handleSandboxes},
		{"pool status", httptest.NewRequest(http.MethodGet, "/__poolctl/api/status", nil), harness.server.handleStatus},
		{"node action", httptest.NewRequest(http.MethodPost, "/__poolctl/api/action", strings.NewReader(`{"action":"node-drain","name":"do-worker-1"}`)), harness.server.handleAction},
		{"register app", httptest.NewRequest(http.MethodPost, "/__poolctl/api/apps", strings.NewReader(`{"Name":"x","Image":"ghcr.io/x/y:1","Domain":"x.example.com","Port":80,"PreferNode":"do-worker-1"}`)), harness.server.handleApps},
		{"extend own sandbox", httptest.NewRequest(http.MethodPost, "/__poolctl/api/sandboxes/"+ownID+"/extend", strings.NewReader(`{"seconds":600}`)), harness.server.handleSandbox},
	}
	for _, surface := range operatorSurfaces {
		surface.request.Header.Set("Authorization", "Bearer "+token)
		recorder := httptest.NewRecorder()
		surface.handler(recorder, surface.request)
		if recorder.Code != http.StatusUnauthorized {
			t.Fatalf("%s accepted a sandbox token (status %d)", surface.name, recorder.Code)
		}
	}
}

func TestSandboxExecRunsThroughNomadWithoutAHostShell(t *testing.T) {
	harness := newSandboxHarness(t)
	harness.enroll(t, pool.SandboxHost{})
	_, created := harness.create(t, sandboxRequestJSON("do-worker-1"))
	sandbox := created.IssuedSandbox.Sandbox

	req := httptest.NewRequest(http.MethodPost, "/__poolctl/api/sandboxes/"+sandbox.ID+"/exec",
		strings.NewReader(`{"command":"uname -m; echo \"done\""}`))
	req.Header.Set("Authorization", "Bearer "+created.IssuedSandbox.Token)
	recorder := httptest.NewRecorder()
	harness.server.handleSandbox(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("exec status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var decoded response
	if err := json.Unmarshal(recorder.Body.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Exec == nil || decoded.Exec.ExitCode != 0 || !strings.Contains(decoded.Exec.Output, "aarch64") {
		t.Fatalf("unexpected exec result %+v", decoded.Exec)
	}

	want := fmt.Sprintf("alloc exec -i=false -t=false -task sandbox -job %s /bin/sh -c uname -m; echo \"done\"", sandbox.JobID)
	if !harness.called("alloc exec") {
		t.Fatalf("exec did not reach Nomad, calls = %v", harness.nomadCalls())
	}
	for _, call := range harness.nomadCalls() {
		if strings.HasPrefix(call, "alloc exec") && call != want {
			t.Fatalf("exec argv = %q, want %q", call, want)
		}
	}
}

func TestSandboxExecArgvRefusesUnsafeShapes(t *testing.T) {
	if _, err := sandboxExecArgv("uname -m", []string{"uname"}); err == nil {
		t.Fatal("expected command and argv to be mutually exclusive")
	}
	if _, err := sandboxExecArgv("", nil); err == nil {
		t.Fatal("expected an empty request to be refused")
	}
	if _, err := sandboxExecArgv("", []string{"-address=http://evil"}); err == nil {
		t.Fatal("argv must not be able to inject a Nomad CLI flag")
	}
	if _, err := sandboxExecArgv("echo \x00", nil); err == nil {
		t.Fatal("expected NUL to be refused")
	}
	if _, err := sandboxExecArgv(strings.Repeat("a", 9<<10), nil); err == nil {
		t.Fatal("expected an oversized command to be refused")
	}
	argv, err := sandboxExecArgv("apt-get update", nil)
	if err != nil || len(argv) != 3 || argv[0] != "/bin/sh" || argv[1] != "-c" || argv[2] != "apt-get update" {
		t.Fatalf("unexpected argv %v (%v)", argv, err)
	}
}

func TestDestroySandboxRefusesJobsOutsideTheSandboxPrefix(t *testing.T) {
	harness := newSandboxHarness(t)
	forged := pool.Sandbox{ID: "abc123", JobID: "p4lens-api", Status: pool.SandboxStatusRunning, Node: "do-worker-1"}
	if _, err := harness.server.destroySandbox(context.Background(), forged, pool.SandboxStatusDestroyed, ""); err == nil {
		t.Fatal("expected a non-sandbox job to be refused")
	}
	if harness.called("job stop") {
		t.Fatalf("a managed app job must never be stopped by the sandbox surface, calls = %v", harness.nomadCalls())
	}
}

func TestExpiredSandboxIsReclaimedAndStopsAuthorizing(t *testing.T) {
	harness := newSandboxHarness(t)
	harness.enroll(t, pool.SandboxHost{})
	_, created := harness.create(t, sandboxRequestJSON("do-worker-1"))
	sandbox := created.IssuedSandbox.Sandbox

	harness.server.now = func() time.Time { return sandbox.ExpiresAt.Add(time.Minute) }
	reaped := harness.server.reapExpiredSandboxes(context.Background())
	if len(reaped) != 1 || reaped[0] != sandbox.ID {
		t.Fatalf("expected the expired sandbox to be reclaimed, got %v", reaped)
	}
	if !harness.called("job stop -purge " + sandbox.JobID) {
		t.Fatalf("expected the sandbox job to be purged, calls = %v", harness.nomadCalls())
	}
	stored, found, err := harness.store.FindSandbox(sandbox.ID)
	if err != nil || !found || stored.Status != pool.SandboxStatusExpired {
		t.Fatalf("sandbox state = %+v (found %t, err %v)", stored, found, err)
	}
	if ok, _ := harness.store.AuthorizeSandboxToken(sandbox.ID, created.IssuedSandbox.Token); ok {
		t.Fatal("an expired sandbox token must stop authorizing")
	}

	// Budget must be released, so a replacement sandbox fits immediately.
	result, replacement := harness.create(t, sandboxRequestJSON("do-worker-1"))
	if result.StatusCode != http.StatusCreated {
		t.Fatalf("replacement sandbox status = %d, body = %+v", result.StatusCode, replacement)
	}
}

func TestCreateSandboxRefusesNodeUnderMemoryPressure(t *testing.T) {
	harness := newSandboxHarness(t)
	harness.enroll(t, pool.SandboxHost{})
	harness.mu.Lock()
	harness.memory = 93
	harness.mu.Unlock()

	result, decoded := harness.create(t, sandboxRequestJSON("do-worker-1"))
	if result.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, body = %+v", result.StatusCode, decoded)
	}
	if !strings.Contains(decoded.Error, "memory") {
		t.Fatalf("unexpected error %q", decoded.Error)
	}
	if harness.called("job run") {
		t.Fatal("a node above the memory ceiling must not receive a sandbox job")
	}
}

func TestSandboxThatFailsToStartReleasesItsBudget(t *testing.T) {
	harness := newSandboxHarness(t)
	harness.enroll(t, pool.SandboxHost{MaxSandboxes: 1, MaxCPU: 500, MaxMemoryMB: 512})
	harness.mu.Lock()
	harness.failed = true
	harness.mu.Unlock()

	result, decoded := harness.create(t, sandboxRequestJSON("do-worker-1"))
	if result.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, body = %+v", result.StatusCode, decoded)
	}
	sandboxes, err := harness.store.ListSandboxes()
	if err != nil || len(sandboxes) != 1 || sandboxes[0].Status != pool.SandboxStatusFailed {
		t.Fatalf("expected one failed sandbox record, got %+v (%v)", sandboxes, err)
	}

	harness.mu.Lock()
	harness.failed = false
	harness.mu.Unlock()
	retry, retried := harness.create(t, sandboxRequestJSON("do-worker-1"))
	if retry.StatusCode != http.StatusCreated {
		t.Fatalf("a failed sandbox must not keep holding budget: status = %d, body = %+v", retry.StatusCode, retried)
	}
}

func TestSandboxHostEnrollmentIsOperatorScopedAndRefusesControlPlane(t *testing.T) {
	harness := newSandboxHarness(t)
	if _, err := harness.server.runAction(context.Background(), "sandbox-host-enroll", "oracle-main", "egress"); err == nil {
		t.Fatal("expected control-plane enrollment to be refused")
	}
	output, err := harness.server.runAction(context.Background(), "sandbox-host-enroll", "do-worker-1", "egress,max=3,cpu=1500,mem=2048")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output, "at most 3 sandboxes, 1500 MHz, 2048 MB") {
		t.Fatalf("unexpected enrollment output %q", output)
	}
	host, found, err := harness.store.FindSandboxHost("do-worker-1")
	if err != nil || !found || !host.EgressAllowed || host.MaxSandboxes != 3 {
		t.Fatalf("unexpected host record %+v (found %t, err %v)", host, found, err)
	}
	if _, err := harness.server.runAction(context.Background(), "sandbox-host-enroll", "do-worker-1", "bogus=1"); err == nil {
		t.Fatal("expected an unknown enrollment option to be refused")
	}
	if _, err := harness.server.runAction(context.Background(), "sandbox-host-remove", "do-worker-1", ""); err != nil {
		t.Fatal(err)
	}
	if _, found, _ := harness.store.FindSandboxHost("do-worker-1"); found {
		t.Fatal("expected the host record to be removed")
	}
}

func TestSandboxHostRemovalRefusesWhileSandboxesAreLive(t *testing.T) {
	harness := newSandboxHarness(t)
	harness.enroll(t, pool.SandboxHost{})
	harness.create(t, sandboxRequestJSON("do-worker-1"))
	if _, err := harness.server.runAction(context.Background(), "sandbox-host-remove", "do-worker-1", ""); err == nil {
		t.Fatal("expected removal to be refused while a sandbox is live")
	}
}

func TestNodeDrainReclaimsSandboxesFirst(t *testing.T) {
	harness := newSandboxHarness(t)
	harness.enroll(t, pool.SandboxHost{})
	_, created := harness.create(t, sandboxRequestJSON("do-worker-1"))
	sandbox := created.IssuedSandbox.Sandbox

	output, err := harness.server.runAction(context.Background(), "node-drain", "do-worker-1", "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output, sandbox.ID) {
		t.Fatalf("drain output should report the reclaimed sandbox: %s", output)
	}
	if !harness.called("job stop -purge " + sandbox.JobID) {
		t.Fatalf("expected the sandbox to be purged before draining, calls = %v", harness.nomadCalls())
	}
	stored, _, _ := harness.store.FindSandbox(sandbox.ID)
	if stored.Status != pool.SandboxStatusDestroyed {
		t.Fatalf("sandbox status = %q", stored.Status)
	}
}

func TestStatusAdvertisesSandboxCapability(t *testing.T) {
	raw, err := json.Marshal(response{OK: true, Capabilities: agentCapabilities{SandboxPartitionsV1: true}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"sandboxPartitionsV1":true`) {
		t.Fatalf("status response = %s", raw)
	}
}

func TestParseSandboxHostValueDefaultsToIsolated(t *testing.T) {
	host, err := parseSandboxHostValue("do-worker-1", "")
	if err != nil {
		t.Fatal(err)
	}
	if host.EgressAllowed {
		t.Fatal("sandbox hosts must default to no egress")
	}
	if host, err = parseSandboxHostValue("do-worker-1", "isolated,max=1"); err != nil || host.EgressAllowed || host.MaxSandboxes != 1 {
		t.Fatalf("unexpected host %+v (%v)", host, err)
	}
}

func TestSandboxesAreReclaimedWhenTheNodeRunsOutOfDisk(t *testing.T) {
	harness := newSandboxHarness(t)
	harness.enroll(t, pool.SandboxHost{})
	_, created := harness.create(t, sandboxRequestJSON("do-worker-1"))
	sandbox := created.IssuedSandbox.Sandbox

	if culled := harness.server.enforceSandboxNodePressure(context.Background()); len(culled) != 0 {
		t.Fatalf("a healthy node must not lose its sandboxes, got %v", culled)
	}

	harness.mu.Lock()
	harness.disk = 95
	harness.mu.Unlock()
	culled := harness.server.enforceSandboxNodePressure(context.Background())
	if len(culled) != 1 || culled[0] != sandbox.ID {
		t.Fatalf("expected the sandbox to be reclaimed under disk pressure, got %v", culled)
	}
	stored, _, _ := harness.store.FindSandbox(sandbox.ID)
	if stored.Status != pool.SandboxStatusDestroyed || !strings.Contains(stored.Note, "node pressure") {
		t.Fatalf("unexpected sandbox record %+v", stored)
	}
}
