package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestNetlifyDNSEnsureACreatesAndVerifiesRecord(t *testing.T) {
	created := false
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-netlify-token" {
			t.Fatal("missing Netlify authorization header")
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/dns_zones":
			json.NewEncoder(w).Encode([]netlifyZone{{ID: "zone-1", Name: "sankalpjha.dev"}})
		case r.Method == http.MethodGet && r.URL.Path == "/dns_zones/zone-1/dns_records":
			json.NewEncoder(w).Encode([]netlifyRecord{})
		case r.Method == http.MethodPost && r.URL.Path == "/dns_zones/zone-1/dns_records":
			var payload map[string]any
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Fatal(err)
			}
			if payload["type"] != "A" || payload["hostname"] != "splidt-api.sankalpjha.dev" || payload["value"] != "140.245.5.201" {
				t.Fatalf("unexpected record payload %#v", payload)
			}
			created = true
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(netlifyRecord{ID: "record-1"})
		default:
			http.Error(w, "unexpected request", http.StatusNotFound)
		}
	}))
	defer api.Close()

	dns := &netlifyDNS{
		token: "test-netlify-token", zone: "sankalpjha.dev", targetIPv4: "140.245.5.201",
		client: api.Client(), apiBase: api.URL,
		lookupHost: func(context.Context, string) ([]string, error) {
			return []string{"140.245.5.201"}, nil
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	result, err := dns.EnsureA(ctx, "splidt-api.sankalpjha.dev")
	if err != nil {
		t.Fatal(err)
	}
	if !created || result.Status != "ready" {
		t.Fatalf("created=%t result=%#v", created, result)
	}
}

func TestNetlifyDNSEnsureAIsIdempotentAndRejectsConflict(t *testing.T) {
	for _, test := range []struct {
		name       string
		record     netlifyRecord
		wantStatus string
		wantError  bool
	}{
		{name: "existing target", record: netlifyRecord{Hostname: "api.sankalpjha.dev", Type: "A", Value: "140.245.5.201"}, wantStatus: "ready"},
		{name: "conflicting target", record: netlifyRecord{Hostname: "api.sankalpjha.dev", Type: "A", Value: "203.0.113.8"}, wantStatus: "conflict", wantError: true},
		{name: "conflicting cname", record: netlifyRecord{Hostname: "api.sankalpjha.dev", Type: "CNAME", Value: "elsewhere.example.com"}, wantStatus: "conflict", wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/dns_zones":
					json.NewEncoder(w).Encode([]netlifyZone{{ID: "zone-1", Domain: "sankalpjha.dev"}})
				case "/dns_zones/zone-1/dns_records":
					if r.Method != http.MethodGet {
						t.Fatal("idempotent/conflict checks must not create a record")
					}
					json.NewEncoder(w).Encode([]netlifyRecord{test.record})
				default:
					http.NotFound(w, r)
				}
			}))
			defer api.Close()
			dns := &netlifyDNS{
				token: "token", zone: "sankalpjha.dev", targetIPv4: "140.245.5.201",
				client: api.Client(), apiBase: api.URL,
				lookupHost: func(context.Context, string) ([]string, error) { return []string{"140.245.5.201"}, nil },
			}
			result, err := dns.EnsureA(context.Background(), "api.sankalpjha.dev")
			if (err != nil) != test.wantError || result.Status != test.wantStatus {
				t.Fatalf("result=%#v err=%v", result, err)
			}
		})
	}
}

func TestNetlifyDNSEnsureARequiresConfigurationAndManagedZone(t *testing.T) {
	dns := &netlifyDNS{}
	result, err := dns.EnsureA(context.Background(), "api.sankalpjha.dev")
	if err == nil || result.Status != "unconfigured" {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	dns = &netlifyDNS{token: "token", zone: "sankalpjha.dev", targetIPv4: "140.245.5.201", client: http.DefaultClient}
	result, err = dns.EnsureA(context.Background(), "api.example.com")
	if err == nil || result.Status != "error" || !strings.Contains(err.Error(), "outside managed zone") {
		t.Fatalf("result=%#v err=%v", result, err)
	}
}
