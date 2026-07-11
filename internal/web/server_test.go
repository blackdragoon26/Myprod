package web

import (
	"net/http"
	"net/http/httptest"
	"testing"
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
