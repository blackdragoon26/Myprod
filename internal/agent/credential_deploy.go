package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/blackdragoon26/Myprod/internal/pool"
)

func withCredentialVersion(cfg pool.Config, name string, v *credentialVersion) (pool.Config, error) {
	if v == nil {
		return cfg, nil
	}
	cfg.Apps = append([]pool.App(nil), cfg.Apps...)
	for i := range cfg.Apps {
		app := &cfg.Apps[i]
		if app.Name != name {
			continue
		}
		if len(v.Keys) > 0 && app.SecretEnv {
			return cfg, errors.New("Managed secrets conflict with the legacy secret file mount")
		}
		for _, key := range v.Keys {
			if _, exists := app.Env[key]; exists {
				return cfg, errors.New("Managed secrets conflict with ordinary environment variables")
			}
		}
		if v.Registry != "" && imageRegistry(app.Image) != v.Host {
			return cfg, errors.New("Image registry does not match the applied registry connection")
		}
		app.Credentials = &pool.RuntimeCredentials{Version: v.ID, Keys: v.Keys, RegistryHost: v.Host}
	}
	return cfg, nil
}
func imageRegistry(image string) string {
	for i, c := range image {
		if c == '/' {
			return image[:i]
		}
	}
	return ""
}
func (s *server) renderAppJob(ctx context.Context, cfg pool.Config, name string) (pool.RenderedFile, error) {
	if s.secretRequest != nil {
		d, _, err := s.credentialDocument(ctx, name)
		if err != nil {
			return pool.RenderedFile{}, err
		}
		cfg, err = withCredentialVersion(cfg, name, d.Active)
		if err != nil {
			return pool.RenderedFile{}, err
		}
	}
	return pool.RenderAppJob(cfg, name)
}

// Called with deployMu held. Immutable task-scoped paths preserve the exact
// values used by previous allocations, even if an apply fails or is rolled back.
func (s *server) deployCredentials(ctx context.Context, cfg pool.Config, app pool.App, rollback bool) (string, error) {
	d, index, err := s.credentialDocument(ctx, app.Name)
	if err != nil {
		return "", err
	}
	if d.Revision == "" {
		return "", errors.New("Save credentials before applying")
	}
	_, state, err := s.store.Load()
	if err != nil {
		return "", err
	}
	live, err := s.findNomadNode(ctx, app.PreferNode)
	if err != nil {
		return "", errors.New("Cannot verify target node readiness")
	}
	major, _ := strconv.Atoi(strings.SplitN(strings.TrimPrefix(live.Version, "v"), ".", 2)[0])
	if major < 2 {
		return "", errors.New("Managed credentials require Nomad 2.0 or later on the target node")
	}
	ns := state.Nodes[app.PreferNode]
	if live.Status != "ready" || live.SchedulingEligibility != "eligible" || live.Drain || ns.Frozen || ns.Draining || ns.ReservedFor != "" {
		return "", errors.New("Target node is not ready and eligible")
	}
	if app.ManageDNS && state.Apps[app.Name].DNSStatus != "ready" {
		return "", errors.New("Verify managed DNS before applying credentials")
	}
	var previousJob json.RawMessage
	err = s.secretRequest(ctx, "GET", "/v1/job/"+app.Name, nil, &previousJob)
	if err != nil && !errors.Is(err, errNomadMissing) {
		return "", errors.New("Cannot checkpoint existing job; no deployment attempted")
	}
	var version *credentialVersion
	if rollback {
		if d.Previous == nil {
			return "", errors.New("No previous credential version exists")
		}
		version = d.Previous
	} else {
		version = &credentialVersion{ID: freshCredentialID(), DraftRevision: d.Revision, Keys: credentialKeys(d.Values), Registry: d.Registry}
		items := map[string]string{"myprod_version": version.ID}
		for k, v := range d.Values {
			items["env_"+k] = v
		}
		if d.Registry != "" {
			catalog, _, e := s.registryCatalog(ctx)
			if e != nil {
				return "", e
			}
			connection, ok := catalog[d.Registry]
			if !ok {
				return "", errors.New("Saved registry connection no longer exists")
			}
			if e := testGHCRPull(ctx, connection, app.Image); e != nil {
				return "", e
			}
			version.Host = connection.Host
			version.RegistryRevision = connection.Revision
			items["registry_username"] = connection.Username
			items["registry_password"] = connection.Password
		}
		raw, _ := json.Marshal(items)
		if len(raw) > 48000 {
			return "", errors.New("Applied credentials exceed 48 KB")
		}
		path := runtimePath(app.Name, version.ID)
		if e := s.secretRequest(ctx, "PUT", "/v1/var/"+path+"?cas=0", nomadVariable{Path: path, Items: items}, nil); e != nil {
			return "", e
		}
	}
	candidate, err := withCredentialVersion(cfg, app.Name, version)
	if err != nil {
		return "", err
	}
	file, err := pool.RenderAppJob(candidate, app.Name)
	if err != nil {
		return "", err
	}
	temp, err := os.MkdirTemp("", "poolctl-credential-deploy-")
	if err != nil {
		return "", errors.New("Cannot create deployment directory")
	}
	defer os.RemoveAll(temp)
	if err = pool.WriteRendered(temp, []pool.RenderedFile{file}); err != nil {
		return "", errors.New("Cannot render credential deployment")
	}
	out, deployErr := s.nomad(ctx, "job", "run", "-detach", temp+"/"+file.Path)
	if deployErr == nil {
		eval, e := submittedEvaluationID(out)
		deployErr = e
		if e == nil {
			_, deployErr = s.verifyJobDeployment(ctx, app.Name, app.PreferNode, eval)
		}
	}
	if deployErr == nil {
		d.Previous = d.Active
		d.Active = version
		deployErr = s.writeCredentialRecord(ctx, draftPath(app.Name), index, d)
	}
	if deployErr != nil {
		// A disconnected browser must not prevent recovery of the checkpointed job.
		recovery, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if len(previousJob) > 0 {
			var registered struct{ EvalID string }
			e := s.secretRequest(recovery, "POST", "/v1/jobs", map[string]any{"Job": previousJob}, &registered)
			if e == nil {
				_, e = s.verifyJobDeployment(recovery, app.Name, "", "")
			}
			if e != nil {
				return "", errors.New("Credential deployment failed; automatic job restoration could not be verified. Operator attention required")
			}
			return "", errors.New("Credential deployment failed; previous job restored and health verified. Saved draft remains pending")
		}
		_, e := s.nomad(recovery, "job", "stop", "-purge", app.Name)
		if e != nil {
			return "", errors.New("Initial credential deployment failed and cleanup could not be verified. Operator attention required")
		}
		return "", errors.New("Initial credential deployment failed; unsuccessful job removed. Saved draft remains pending")
	}
	if err := s.store.MarkAppManagedCredentials(app.Name); err != nil {
		return "", errors.New("Credentials applied and workload healthy, but compatibility marker could not be saved; operator attention required")
	}
	_, state, err = s.store.Load()
	if err != nil {
		return "", errors.New("Applied workload is healthy but status reload failed")
	}
	state.SetApp(app.Name, app.PreferNode, "deployed")
	if err := s.store.SaveState(state); err != nil {
		return "", errors.New("Credentials applied and workload healthy, but application status could not be saved; refresh before retrying")
	}
	log.Printf("credential_audit action=apply app=%s version=%s rollback=%t", app.Name, version.ID, rollback)
	return fmt.Sprintf("Applied credential version %s to %s. Healthy allocation verified; other apps were not changed.", version.ID, app.Name), nil
}

// Managed task diagnostics may contain third-party registry/task error text.
// Keep such output out of dashboard and CI responses, even on ordinary deploys.
func redactManagedDeployment(managed bool, output *string, err *error) {
	if !managed {
		return
	}
	if *err != nil {
		*output = ""
		*err = errors.New("Managed application deployment failed; credential-bearing diagnostics are withheld. Check image access, node readiness and application health")
	} else {
		*output = "Managed application deployment verified healthy. Credential values and raw task diagnostics are withheld."
	}
}
