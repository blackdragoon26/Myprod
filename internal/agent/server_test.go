package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blackdragoon26/Myprod/internal/pool"
)

func TestRegisterAppEndpointPersistsWithoutDeploying(t *testing.T) {
	store := testStore(t)
	calledNomad := false
	s := &server{store: store, token: "test-token", runNomad: func(context.Context, ...string) (string, error) {
		calledNomad = true
		return "", nil
	}}
	body := `{"Name":"example-api","Image":"ghcr.io/example/api:abc123","Domain":"example.example.com","Port":8080,"PreferNode":"do-worker-1","CPU":700,"MemoryMB":768,"HealthPath":"/healthz"}`
	req := httptest.NewRequest(http.MethodPost, "/__poolctl/api/apps", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-token")
	recorder := httptest.NewRecorder()
	s.handleApps(recorder, req)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if calledNomad {
		t.Fatal("registration must not submit a Nomad job")
	}
	cfg, state, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	app, ok := cfg.FindApp("example-api")
	if !ok || app.PreferNode != "do-worker-1" || app.AllowWorkers {
		t.Fatalf("registered app = %#v", app)
	}
	if state.Apps["example-api"].Status != "configured" {
		t.Fatalf("app state = %#v", state.Apps["example-api"])
	}
}

func TestReadNodeResourcesUsesLiveClientStats(t *testing.T) {
	stats := `{
		"CPU":[{"Total":20},{"Total":40}],
		"Memory":{"Available":400,"Total":1000,"Used":600},
		"DiskStats":[{"Mountpoint":"/","Available":700,"Size":1000,"Used":300,"UsedPercent":30}],
		"Uptime":3600
	}`
	runner := func(_ context.Context, args ...string) (string, error) {
		switch strings.Join(args, " ") {
		case "node status -json":
			return nodeListJSON(t), nil
		case "operator api /v1/client/stats?node_id=control-id", "operator api /v1/client/stats?node_id=worker-id":
			return stats, nil
		default:
			return "", nil
		}
	}
	s := &server{runNomad: runner}
	resources := s.readNodeResources(context.Background())
	if len(resources) != 2 {
		t.Fatalf("resources = %#v", resources)
	}
	if got := resources[0]; got.CPUUsedPercent != 30 || got.MemoryUsedPercent != 60 || got.DiskUsedPercent != 30 || got.UptimeSeconds != 3600 {
		t.Fatalf("resource sample = %#v", got)
	}
}

func TestLoopback(t *testing.T) {
	if !isLoopback("127.0.0.1:8790") {
		t.Fatal("expected localhost agent bind to be loopback")
	}
	if isLoopback("0.0.0.0:8790") {
		t.Fatal("public agent bind must not be treated as loopback")
	}
}

func TestFirstLine(t *testing.T) {
	if got := firstLine("active\ninactive\n"); got != "active" {
		t.Fatalf("firstLine = %q, want active", got)
	}
	if got := firstLine(""); got != "unknown" {
		t.Fatalf("empty firstLine = %q, want unknown", got)
	}
}

func TestReserveReleaseAndUnfreezeNode(t *testing.T) {
	store := testStore(t)
	var calls [][]string
	runner := func(_ context.Context, args ...string) (string, error) {
		calls = append(calls, append([]string(nil), args...))
		if strings.Join(args, " ") == "node status -json" {
			return nodeListJSON(t), nil
		}
		if strings.Join(args, " ") == "node status -json worker-id" {
			return `{"ID":"worker-id","Name":"do-worker-1","Allocations":[]}`, nil
		}
		return "Nomad accepted command.\n", nil
	}
	s := &server{store: store, runNomad: runner}

	out, err := s.runAction(context.Background(), "node-reserve", "do-worker-1", "project-alpha")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Reserved do-worker-1 exclusively for project project-alpha") {
		t.Fatalf("unexpected reserve output %q", out)
	}
	_, state, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if got := state.Nodes["do-worker-1"]; got.ReservedFor != "project-alpha" || !got.Frozen {
		t.Fatalf("reservation state = %#v", got)
	}
	if !hasCall(calls, "node eligibility -disable worker-id") {
		t.Fatalf("reserve did not disable Nomad eligibility: %#v", calls)
	}
	if _, err := s.runAction(context.Background(), "node-drain", "do-worker-1", ""); err == nil || !strings.Contains(err.Error(), "release the reservation") {
		t.Fatalf("reserved node drain error = %v", err)
	}

	if _, err := s.runAction(context.Background(), "node-unfreeze", "do-worker-1", ""); err == nil || !strings.Contains(err.Error(), "release the reservation") {
		t.Fatalf("reserved node unfreeze error = %v", err)
	}
	if _, err := s.runAction(context.Background(), "node-release", "do-worker-1", ""); err != nil {
		t.Fatal(err)
	}
	_, state, _ = store.Load()
	if got := state.Nodes["do-worker-1"]; got.ReservedFor != "" || !got.Frozen {
		t.Fatalf("released node should remain frozen: %#v", got)
	}
	if _, err := s.runAction(context.Background(), "node-unfreeze", "do-worker-1", ""); err != nil {
		t.Fatal(err)
	}
	if !hasCall(calls, "node eligibility -enable worker-id") {
		t.Fatalf("unfreeze did not enable Nomad eligibility: %#v", calls)
	}
}

func TestReserveRejectsActiveAllocations(t *testing.T) {
	store := testStore(t)
	var eligibilityChanged bool
	runner := func(_ context.Context, args ...string) (string, error) {
		switch strings.Join(args, " ") {
		case "node status -json":
			return nodeListJSON(t), nil
		case "node status -json worker-id":
			return `{"ID":"worker-id","Name":"do-worker-1","Allocations":[{"ID":"alloc-12345678","JobID":"busy-api","ClientStatus":"running","DesiredStatus":"run"}]}`, nil
		default:
			eligibilityChanged = true
			return "", nil
		}
	}
	s := &server{store: store, runNomad: runner}
	_, err := s.runAction(context.Background(), "node-reserve", "do-worker-1", "project-alpha")
	if err == nil || !strings.Contains(err.Error(), "active allocations") {
		t.Fatalf("reserve error = %v", err)
	}
	if eligibilityChanged {
		t.Fatal("reservation must not change eligibility while allocations are active")
	}
}

func TestFreezeAndDrainUseNomad(t *testing.T) {
	store := testStore(t)
	var calls [][]string
	runner := func(_ context.Context, args ...string) (string, error) {
		calls = append(calls, append([]string(nil), args...))
		if strings.Join(args, " ") == "node status -json" {
			return nodeListJSON(t), nil
		}
		return "ok\n", nil
	}
	s := &server{store: store, runNomad: runner}
	if _, err := s.runAction(context.Background(), "node-freeze", "do-worker-1", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.runAction(context.Background(), "node-drain", "do-worker-1", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.runAction(context.Background(), "node-cancel-drain", "do-worker-1", ""); err != nil {
		t.Fatal(err)
	}
	if !hasCall(calls, "node eligibility -disable worker-id") {
		t.Fatal("freeze did not call Nomad eligibility")
	}
	if !hasCall(calls, "node drain -enable -detach -yes -m poolctl web drain worker-id") {
		t.Fatal("drain did not call Nomad drain")
	}
	if !hasCall(calls, "node drain -disable -keep-ineligible -yes -m poolctl web drain cancelled worker-id") {
		t.Fatal("cancel drain did not call Nomad drain")
	}
}

func TestDeployRefusesFrozenTarget(t *testing.T) {
	store := testStore(t)
	_, state, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	state.SetFrozen("oracle-main", true)
	if err := store.SaveState(state); err != nil {
		t.Fatal(err)
	}
	s := &server{store: store, runNomad: func(context.Context, ...string) (string, error) {
		t.Fatal("Nomad must not be called for a frozen target")
		return "", nil
	}}
	_, err = s.runAction(context.Background(), "app-deploy", "sample-api", "")
	if err == nil || !strings.Contains(err.Error(), "target node oracle-main is unavailable") {
		t.Fatalf("deploy error = %v", err)
	}
}

func TestDeployVerifiesHealthyAllocationBeforeSavingState(t *testing.T) {
	store := testStore(t)
	var calls [][]string
	runner := func(_ context.Context, args ...string) (string, error) {
		calls = append(calls, append([]string(nil), args...))
		if strings.Join(args, " ") == "job status -json sample-api" {
			return `[{"Allocations":[{
				"ID":"alloc-1","JobID":"sample-api","NodeName":"oracle-main",
				"ClientStatus":"running","DesiredStatus":"run",
				"DeploymentStatus":{"Healthy":true}
			}]}]`, nil
		}
		return "Nomad accepted command.\n", nil
	}
	s := &server{store: store, runNomad: runner}
	out, err := s.runAction(context.Background(), "app-deploy", "sample-api", "")
	if err != nil {
		t.Fatal(err)
	}
	if !hasCall(calls, "job status -json sample-api") {
		t.Fatalf("deploy did not verify JSON job status: %#v", calls)
	}
	if !strings.Contains(out, "Verified Nomad job status") {
		t.Fatalf("unexpected deploy output %q", out)
	}
	_, state, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if got := state.Apps["sample-api"]; got.Status != "deployed" || got.Node != "oracle-main" {
		t.Fatalf("deployment state = %#v", got)
	}
}

func TestDeploymentVerifiedRejectsWrongNode(t *testing.T) {
	raw := []byte(`[{"Allocations":[{
		"JobID":"sample-api","NodeName":"do-worker-1",
		"ClientStatus":"running","DesiredStatus":"run",
		"DeploymentStatus":{"Healthy":true}
	}]}]`)
	ok, reason, err := deploymentVerified(raw, "sample-api", "oracle-main")
	if err != nil {
		t.Fatal(err)
	}
	if ok || !strings.Contains(reason, "none are healthy and running on oracle-main") {
		t.Fatalf("verified=%t reason=%q", ok, reason)
	}
}

func TestValidProjectID(t *testing.T) {
	for _, valid := range []string{"splidt", "project-alpha", "team_2"} {
		if !validProjectID(valid) {
			t.Fatalf("expected %q to be valid", valid)
		}
	}
	for _, invalid := range []string{"", "two projects", "../escape", "project/one"} {
		if validProjectID(invalid) {
			t.Fatalf("expected %q to be invalid", invalid)
		}
	}
}

func testStore(t *testing.T) pool.Store {
	t.Helper()
	store := pool.NewStore(filepath.Join(t.TempDir(), ".poolctl"))
	if _, err := store.Init(); err != nil {
		t.Fatal(err)
	}
	if err := store.AddNode(pool.Node{
		Name: "do-worker-1", Role: "worker", Provider: "digitalocean",
		PublicIP: "203.0.113.10", SSHUser: "ubuntu", SSHKey: "worker.key", OverlayIP: "10.44.0.2",
	}); err != nil {
		t.Fatal(err)
	}
	return store
}

func nodeListJSON(t *testing.T) string {
	t.Helper()
	raw, err := json.Marshal([]nomadNode{
		{ID: "control-id", Name: "oracle-main", Status: "ready", SchedulingEligibility: "eligible"},
		{ID: "worker-id", Name: "do-worker-1", Status: "ready", SchedulingEligibility: "eligible"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func hasCall(calls [][]string, want string) bool {
	for _, call := range calls {
		if strings.Join(call, " ") == want {
			return true
		}
	}
	return false
}
