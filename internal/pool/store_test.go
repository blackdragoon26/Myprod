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
