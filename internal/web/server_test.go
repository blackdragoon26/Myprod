package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/blackdragoon26/Myprod/internal/pool"
)

func TestActionArgs(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/action", nil)
	req.Form = map[string][]string{
		"action": {"app-deploy"},
		"app":    {"sample-api"},
	}
	got, err := actionArgs(req)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"app", "deploy", "sample-api"}
	if len(got) != len(want) {
		t.Fatalf("len(action args) = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("arg %d = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestActionArgsNodeJoin(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/action", nil)
	req.Form = map[string][]string{
		"action": {"node-join"},
		"node":   {"do-worker-1"},
	}
	got, err := actionArgs(req)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"node", "join", "do-worker-1"}
	if len(got) != len(want) {
		t.Fatalf("len(action args) = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("arg %d = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestWantsJSON(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/action", nil)
	req.Header.Set("Accept", "application/json")
	if !wantsJSON(req) {
		t.Fatal("expected JSON accept header to request JSON response")
	}
}

func TestWriteActionJSON(t *testing.T) {
	rec := httptest.NewRecorder()
	writeActionJSON(rec, "out", "err")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	var got actionResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Output != "out" || got.Error != "err" {
		t.Fatalf("unexpected response: %#v", got)
	}
}

func TestActionArgsRejectsMissingTarget(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/action", nil)
	req.Form = map[string][]string{"action": {"node-freeze"}}
	if _, err := actionArgs(req); err == nil {
		t.Fatal("expected missing node to fail")
	}
}

func TestLoopbackDetection(t *testing.T) {
	if !isLoopbackAddr("127.0.0.1:8088") {
		t.Fatal("expected loopback addr")
	}
	if isLoopbackAddr("0.0.0.0:8088") {
		t.Fatal("0.0.0.0 must require a password")
	}
}

func TestNodeFromForm(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/action", nil)
	req.Form = map[string][]string{
		"name":       {"do-worker-1"},
		"public_ip":  {"203.0.113.10"},
		"ssh_user":   {"ubuntu"},
		"ssh_key":    {"~/.ssh/keys/digitalocean-worker.key"},
		"overlay_ip": {"10.44.0.2"},
	}
	got, err := nodeFromForm(req)
	if err != nil {
		t.Fatal(err)
	}
	if got.Provider != "digitalocean" || got.CostMode != "credit_temporary" || got.Placement != "burst" {
		t.Fatalf("unexpected defaults: %#v", got)
	}
	if !got.Guard.Enabled || got.Guard.MaxLoad1 != 3.5 {
		t.Fatalf("unexpected guard defaults: %#v", got.Guard)
	}
}

func TestNodeFromFormRejectsInvalidIP(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/action", nil)
	req.Form = map[string][]string{
		"name":       {"do-worker-1"},
		"public_ip":  {"not-an-ip"},
		"ssh_user":   {"ubuntu"},
		"ssh_key":    {"~/.ssh/keys/digitalocean-worker.key"},
		"overlay_ip": {"10.44.0.2"},
	}
	if _, err := nodeFromForm(req); err == nil {
		t.Fatal("expected invalid public IP to fail")
	}
}

func TestNextOverlayIP(t *testing.T) {
	got := nextOverlayIP(pool.Config{Nodes: []pool.Node{
		{Name: "oracle-main", OverlayIP: "10.44.0.1"},
		{Name: "do-worker-1", OverlayIP: "10.44.0.2"},
	}})
	if got != "10.44.0.3" {
		t.Fatalf("nextOverlayIP = %q, want 10.44.0.3", got)
	}
}
