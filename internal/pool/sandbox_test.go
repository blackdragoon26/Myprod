package pool

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func sandboxTestStore(t *testing.T) Store {
	t.Helper()
	store := NewStore(filepath.Join(t.TempDir(), ".poolctl"))
	if _, err := store.Init(); err != nil {
		t.Fatal(err)
	}
	if err := store.AddNode(Node{
		Name: "oracle-worker-1", Role: "worker", Provider: "oracle",
		PublicIP: "203.0.113.20", SSHUser: "ubuntu", SSHKey: "worker.key", OverlayIP: "10.44.0.3",
	}); err != nil {
		t.Fatal(err)
	}
	return store
}

func sandboxTestConfig() Config {
	return Config{Nodes: []Node{
		{Name: "oracle-main", Role: "control-plane", OverlayIP: "10.44.0.1"},
		{Name: "oracle-worker-1", Role: "worker", OverlayIP: "10.44.0.3"},
	}}
}

func TestRenderSandboxJobIsHardenedAndUnroutable(t *testing.T) {
	cfg := sandboxTestConfig()
	sandbox := Sandbox{
		ID: "a1b2c3d4e5f6", Name: "llm-box", Node: "oracle-worker-1",
		CreatedAt: time.Now().UTC(), TTLSeconds: 900,
	}
	file, err := RenderSandboxJob(cfg, sandbox)
	if err != nil {
		t.Fatal(err)
	}
	content := file.Content

	for _, required := range []string{
		`job "poolctl-sbx-a1b2c3d4e5f6"`,
		`type        = "batch"`,
		`attribute = "$${node.unique.name}"`,
		`value     = "oracle-worker-1"`,
		`attribute = "$${attr.cpu.arch}"`,
		`value     = "arm64"`,
		`network_mode    = "none"`,
		`privileged      = false`,
		`readonly_rootfs = true`,
		`cap_drop        = ["ALL"]`,
		`security_opt    = ["no-new-privileges"]`,
		`pids_limit      = 512`,
		`args            = ["14400"]`,
		`poolctl_hard_stop_s = "14400"`,
		"identity {",
		"env  = false",
	} {
		if !strings.Contains(content, required) {
			t.Fatalf("rendered sandbox job is missing %q:\n%s", required, content)
		}
	}

	// A sandbox must never be routable, never join the pool overlay, never
	// mount a host path, and never gain capabilities in the strict profile.
	for _, forbidden := range []string{
		"traefik",
		"service {",
		"host_network",
		"volumes",
		"volume_mount",
		"cap_add",
		"/var/run/docker.sock",
		"wireguard",
	} {
		if strings.Contains(content, forbidden) {
			t.Fatalf("rendered sandbox job must not contain %q:\n%s", forbidden, content)
		}
	}
	if file.Path != filepath.Join("nomad", "jobs", "poolctl-sbx-a1b2c3d4e5f6.nomad.hcl") {
		t.Fatalf("unexpected render path %q", file.Path)
	}
}

func TestRenderSandboxWorkspaceProfileKeepsDangerousCapabilitiesDropped(t *testing.T) {
	file, err := RenderSandboxJob(sandboxTestConfig(), Sandbox{
		ID: "0011223344ff", Node: "oracle-worker-1",
		Profile: SandboxProfileWorkspace, Network: SandboxNetworkEgress, CreatedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(file.Content, `network_mode    = "poolctl-sandbox"`) {
		t.Fatalf("egress sandbox must use the isolated docker network:\n%s", file.Content)
	}
	if !strings.Contains(file.Content, `readonly_rootfs = false`) {
		t.Fatalf("workspace profile needs a writable container filesystem:\n%s", file.Content)
	}
	if !strings.Contains(file.Content, `dns_servers     = ["1.1.1.1", "9.9.9.9"]`) {
		t.Fatalf("egress sandbox must use public resolvers:\n%s", file.Content)
	}
	for _, forbidden := range []string{"sys_admin", "net_admin", "net_raw", "sys_ptrace", "sys_module", "mknod", "sys_chroot"} {
		if strings.Contains(file.Content, forbidden) {
			t.Fatalf("workspace profile must not grant %q:\n%s", forbidden, file.Content)
		}
	}
}

func TestRenderSandboxJobRefusesControlPlaneAndUnknownNode(t *testing.T) {
	cfg := sandboxTestConfig()
	if _, err := RenderSandboxJob(cfg, Sandbox{ID: "aabbccddeeff", Node: "oracle-main", CreatedAt: time.Now().UTC()}); err == nil {
		t.Fatal("expected the control plane to be refused")
	}
	if _, err := RenderSandboxJob(cfg, Sandbox{ID: "aabbccddeeff", Node: "ghost-node", CreatedAt: time.Now().UTC()}); err == nil {
		t.Fatal("expected an unknown node to be refused")
	}
	if _, err := RenderSandboxJob(cfg, Sandbox{Node: "oracle-worker-1", CreatedAt: time.Now().UTC()}); err == nil {
		t.Fatal("expected a missing sandbox id to be refused")
	}
}

func TestRenderSandboxJobRefusesManagedAppCollision(t *testing.T) {
	cfg := sandboxTestConfig()
	cfg.Apps = []App{{Name: "poolctl-sbx-abcabcabcabc", Image: "ghcr.io/x/y:1", Domain: "x.example.com", Port: 80, PreferNode: "oracle-worker-1"}}
	if _, err := RenderSandboxJob(cfg, Sandbox{ID: "abcabcabcabc", Node: "oracle-worker-1", CreatedAt: time.Now().UTC()}); err == nil {
		t.Fatal("expected a job-name collision with a managed app to be refused")
	}
}

func TestAppNamesCannotClaimTheSandboxPrefix(t *testing.T) {
	store := sandboxTestStore(t)
	err := store.AddApp(App{
		Name: "poolctl-sbx-evil", Image: "ghcr.io/example/api:1", Domain: "evil.example.com",
		Port: 8080, PreferNode: "oracle-worker-1", CPU: 500, MemoryMB: 512, HealthPath: "/",
	})
	if err == nil || !strings.Contains(err.Error(), "reserved sandbox prefix") {
		t.Fatalf("expected the reserved prefix to be refused, got %v", err)
	}
}

func TestCreateSandboxRequiresEnrolledHost(t *testing.T) {
	store := sandboxTestStore(t)
	if _, _, err := store.CreateSandbox(Sandbox{Node: "oracle-worker-1"}); err == nil ||
		!strings.Contains(err.Error(), "not enrolled") {
		t.Fatalf("expected an unenrolled node to be refused, got %v", err)
	}
	if _, err := store.EnrollSandboxHost(SandboxHost{Node: "oracle-main"}); err == nil {
		t.Fatal("expected control-plane enrollment to be refused")
	}
	if _, err := store.EnrollSandboxHost(SandboxHost{Node: "ghost"}); err == nil {
		t.Fatal("expected an unknown node to be refused")
	}
}

func TestCreateSandboxEnforcesNodeBudget(t *testing.T) {
	store := sandboxTestStore(t)
	if _, err := store.EnrollSandboxHost(SandboxHost{Node: "oracle-worker-1", MaxSandboxes: 2, MaxCPU: 900, MaxMemoryMB: 1024}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.CreateSandbox(Sandbox{Node: "oracle-worker-1", CPU: 500, MemoryMB: 512}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.CreateSandbox(Sandbox{Node: "oracle-worker-1", CPU: 500, MemoryMB: 512}); err == nil ||
		!strings.Contains(err.Error(), "CPU budget") {
		t.Fatalf("expected the CPU budget to stop the second sandbox, got %v", err)
	}
	if _, _, err := store.CreateSandbox(Sandbox{Node: "oracle-worker-1", CPU: 100, MemoryMB: 512}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.CreateSandbox(Sandbox{Node: "oracle-worker-1", CPU: 100, MemoryMB: 128}); err == nil ||
		!strings.Contains(err.Error(), "already runs 2/2") {
		t.Fatalf("expected the concurrency budget to stop the third sandbox, got %v", err)
	}
}

func TestCreateSandboxRefusesUnavailableNodes(t *testing.T) {
	store := sandboxTestStore(t)
	if _, err := store.EnrollSandboxHost(SandboxHost{Node: "oracle-worker-1"}); err != nil {
		t.Fatal(err)
	}
	_, state, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	state.SetReserved("oracle-worker-1", "splidt")
	if err := store.SaveState(state); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.CreateSandbox(Sandbox{Node: "oracle-worker-1"}); err == nil ||
		!strings.Contains(err.Error(), "reserved_for") {
		t.Fatalf("expected a reserved node to be refused, got %v", err)
	}
}

func TestSandboxEgressRequiresIsolatedHostEnrollment(t *testing.T) {
	store := sandboxTestStore(t)
	if _, err := store.EnrollSandboxHost(SandboxHost{Node: "oracle-worker-1"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.CreateSandbox(Sandbox{Node: "oracle-worker-1", Network: SandboxNetworkEgress}); err == nil ||
		!strings.Contains(err.Error(), "enrolled without egress") {
		t.Fatalf("expected egress to require an isolation-enrolled host, got %v", err)
	}
	if _, err := store.EnrollSandboxHost(SandboxHost{Node: "oracle-worker-1", EgressAllowed: true}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.CreateSandbox(Sandbox{Node: "oracle-worker-1", Network: SandboxNetworkEgress}); err != nil {
		t.Fatal(err)
	}
}

func TestSandboxTokenIsDigestOnlyAndScopedToOneSandbox(t *testing.T) {
	store := sandboxTestStore(t)
	if _, err := store.EnrollSandboxHost(SandboxHost{Node: "oracle-worker-1", MaxSandboxes: 4, MaxCPU: 4000, MaxMemoryMB: 4096}); err != nil {
		t.Fatal(err)
	}
	firstToken, first, err := store.CreateSandbox(Sandbox{Node: "oracle-worker-1", Name: "one"})
	if err != nil {
		t.Fatal(err)
	}
	secondToken, second, err := store.CreateSandbox(Sandbox{Node: "oracle-worker-1", Name: "two"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(firstToken, "poolctl_sbx_v1.") || len(firstToken) < 48 {
		t.Fatalf("unexpected sandbox token shape %q", firstToken)
	}

	raw, err := readFileString(filepath.Join(storeDir(store), "sandboxes.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(raw, firstToken) || strings.Contains(raw, secondToken) {
		t.Fatal("sandbox store must persist only token digests")
	}

	authorized, err := store.AuthorizeSandboxToken(first.ID, firstToken)
	if err != nil || !authorized {
		t.Fatalf("own token must authorize its sandbox: %v %t", err, authorized)
	}
	crossed, err := store.AuthorizeSandboxToken(second.ID, firstToken)
	if err != nil || crossed {
		t.Fatalf("one sandbox token must not authorize another sandbox: %v %t", err, crossed)
	}
	if ok, _ := store.AuthorizeSandboxToken(first.ID, "poolctl_sbx_v1.forged.value"); ok {
		t.Fatal("a forged token must not authorize a sandbox")
	}

	if _, err := store.UpdateSandbox(first.ID, SandboxStatusDestroyed, "done"); err != nil {
		t.Fatal(err)
	}
	if ok, _ := store.AuthorizeSandboxToken(first.ID, firstToken); ok {
		t.Fatal("a destroyed sandbox token must stop authorizing")
	}
}

func TestSandboxExpiryAndExtensionCeiling(t *testing.T) {
	store := sandboxTestStore(t)
	if _, err := store.EnrollSandboxHost(SandboxHost{Node: "oracle-worker-1"}); err != nil {
		t.Fatal(err)
	}
	_, sandbox, err := store.CreateSandbox(Sandbox{Node: "oracle-worker-1", TTLSeconds: SandboxMinTTL})
	if err != nil {
		t.Fatal(err)
	}
	if sandbox.Expired(sandbox.CreatedAt) {
		t.Fatal("a new sandbox must not be expired")
	}
	if !sandbox.Expired(sandbox.ExpiresAt.Add(time.Second)) {
		t.Fatal("a sandbox past its deadline must report expired")
	}
	expired, err := store.ExpiredSandboxes(time.Now().UTC().Add(2 * time.Hour))
	if err != nil || len(expired) != 1 {
		t.Fatalf("expected one expired sandbox, got %d (%v)", len(expired), err)
	}
	if _, err := store.ExtendSandbox(sandbox.ID, SandboxMaxTTL*time.Second); err == nil {
		t.Fatal("expected the absolute lifetime ceiling to be enforced")
	}
	extended, err := store.ExtendSandbox(sandbox.ID, 120*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !extended.ExpiresAt.After(sandbox.ExpiresAt) {
		t.Fatal("extension did not move the deadline")
	}
}

func TestSandboxImageAllowlistAcceptsOnlyUbuntuBases(t *testing.T) {
	allowed := []string{"ubuntu:24.04", "ubuntu:22.04", "docker.io/library/ubuntu:24.04", "public.ecr.aws/ubuntu/ubuntu:24.04",
		"ubuntu@sha256:" + strings.Repeat("a", 64)}
	for _, image := range allowed {
		if !SandboxImageAllowed(image, nil) {
			t.Fatalf("expected %q to be allowed", image)
		}
	}
	refused := []string{"", "ubuntu", "alpine:3.20", "ghcr.io/attacker/tools:latest", "ubuntu:24.04 && curl evil",
		"nvidia/cuda:12.0-ubuntu22.04", "docker.io/library/ubuntu-tools:1"}
	for _, image := range refused {
		if SandboxImageAllowed(image, nil) {
			t.Fatalf("expected %q to be refused", image)
		}
	}
}

func TestSandboxValidationBoundsResources(t *testing.T) {
	store := sandboxTestStore(t)
	if _, err := store.EnrollSandboxHost(SandboxHost{Node: "oracle-worker-1", MaxSandboxes: 4, MaxCPU: 16000, MaxMemoryMB: 32768}); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name    string
		sandbox Sandbox
	}{
		{"cpu", Sandbox{Node: "oracle-worker-1", CPU: SandboxMaxCPU + 1}},
		{"memory", Sandbox{Node: "oracle-worker-1", MemoryMB: SandboxMaxMemoryMB + 1}},
		{"disk", Sandbox{Node: "oracle-worker-1", DiskMB: SandboxMaxDiskMB + 1}},
		{"ttl", Sandbox{Node: "oracle-worker-1", TTLSeconds: SandboxMaxTTL + 1}},
		{"pids", Sandbox{Node: "oracle-worker-1", PIDsLimit: SandboxMaxPIDs + 1}},
		{"profile", Sandbox{Node: "oracle-worker-1", Profile: "privileged"}},
		{"network", Sandbox{Node: "oracle-worker-1", Network: "host"}},
		{"name", Sandbox{Node: "oracle-worker-1", Name: "bad name; rm -rf /"}},
		{"image", Sandbox{Node: "oracle-worker-1", Image: "alpine:3"}},
	}
	for _, testCase := range cases {
		if _, _, err := store.CreateSandbox(testCase.sandbox); err == nil {
			t.Fatalf("expected %s to be refused", testCase.name)
		}
	}
}

func TestSandboxIsolationScriptDeniesPoolDestinations(t *testing.T) {
	files := RenderSandboxIsolation()
	if len(files) != 2 {
		t.Fatalf("expected the isolation bundle to render 2 files, got %d", len(files))
	}
	var script string
	for _, file := range files {
		if file.Path == "sandbox/sandbox-isolation.sh" {
			script = file.Content
		}
	}
	if script == "" {
		t.Fatal("isolation script was not rendered")
	}
	for _, required := range []string{
		"10.44.0.0/24",
		"169.254.0.0/16",
		"10.0.0.0/8",
		"172.16.0.0/12",
		"192.168.0.0/16",
		"--dports 4646,4647,4648",
		"--dport 51820",
		"DOCKER-USER",
		"enable_icc=false",
		SandboxNetworkCIDR,
		SandboxBridgeName,
		"refuse_control_plane",
		"--remove",
	} {
		if !strings.Contains(script, required) {
			t.Fatalf("isolation script is missing %q", required)
		}
	}
}

func TestIsSandboxJobIDGuardsStopPaths(t *testing.T) {
	if IsSandboxJobID("p4lens-api") || IsSandboxJobID("poolctl-sbx-") || IsSandboxJobID("") {
		t.Fatal("only prefixed sandbox job IDs may be treated as sandbox jobs")
	}
	if !IsSandboxJobID(SandboxJobID("abc123")) {
		t.Fatal("a rendered sandbox job ID must be recognized")
	}
}

func storeDir(store Store) string { return store.dir }

func readFileString(path string) (string, error) {
	raw, err := os.ReadFile(path)
	return string(raw), err
}

func TestSandboxTokenStopsAuthorizingAtExpiryBeforeTheReaperRuns(t *testing.T) {
	store := sandboxTestStore(t)
	if _, err := store.EnrollSandboxHost(SandboxHost{Node: "oracle-worker-1"}); err != nil {
		t.Fatal(err)
	}
	token, sandbox, err := store.CreateSandbox(Sandbox{Node: "oracle-worker-1", TTLSeconds: SandboxMinTTL})
	if err != nil {
		t.Fatal(err)
	}

	// The record is still "running" here: nothing has reaped it yet. The
	// deadline alone must be enough to stop the credential working.
	stored, found, err := store.FindSandbox(sandbox.ID)
	if err != nil || !found || stored.Status != SandboxStatusStarting {
		t.Fatalf("sandbox = %+v (found %t, err %v)", stored, found, err)
	}
	authorized, err := store.authorizeSandboxTokenAt(sandbox.ID, token, sandbox.ExpiresAt.Add(-time.Second))
	if err != nil || !authorized {
		t.Fatalf("a live sandbox token must authorize: %t %v", authorized, err)
	}
	expired, err := store.authorizeSandboxTokenAt(sandbox.ID, token, sandbox.ExpiresAt.Add(time.Second))
	if err != nil || expired {
		t.Fatalf("an expired sandbox token must be refused even before reaping: %t %v", expired, err)
	}
}

func TestSandboxIsolationRefusesTheControlPlaneOnAnySingleIndicator(t *testing.T) {
	var script string
	for _, file := range RenderSandboxIsolation() {
		if file.Path == "sandbox/sandbox-isolation.sh" {
			script = file.Content
		}
	}
	start := strings.Index(script, "refuse_control_plane() {")
	if start < 0 {
		t.Fatal("isolation script has no control-plane guard")
	}
	guard := script[start : start+strings.Index(script[start:], "\n}")]
	for _, indicator := range []string{"/etc/traefik/traefik.yml", "/etc/nomad.d/tls", "/var/lib/poolctl/control-plane.ready"} {
		if !strings.Contains(guard, indicator) {
			t.Fatalf("control-plane guard does not check %q:\n%s", indicator, guard)
		}
	}
	// Every indicator must be an independent refusal, never a conjunction.
	if strings.Count(guard, "||") != 2 || strings.Contains(guard, "&&") {
		t.Fatalf("control-plane indicators must each refuse on their own:\n%s", guard)
	}
	if strings.Count(guard, "die ") != 1 {
		t.Fatalf("control-plane guard should refuse exactly once:\n%s", guard)
	}
}
