package pool

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestStoreInitAndLoad(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), ".poolctl"))
	created, err := store.Init()
	if err != nil {
		t.Fatal(err)
	}
	if !created {
		t.Fatal("expected store to be created")
	}

	cfg, state, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Name != "personal-compute-pool" {
		t.Fatalf("unexpected pool name %q", cfg.Name)
	}
	if !cfg.HasNode("oracle-main") {
		t.Fatal("expected oracle-main node")
	}
	if state.Nodes["oracle-main"].Frozen {
		t.Fatal("oracle-main should start unfrozen")
	}
}

func TestStateTransitions(t *testing.T) {
	state := State{}
	state.SetFrozen("oracle-main", true)
	if !state.Nodes["oracle-main"].Frozen {
		t.Fatal("expected frozen node")
	}
	state.SetDraining("oracle-main", true)
	if !state.Nodes["oracle-main"].Draining || !state.Nodes["oracle-main"].Frozen {
		t.Fatal("draining node should also be frozen")
	}
	state.SetFrozen("oracle-main", false)
	if state.Nodes["oracle-main"].Frozen {
		t.Fatal("expected unfrozen node")
	}
	state.SetApp("sample-api", "oracle-main", "deployed")
	if state.Apps["sample-api"].Node != "oracle-main" || state.Apps["sample-api"].Status != "deployed" {
		t.Fatal("expected app deployment state")
	}
}

func TestRenderControlPlaneRequiresConnectionFields(t *testing.T) {
	_, err := RenderControlPlane(Config{
		Nodes: []Node{{
			Name:      "oracle-main",
			Role:      "control-plane",
			OverlayIP: "10.44.0.1",
		}},
	})
	if err == nil {
		t.Fatal("expected missing connection fields to fail")
	}
}

func TestRenderControlPlane(t *testing.T) {
	files, err := RenderControlPlane(Config{
		Nodes: []Node{{
			Name:      "oracle-main",
			Role:      "control-plane",
			PublicIP:  "140.245.5.201",
			SSHUser:   "ubuntu",
			SSHKey:    "~/.ssh/keys/openclaw-oracle.key",
			OverlayIP: "10.44.0.1",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 8 {
		t.Fatalf("expected 8 files, got %d", len(files))
	}

	var bootstrap, nomadService string
	for _, file := range files {
		switch file.Path {
		case "bootstrap-control-plane.sh":
			bootstrap = file.Content
		case "systemd/nomad.service":
			nomadService = file.Content
		}
	}
	if strings.Contains(nomadService, "User=nomad") || strings.Contains(nomadService, "Group=nomad") {
		t.Fatal("nomad service should run with client privileges for Docker/cgroups")
	}
	if !strings.Contains(bootstrap, `POOLCTL_READY_MARKER="$POOLCTL_STATE_DIR/control-plane.ready"`) {
		t.Fatal("bootstrap should mark completed control planes")
	}
	if !strings.Contains(bootstrap, `NOMAD_ACL_DIR="$POOLCTL_STATE_DIR/nomad-acl"`) {
		t.Fatal("bootstrap should keep ACL tokens outside Nomad's config file scan path")
	}
	if !strings.Contains(bootstrap, "migrate_nomad_acl_files") {
		t.Fatal("bootstrap should migrate older ACL token files")
	}
	if !strings.Contains(bootstrap, `"$NOMAD_ADDR/v1/status/leader" 2>/dev/null || true`) {
		t.Fatal("nomad readiness HTTP fallback should tolerate connection-refused during startup")
	}
	if !strings.Contains(bootstrap, "run_nomad_acl_bootstrap") || !strings.Contains(bootstrap, "/opt/nomad/acl-bootstrap-reset") {
		t.Fatal("bootstrap should recover when Nomad ACL is already bootstrapped but the local token is missing")
	}
	if !strings.Contains(bootstrap, "archive_nomad_server_state_for_bootstrap") || !strings.Contains(bootstrap, "server.bootstrap-recovery") {
		t.Fatal("bootstrap should archive interrupted pre-ready Nomad server state as a last resort")
	}
	if !strings.Contains(bootstrap, "bootstrap failed near line") {
		t.Fatal("bootstrap should report failing shell line for remote diagnostics")
	}
}

func TestRenderAppJob(t *testing.T) {
	file, err := RenderAppJob(Config{
		Apps: []App{{
			Name:   "sample-api",
			Image:  "ghcr.io/example/sample-api:latest",
			Domain: "api.example.com",
			Port:   3000,
		}},
	}, "sample-api")
	if err != nil {
		t.Fatal(err)
	}
	if file.Path != "nomad/jobs/sample-api.nomad.hcl" {
		t.Fatalf("unexpected path %q", file.Path)
	}
	if !strings.Contains(file.Content, "provider = \"nomad\"") {
		t.Fatal("expected nomad service provider")
	}
}
