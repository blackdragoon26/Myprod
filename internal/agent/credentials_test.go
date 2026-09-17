package agent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/blackdragoon26/Myprod/internal/pool"
)

type fakeCredentialNomad struct {
	vars      map[string]nomadVariable
	job       json.RawMessage
	calls     []string
	failWrite string
}

func (f *fakeCredentialNomad) request(_ context.Context, method, path string, body, result any) error {
	f.calls = append(f.calls, method+" "+path)
	u, _ := url.Parse(path)
	if strings.HasPrefix(u.Path, "/v1/var/") {
		key := strings.TrimPrefix(u.Path, "/v1/var/")
		switch method {
		case "GET":
			v, ok := f.vars[key]
			if !ok {
				return errNomadMissing
			}
			raw, _ := json.Marshal(v)
			return json.Unmarshal(raw, result)
		case "PUT":
			if f.failWrite == key {
				return errors.New("storage unavailable")
			}
			previous := f.vars[key]
			cas, _ := strconv.ParseUint(u.Query().Get("cas"), 10, 64)
			if cas != previous.ModifyIndex {
				return errors.New("CAS conflict")
			}
			raw, _ := json.Marshal(body)
			var v nomadVariable
			json.Unmarshal(raw, &v)
			v.ModifyIndex = previous.ModifyIndex + 1
			f.vars[key] = v
			return nil
		}
	}
	if strings.HasPrefix(u.Path, "/v1/job/") && method == "GET" {
		if len(f.job) == 0 {
			return errNomadMissing
		}
		return json.Unmarshal(f.job, result)
	}
	if u.Path == "/v1/jobs" && method == "POST" {
		raw, _ := json.Marshal(map[string]string{"EvalID": "restore"})
		return json.Unmarshal(raw, result)
	}
	return errors.New("unexpected fake Nomad request")
}
func credentialTestServer(t *testing.T) (*server, *fakeCredentialNomad) {
	t.Helper()
	store := testStore(t)
	if err := store.AddApp(pool.App{Name: "credential-api", Image: "ghcr.io/example/backend:one", Domain: "credentials.example.com", Port: 8080, PreferNode: "oracle-main", CPU: 500, MemoryMB: 512, HealthPath: "/health"}); err != nil {
		t.Fatal(err)
	}
	f := &fakeCredentialNomad{vars: map[string]nomadVariable{}}
	return &server{store: store, token: "operator", secretRequest: f.request}, f
}
func credentialCall(s *server, method, body, operation, token string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "/__poolctl/api/apps/credential-api/credentials", strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	s.handleAppCredentials(w, r, "credential-api", operation)
	return w
}
func TestCredentialDraftIsWriteOnlyAndDoesNotDeploy(t *testing.T) {
	s, f := credentialTestServer(t)
	s.runNomad = func(context.Context, ...string) (string, error) {
		t.Fatal("saving draft must not invoke Nomad CLI")
		return "", nil
	}
	w := credentialCall(s, "PUT", `{"revision":"","values":{"API_KEY":"canary-value","DATABASE_URL":"postgres://private"}}`, "", "operator")
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if strings.Contains(w.Body.String(), "canary-value") || strings.Contains(w.Body.String(), "postgres://private") {
		t.Fatal("response leaked secret")
	}
	for path := range f.vars {
		if strings.HasPrefix(path, "nomad/jobs/") {
			t.Fatal("draft entered workload-readable path")
		}
	}
	d, _, err := s.credentialDocument(context.Background(), "credential-api")
	if err != nil || d.Values["API_KEY"] != "canary-value" {
		t.Fatal("draft missing")
	}
	cfg, _, _ := s.store.Load()
	legacy, _ := pool.RenderAppJob(cfg, "credential-api")
	rendered, e := s.renderAppJob(context.Background(), cfg, "credential-api")
	if e != nil || rendered.Content != legacy.Content {
		t.Fatal("draft changed ordinary deployment")
	}
	bad := credentialCall(s, "PUT", `{"revision":"","values":{"API_KEY":"overwrite"}}`, "", "operator")
	if bad.Code != 409 {
		t.Fatal("stale writer accepted")
	}
	data, _ := json.Marshal(map[string]any{"revision": d.Revision, "values": map[string]any{"API_KEY": nil}})
	w = credentialCall(s, "PUT", string(data), "", "operator")
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	d, _, _ = s.credentialDocument(context.Background(), "credential-api")
	if _, ok := d.Values["API_KEY"]; ok {
		t.Fatal("key removal failed")
	}
	if d.Values["DATABASE_URL"] == "" {
		t.Fatal("unmentioned key lost")
	}
}
func TestCredentialEndpointsRejectCITokensAndDisabledAgents(t *testing.T) {
	s, f := credentialTestServer(t)
	token, _, err := s.store.MintDeployToken("credential-api", "test")
	if err != nil {
		t.Fatal(err)
	}
	w := credentialCall(s, "PUT", `{"values":{"API_KEY":"secret"}}`, "", token)
	if w.Code != 401 {
		t.Fatalf("CI token status %d", w.Code)
	}
	r := httptest.NewRequest("GET", "/__poolctl/api/registries", nil)
	r.Header.Set("Authorization", "Bearer "+token)
	rw := httptest.NewRecorder()
	s.handleRegistries(rw, r)
	if rw.Code != 401 {
		t.Fatal("CI token read registry metadata")
	}
	if len(f.calls) != 0 {
		t.Fatal("unauthorized request touched storage")
	}
	s.secretRequest = nil
	w = credentialCall(s, "GET", "", "", "operator")
	if w.Code != 503 {
		t.Fatal("disabled backend accepted secrets")
	}
}
func TestCredentialValidationAndRedaction(t *testing.T) {
	for _, body := range []string{
		`{"values":{"NOMAD_TOKEN":"test"}}`, `{"values":{"BAD-NAME":"test"}}`, `{"values":{"API_KEY":"test\u0000"}}`,
		`{"values":{"API_KEY":"test"},"unknown":"secret"}`, `{"values":{}} {"values":{"A":"B"}}`,
	} {
		s, _ := credentialTestServer(t)
		w := credentialCall(s, "PUT", body, "", "operator")
		if w.Code != 400 {
			t.Fatalf("bad request accepted: %s", body)
		}
		if strings.Contains(w.Body.String(), "test") {
			t.Fatal("validation reflected credential")
		}
	}
}
func TestCredentialVersionReferencesAreIsolatedAndPreserveLegacy(t *testing.T) {
	s, _ := credentialTestServer(t)
	cfg, _, _ := s.store.Load()
	v := &credentialVersion{ID: strings.Repeat("a", 32), Keys: []string{"API_KEY"}, Registry: "github", Host: "ghcr.io"}
	decorated, err := withCredentialVersion(cfg, "credential-api", v)
	if err != nil {
		t.Fatal(err)
	}
	job, err := pool.RenderAppJob(decorated, "credential-api")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`task "app-` + v.ID + `"`, runtimePath("credential-api", v.ID), `API_KEY = "$${secret.myprod.env_API_KEY}"`, `password = "$${secret.myprod.registry_password}"`} {
		if !strings.Contains(job.Content, want) {
			t.Fatalf("missing reference %s", want)
		}
	}
	if cfg.Apps[len(cfg.Apps)-1].Credentials != nil {
		t.Fatal("decorating mutated original config")
	}
	raw, _ := json.Marshal(decorated)
	if strings.Contains(string(raw), v.ID) {
		t.Fatal("runtime metadata entered app API")
	}
}
func TestCredentialApplyCheckpointsAndRollback(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(strconv.FormatBool(fail), func(t *testing.T) {
			s, f := credentialTestServer(t)
			f.job = json.RawMessage(`{"ID":"credential-api","TaskGroups":[]}`)
			w := credentialCall(s, "PUT", `{"values":{"API_KEY":"fixture-value"}}`, "", "operator")
			if w.Code != 200 {
				t.Fatal(w.Body.String())
			}
			var jobText string
			s.runNomad = func(_ context.Context, args ...string) (string, error) {
				switch strings.Join(args[:min(2, len(args))], " ") {
				case "node status":
					return `[{"ID":"node-id","Name":"oracle-main","Version":"2.0.4","Status":"ready","SchedulingEligibility":"eligible"}]`, nil
				case "job run":
					raw, _ := os.ReadFile(args[len(args)-1])
					jobText = string(raw)
					if fail {
						return "SECRET SHOULD NOT LEAK", errors.New("SECRET SHOULD NOT LEAK")
					}
					return "Evaluation ID: candidate", nil
				case "job status":
					return `[{"Allocations":[{"EvalID":"candidate","JobID":"credential-api","NodeName":"oracle-main","ClientStatus":"running","DesiredStatus":"run","DeploymentStatus":{"Healthy":true}}]}]`, nil
				}
				return "", errors.New("unexpected command")
			}
			w = credentialCall(s, "POST", "", "apply", "operator")
			if strings.Contains(w.Body.String(), "fixture-value") || strings.Contains(w.Body.String(), "SECRET SHOULD NOT LEAK") {
				t.Fatal("deployment output leaked credentials")
			}
			if strings.Contains(jobText, "fixture-value") {
				t.Fatal("rendered job contains secret")
			}
			d, _, _ := s.credentialDocument(context.Background(), "credential-api")
			if fail {
				if w.Code != 500 || d.Active != nil {
					t.Fatal("failed apply activated draft")
				}
				if !strings.Contains(strings.Join(f.calls, "\n"), "POST /v1/jobs") {
					t.Fatal("previous job not restored")
				}
			} else {
				if w.Code != 200 || d.Active == nil {
					t.Fatal(w.Body.String())
				}
				if f.vars[runtimePath("credential-api", d.Active.ID)].Items["env_API_KEY"] != "fixture-value" {
					t.Fatal("immutable version missing")
				}
			}
		})
	}
}

func TestRegistryConnectionsAreWriteOnlyAndCannotBeDeletedWhileReferenced(t *testing.T) {
	s, _ := credentialTestServer(t)
	call := func(method, path, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "/__poolctl/api/registries"+path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer operator")
		w := httptest.NewRecorder()
		s.handleRegistries(w, r)
		return w
	}
	w := call("PUT", "/github", `{"host":"ghcr.io","username":"fixture-user","password":"fixture-pull-password","revision":""}`)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	w = call("GET", "", "")
	if w.Code != 200 || strings.Contains(w.Body.String(), "fixture-pull-password") {
		t.Fatal("registry read leaked values or failed")
	}
	w = credentialCall(s, "PUT", `{"registry":"github"}`, "", "operator")
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	w = call("DELETE", "/github", "")
	if w.Code != 409 {
		t.Fatal("referenced registry deleted")
	}
	w = call("PUT", "/github", `{"host":"ghcr.io","username":"fixture-user","password":"replacement","revision":""}`)
	if w.Code != 409 {
		t.Fatal("stale rotation accepted")
	}
	w = call("PUT", "/other", `{"host":"internal.example","username":"fixture-user","password":"fixture","revision":""}`)
	if w.Code != 400 {
		t.Fatal("unsupported registry accepted")
	}
}

type fixtureRoundTrip func(*http.Request) (*http.Response, error)

func (f fixtureRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestGHCRManifestCheckAndCredentialErrorRedaction(t *testing.T) {
	old := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = old })
	calls := 0
	http.DefaultTransport = fixtureRoundTrip(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.Scheme != "https" || r.URL.Host != "ghcr.io" {
			t.Fatal("credential sent outside GHCR")
		}
		status := 200
		body := `{"token":"fixture-bearer"}`
		if r.Method == "GET" {
			u, p, ok := r.BasicAuth()
			if !ok || u != "fixture-user" || p != "fixture-password" {
				t.Fatal("missing scoped auth")
			}
		} else {
			if r.Method != "HEAD" || r.Header.Get("Authorization") != "Bearer fixture-bearer" {
				t.Fatal("manifest request did not use issued bearer")
			}
			status = 403
			body = "fixture-password must never be reflected"
		}
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header), Request: r}, nil
	})
	err := testGHCRPull(context.Background(), registryConnection{Host: "ghcr.io", Username: "fixture-user", Password: "fixture-password"}, "ghcr.io/example/backend:one")
	if err == nil || strings.Contains(err.Error(), "fixture-password") || calls != 2 {
		t.Fatalf("unsafe registry error or incorrect request flow: %v", err)
	}
	for _, image := range []string{"ghcr.io/example/%bad:one", "https://ghcr.io/example/backend:one", "ghcr.io/example/backend:one?leak=yes", "ghcr.io/../backend:one", "evil.example/backend:one"} {
		before := calls
		err := testGHCRPull(context.Background(), registryConnection{Host: "ghcr.io"}, image)
		if err == nil || calls != before {
			t.Fatal("unsafe image reached network")
		}
	}
}

func TestManagedAppFailsClosedWithoutActiveReferences(t *testing.T) {
	s, _ := credentialTestServer(t)
	if err := s.store.MarkAppManagedCredentials("credential-api"); err != nil {
		t.Fatal(err)
	}
	cfg, _, _ := s.store.Load()
	app, _ := cfg.FindApp("credential-api")
	if !app.ManagedCredentials {
		t.Fatal("compatibility marker not persisted")
	}
	if _, err := pool.RenderAppJob(cfg, app.Name); err == nil {
		t.Fatal("local renderer silently dropped managed credentials")
	}
	app.ManagedCredentials = false
	if err := s.store.UpdateApp(app.Name, app); err != nil {
		t.Fatal(err)
	}
	cfg, _, _ = s.store.Load()
	app, _ = cfg.FindApp(app.Name)
	if !app.ManagedCredentials {
		t.Fatal("old dashboard edit erased marker")
	}
}
