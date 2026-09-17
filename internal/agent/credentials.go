package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Drafts and registry connections live outside workload-readable paths. Each
// applied version gets an immutable variable at its task's exact identity path.
// Neither editing a draft nor rotating a connection can restart a workload.
type credentialVersion struct {
	ID               string   `json:"id"`
	DraftRevision    string   `json:"draftRevision"`
	Keys             []string `json:"keys"`
	Registry         string   `json:"registry,omitempty"`
	RegistryRevision string   `json:"registryRevision,omitempty"`
	Host             string   `json:"host,omitempty"`
}
type credentialDocument struct {
	Values   map[string]string  `json:"values"`
	Registry string             `json:"registry"`
	Revision string             `json:"revision"`
	Active   *credentialVersion `json:"active,omitempty"`
	Previous *credentialVersion `json:"previous,omitempty"`
}
type registryConnection struct {
	Name     string `json:"name"`
	Host     string `json:"host"`
	Username string `json:"username"`
	Password string `json:"password"`
	Revision string `json:"revision"`
}
type nomadVariable struct {
	Path        string
	Items       map[string]string
	ModifyIndex uint64
}
type credentialMetadata struct {
	Keys     []string           `json:"keys"`
	Registry string             `json:"registry"`
	Revision string             `json:"revision"`
	Active   *credentialVersion `json:"active,omitempty"`
	Previous *credentialVersion `json:"previous,omitempty"`
	Pending  bool               `json:"pending"`
}
type registryMetadata struct {
	Name     string `json:"name"`
	Host     string `json:"host"`
	Username string `json:"username"`
	Revision string `json:"revision"`
}

func freshCredentialID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}
func draftPath(name string) string       { return "myprod/apps/" + name }
func runtimePath(name, id string) string { return "nomad/jobs/" + name + "/web/app-" + id }
func (s *server) readCredentialRecord(ctx context.Context, path string, result any) (uint64, error) {
	var v nomadVariable
	err := s.secretRequest(ctx, "GET", "/v1/var/"+path, nil, &v)
	if errors.Is(err, errNomadMissing) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if err := json.Unmarshal([]byte(v.Items["document"]), result); err != nil {
		return 0, errors.New("invalid credential record; refusing to overwrite")
	}
	return v.ModifyIndex, nil
}
func (s *server) writeCredentialRecord(ctx context.Context, path string, index uint64, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return errors.New("cannot encode credential record")
	}
	if len(raw) > 48000 {
		return errors.New("credential record exceeds 48 KB")
	}
	return s.secretRequest(ctx, "PUT", fmt.Sprintf("/v1/var/%s?cas=%d", path, index), nomadVariable{Path: path, Items: map[string]string{"document": string(raw)}}, nil)
}
func (s *server) credentialDocument(ctx context.Context, name string) (credentialDocument, uint64, error) {
	d := credentialDocument{Values: map[string]string{}}
	index, err := s.readCredentialRecord(ctx, draftPath(name), &d)
	return d, index, err
}
func (s *server) registryCatalog(ctx context.Context) (map[string]registryConnection, uint64, error) {
	c := map[string]registryConnection{}
	index, err := s.readCredentialRecord(ctx, "myprod/registries", &c)
	return c, index, err
}
func credentialKeys(values map[string]string) []string {
	keys := make([]string, 0, len(values))
	for k := range values {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
func validCredentialKey(k string) bool {
	if len(k) == 0 || len(k) > 128 || strings.HasPrefix(k, "NOMAD_") || strings.HasPrefix(k, "MYPROD_") {
		return false
	}
	for i, c := range k {
		if !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c == '_' || i > 0 && c >= '0' && c <= '9') {
			return false
		}
	}
	return true
}
func decodeCredentialRequest(w http.ResponseWriter, r *http.Request, v any) error {
	r.Body = http.MaxBytesReader(w, r.Body, 48<<10)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if d.Decode(v) != nil {
		return errors.New("invalid credential request (maximum 48 KB)")
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return errors.New("request must contain one JSON object")
	}
	return nil
}
func (s *server) credentialAccess(w http.ResponseWriter, r *http.Request) bool {
	if !s.authorized(w, r) {
		return false
	}
	if s.secretRequest == nil {
		writeJSON(w, 503, response{Error: "Managed credentials are not enabled on this agent"})
		return false
	}
	return true
}
func (s *server) handleAppCredentials(w http.ResponseWriter, r *http.Request, name, operation string) {
	if !s.credentialAccess(w, r) {
		return
	}
	s.deployMu.Lock()
	defer s.deployMu.Unlock()
	cfg, _, err := s.store.Load()
	if err != nil {
		writeJSON(w, 500, response{Error: "Cannot load applications"})
		return
	}
	app, ok := cfg.FindApp(name)
	if !ok {
		writeJSON(w, 404, response{Error: "Unknown application"})
		return
	}
	if operation != "" {
		if r.Method != "POST" || (operation != "apply" && operation != "rollback") {
			writeJSON(w, 405, response{Error: "method not allowed"})
			return
		}
		output, err := s.deployCredentials(r.Context(), cfg, app, operation == "rollback")
		if err != nil {
			writeJSON(w, 500, response{Error: err.Error(), Output: output})
			return
		}
		writeJSON(w, 200, response{OK: true, Output: output})
		return
	}
	d, index, err := s.credentialDocument(r.Context(), name)
	if err != nil {
		writeJSON(w, 500, response{Error: err.Error()})
		return
	}
	switch r.Method {
	case "PUT":
		var req struct {
			Revision string             `json:"revision"`
			Values   map[string]*string `json:"values"`
			Registry *string            `json:"registry"`
		}
		if err := decodeCredentialRequest(w, r, &req); err != nil {
			writeJSON(w, 400, response{Error: err.Error()})
			return
		}
		if req.Revision != d.Revision {
			writeJSON(w, 409, response{Error: "Credentials changed; reopen this screen before saving"})
			return
		}
		for k, v := range req.Values {
			if !validCredentialKey(k) || v != nil && (len(*v) > 16384 || strings.ContainsRune(*v, 0)) {
				writeJSON(w, 400, response{Error: "Invalid secret name or value (16 KB maximum per value; no NUL)"})
				return
			}
			if v == nil {
				delete(d.Values, k)
			} else {
				d.Values[k] = *v
			}
		}
		if len(d.Values) > 64 {
			writeJSON(w, 400, response{Error: "At most 64 secrets are supported"})
			return
		}
		for k := range d.Values {
			if _, exists := app.Env[k]; exists {
				writeJSON(w, 400, response{Error: "A secret name duplicates an ordinary environment variable"})
				return
			}
		}
		if req.Registry != nil {
			if *req.Registry != "" {
				catalog, _, e := s.registryCatalog(r.Context())
				if e != nil {
					writeJSON(w, 500, response{Error: e.Error()})
					return
				}
				registry, exists := catalog[*req.Registry]
				if !exists || !strings.HasPrefix(app.Image, registry.Host+"/") {
					writeJSON(w, 400, response{Error: "Select a saved connection matching the image registry"})
					return
				}
			}
			d.Registry = *req.Registry
		}
		if len(d.Values) > 0 && app.SecretEnv {
			writeJSON(w, 400, response{Error: "Disable the legacy file mount in Edit before opting into managed environment secrets"})
			return
		}
		d.Revision = freshCredentialID()
		if err := s.writeCredentialRecord(r.Context(), draftPath(name), index, d); err != nil {
			writeJSON(w, 500, response{Error: err.Error()})
			return
		}
		log.Printf("credential_audit action=save_draft app=%s revision=%s", name, d.Revision)
	case "GET":
	default:
		writeJSON(w, 405, response{Error: "method not allowed"})
		return
	}
	pending := d.Active == nil && d.Revision != "" || d.Active != nil && d.Active.DraftRevision != d.Revision
	if d.Registry != "" {
		c, _, e := s.registryCatalog(r.Context())
		if e != nil {
			writeJSON(w, 500, response{Error: e.Error()})
			return
		}
		if d.Active == nil || d.Active.RegistryRevision != c[d.Registry].Revision {
			pending = true
		}
	}
	writeJSON(w, 200, response{OK: true, Credentials: &credentialMetadata{Keys: credentialKeys(d.Values), Registry: d.Registry, Revision: d.Revision, Active: d.Active, Previous: d.Previous, Pending: pending}})
}
func (s *server) handleRegistries(w http.ResponseWriter, r *http.Request) {
	if !s.credentialAccess(w, r) {
		return
	}
	s.deployMu.Lock()
	defer s.deployMu.Unlock()
	catalog, index, err := s.registryCatalog(r.Context())
	if err != nil {
		writeJSON(w, 500, response{Error: err.Error()})
		return
	}
	name := strings.TrimPrefix(r.URL.Path, "/__poolctl/api/registries")
	name = strings.TrimPrefix(name, "/")
	if name == "" && r.Method == "GET" {
		list := []registryMetadata{}
		for _, c := range catalog {
			list = append(list, registryMetadata{Name: c.Name, Host: c.Host, Username: c.Username, Revision: c.Revision})
		}
		sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })
		writeJSON(w, 200, response{OK: true, Registries: list})
		return
	}
	if strings.HasSuffix(name, "/test") && r.Method == "POST" {
		c, ok := catalog[strings.TrimSuffix(name, "/test")]
		if !ok {
			writeJSON(w, 404, response{Error: "Unknown registry connection"})
			return
		}
		var req struct {
			Image string `json:"image"`
		}
		if decodeCredentialRequest(w, r, &req) != nil {
			writeJSON(w, 400, response{Error: "Invalid image test request"})
			return
		}
		if err := testGHCRPull(r.Context(), c, req.Image); err != nil {
			writeJSON(w, 400, response{Error: err.Error()})
			return
		}
		writeJSON(w, 200, response{OK: true, Output: "Registry accepted this credential for the selected image manifest. No workload changed."})
		return
	}
	if !validProjectID(name) || len(name) > 64 {
		writeJSON(w, 400, response{Error: "Connection name must use letters, numbers, dash or underscore (maximum 64)"})
		return
	}
	switch r.Method {
	case "PUT":
		var req struct {
			Host     string `json:"host"`
			Username string `json:"username"`
			Password string `json:"password"`
			Revision string `json:"revision"`
		}
		if decodeCredentialRequest(w, r, &req) != nil {
			writeJSON(w, 400, response{Error: "Invalid registry request"})
			return
		}
		old := catalog[name]
		if old.Revision != req.Revision {
			writeJSON(w, 409, response{Error: "Connection changed; reload before saving"})
			return
		}
		if req.Host != "ghcr.io" || req.Username == "" || len(req.Username) > 128 || strings.ContainsAny(req.Username, ":\r\n") || req.Password == "" || len(req.Password) > 8192 || strings.ContainsAny(req.Password, "\x00\r\n") {
			writeJSON(w, 400, response{Error: "Provide ghcr.io, a GitHub username and a pull credential"})
			return
		}
		catalog[name] = registryConnection{Name: name, Host: req.Host, Username: req.Username, Password: req.Password, Revision: freshCredentialID()}
		if len(catalog) > 32 {
			writeJSON(w, 400, response{Error: "At most 32 registry connections are supported"})
			return
		}
	case "DELETE":
		if _, exists := catalog[name]; !exists {
			writeJSON(w, 404, response{Error: "Unknown registry connection"})
			return
		}
		cfg, _, e := s.store.Load()
		if e != nil {
			writeJSON(w, 500, response{Error: "Cannot check registry use"})
			return
		}
		for _, app := range cfg.Apps {
			d, _, e := s.credentialDocument(r.Context(), app.Name)
			if e != nil {
				writeJSON(w, 500, response{Error: e.Error()})
				return
			}
			if d.Registry == name || d.Active != nil && d.Active.Registry == name || d.Previous != nil && d.Previous.Registry == name {
				writeJSON(w, 409, response{Error: "Connection is referenced by an app draft, active version or rollback version; detach it before deletion"})
				return
			}
		}
		delete(catalog, name)
	default:
		writeJSON(w, 405, response{Error: "method not allowed"})
		return
	}
	if err := s.writeCredentialRecord(r.Context(), "myprod/registries", index, catalog); err != nil {
		writeJSON(w, 500, response{Error: err.Error()})
		return
	}
	log.Printf("credential_audit action=registry_%s connection=%s", r.Method, name)
	writeJSON(w, 200, response{OK: true, Output: "Registry connection saved. Running apps are unchanged; apply credentials per app to use a rotated connection."})
}

var ghcrImagePattern = regexp.MustCompile(`^ghcr\.io/[a-z0-9][a-z0-9._-]*(/[a-z0-9][a-z0-9._-]*)+(?::[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}|@sha256:[a-f0-9]{64})?$`)

func testGHCRPull(ctx context.Context, c registryConnection, image string) error {
	// Fixed HTTPS endpoints and no redirects prevent credential exfiltration/SSRF.
	if c.Host != "ghcr.io" || len(image) > 512 || !ghcrImagePattern.MatchString(image) {
		return errors.New("Image must belong to ghcr.io")
	}
	repo := strings.TrimPrefix(imageRepository(image), "ghcr.io/")
	if !strings.Contains(repo, "/") || strings.ContainsAny(repo, " ?#\\\r\n") {
		return errors.New("Invalid GHCR repository")
	}
	ref := "latest"
	if _, digest, ok := strings.Cut(image, "@"); ok {
		ref = digest
	} else if i := strings.LastIndex(image, ":"); i > strings.LastIndex(image, "/") {
		ref = image[i+1:]
	}
	if strings.ContainsAny(ref, "/?#\\\r\n") || ref == "" {
		return errors.New("Invalid image reference")
	}
	client := &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, _ := http.NewRequestWithContext(ctx, "GET", "https://ghcr.io/token?service=ghcr.io&scope="+url.QueryEscape("repository:"+repo+":pull"), nil)
	req.SetBasicAuth(c.Username, c.Password)
	resp, err := client.Do(req)
	if err != nil {
		return errors.New("GHCR authentication request failed")
	}
	defer resp.Body.Close()
	var token struct {
		Token string `json:"token"`
	}
	if resp.StatusCode != 200 || json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&token) != nil || token.Token == "" {
		return errors.New("GHCR rejected this credential; check package read access and organization authorization")
	}
	req, _ = http.NewRequestWithContext(ctx, "HEAD", "https://ghcr.io/v2/"+repo+"/manifests/"+ref, nil)
	req.Header.Set("Authorization", "Bearer "+token.Token)
	req.Header.Set("Accept", "application/vnd.oci.image.index.v1+json, application/vnd.docker.distribution.manifest.list.v2+json, application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.v2+json")
	result, err := client.Do(req)
	if err != nil {
		return errors.New("GHCR manifest check failed")
	}
	defer result.Body.Close()
	if result.StatusCode != 200 {
		return fmt.Errorf("GHCR image access failed (HTTP %d); check the image and package permissions", result.StatusCode)
	}
	return nil
}

// Only called after the app workload has been stopped. Restrict deletion to
// Myprod's exact random-version namespace; preserve legacy host files.
func (s *server) deleteAppCredentials(ctx context.Context, name string) error {
	if s.secretRequest == nil {
		return nil
	}
	prefix := "nomad/jobs/" + name + "/web/app-"
	var variables []nomadVariable
	if err := s.secretRequest(ctx, "GET", "/v1/vars?prefix="+url.QueryEscape(prefix), nil, &variables); err != nil {
		return err
	}
	for _, v := range variables {
		suffix := strings.TrimPrefix(v.Path, prefix)
		if !strings.HasPrefix(v.Path, prefix) || len(suffix) != 32 {
			continue
		}
		if _, err := hex.DecodeString(suffix); err != nil {
			continue
		}
		if err := s.secretRequest(ctx, "DELETE", "/v1/var/"+v.Path, nil, nil); err != nil && !errors.Is(err, errNomadMissing) {
			return err
		}
	}
	if err := s.secretRequest(ctx, "DELETE", "/v1/var/"+draftPath(name), nil, nil); err != nil && !errors.Is(err, errNomadMissing) {
		return err
	}
	log.Printf("credential_audit action=delete_app_credentials app=%s", name)
	return nil
}
