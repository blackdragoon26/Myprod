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

func TestNormalizeRemoteHome(t *testing.T) {
	cases := map[string]string{
		"~":                  ".",
		"~/poolctl-jobs":     "poolctl-jobs",
		"$HOME":              ".",
		"$HOME/poolctl-jobs": "poolctl-jobs",
		"/opt/poolctl-jobs":  "/opt/poolctl-jobs",
	}

	for input, want := range cases {
		if got := normalizeRemoteHome(input); got != want {
			t.Fatalf("normalizeRemoteHome(%q) = %q, want %q", input, got, want)
		}
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

	var bootstrap, nomadService, traefikConfig, traefikService string
	for _, file := range files {
		switch file.Path {
		case "bootstrap-control-plane.sh":
			bootstrap = file.Content
		case "systemd/nomad.service":
			nomadService = file.Content
		case "traefik/traefik.yml":
			traefikConfig = file.Content
		case "systemd/traefik.service":
			traefikService = file.Content
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
	if !strings.Contains(bootstrap, "read_nomad_token()") || strings.Contains(bootstrap, "return 1\n}\n\nparse_nomad_secret_id") {
		t.Fatal("optional token reader should not fail when no token exists")
	}
	if !strings.Contains(bootstrap, "empty-token.") || !strings.Contains(bootstrap, "invalid-token.") {
		t.Fatal("bootstrap should archive empty or invalid Nomad token files")
	}
	if !strings.Contains(bootstrap, "nomad_token_can_read_services") {
		t.Fatal("bootstrap should verify existing Nomad tokens before reusing them")
	}
	if !strings.Contains(bootstrap, `NOMAD_TRAEFIK_TOKEN="$token"`) || !strings.Contains(bootstrap, `token="$NOMAD_TRAEFIK_TOKEN"`) {
		t.Fatal("bootstrap should pass the verified Nomad token directly into Traefik rendering")
	}
	if !strings.Contains(bootstrap, `sed -n 's/.*"SecretID"`) {
		t.Fatal("bootstrap should parse SecretID from one-line Nomad JSON")
	}
	if !strings.Contains(bootstrap, "systemctl stop traefik") {
		t.Fatal("bootstrap should stop stale Traefik before rotating Nomad TLS or tokens")
	}
	if !strings.Contains(bootstrap, "bootstrap failed near line") {
		t.Fatal("bootstrap should report failing shell line for remote diagnostics")
	}
	if !strings.Contains(bootstrap, "ensure_host_ingress_firewall") || !strings.Contains(bootstrap, "poolctl-ingress-http") || !strings.Contains(bootstrap, "poolctl-ingress-https") {
		t.Fatal("bootstrap should open Oracle host iptables ingress before provider traffic reaches UFW")
	}
	if !strings.Contains(bootstrap, "validate_nomad_token") || !strings.Contains(bootstrap, `"$NOMAD_ADDR/v1/services"`) {
		t.Fatal("bootstrap should validate the Nomad token before starting Traefik")
	}
	if !strings.Contains(bootstrap, "render_traefik_config") || !strings.Contains(bootstrap, "/etc/traefik/traefik.yml.template") {
		t.Fatal("bootstrap should render Traefik config with the verified Nomad token")
	}
	if !strings.Contains(bootstrap, "/var/lib/traefik/acme.json") || !strings.Contains(bootstrap, "certificatesResolvers:") {
		t.Fatal("bootstrap should configure Traefik ACME storage and resolver")
	}
	if !strings.Contains(traefikConfig, "token: \"__NOMAD_TOKEN__\"") {
		t.Fatal("traefik config should use a systemd-safe token placeholder")
	}
	if !strings.Contains(traefikConfig, "letsencrypt:") || !strings.Contains(traefikConfig, "httpChallenge:") {
		t.Fatal("traefik config should include the Let's Encrypt HTTP challenge resolver")
	}
	if strings.Contains(traefikService, "EnvironmentFile=") || strings.Contains(traefikService, "ExecStartPre=") {
		t.Fatal("traefik service should read the rendered config directly")
	}
	if !strings.Contains(traefikService, "--configFile=/etc/traefik/traefik.yml") {
		t.Fatal("traefik service should use the rendered config file")
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
	if !strings.Contains(file.Content, "tls.certresolver=letsencrypt") {
		t.Fatal("expected HTTPS router to use the Let's Encrypt resolver")
	}
}
