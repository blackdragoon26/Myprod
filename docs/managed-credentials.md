# Managed app secrets and private GHCR images

The dashboard offers **Registry connections** and each app's **Secrets &
registry** when the authenticated agent advertises `registryConnectionsV1` and
`appSecretsV1`. These require `POOLCTL_MANAGED_CREDENTIALS=true` on the Oracle
agent and Nomad 2.0 or later on the server and selected application node.
The current pool uses 2.0.4. Older agents omit these flags; their existing app,
legacy secrets-file, and CI workflows remain unchanged.

## Operator workflow

1. Register an ARM64 or multi-platform image and its app configuration as usual.
   Registration does not deploy. Private source repositories and images are
   supported; never package runtime secrets inside an image.
2. For a private GHCR image, open **Registry connections**. Save a connection
   name, GitHub username, and a personal access token (classic) with
   `read:packages` and access to the intended packages.
   Authorize organization SSO if required. The initial provider is `ghcr.io`;
   other registry providers are not yet supported by this screen.
3. Select the saved connection and test access to an exact image tag or digest.
   This checks authorization to read its manifest without deploying anything.
   It does not check image architecture or application health.
4. Open the app's **Secrets & registry**. Add `NAME=value` lines and select the
   saved connection. Values are literal: quotes are retained, no expansion or
   shell execution occurs. Unmentioned keys are kept. Check Remove to delete a
   key. The API also accepts newline-containing values through JSON strings.
5. **Save draft** stores encrypted values but does not deliver them to any
   running task. Existing values are never returned to the browser. Only key
   names, connection names, revisions and pending/applied state are displayed.
6. **Apply & restart app** explicitly deploys the current app configuration with
   the saved draft and current registry connection. It checks node eligibility,
   DNS readiness, registry access where applicable, and deployment health.
   This can briefly interrupt this app. No other app is resubmitted.
7. Ordinary **Deploy**, image updates, and app-scoped CI deployments continue
   using the last successfully applied credential version. They do not apply
   pending secret edits or rotated registry connections implicitly.
8. **Restore previous credentials** explicitly deploys the current app
   configuration using its previous successfully applied managed version. It
   does not roll back the image or other app configuration. On a failed apply,
   the agent instead automatically restores the entire checkpointed job and
   verifies its health; if recovery fails it reports operator attention needed.

Registry credentials are saved once and reusable until expired or revoked.
Rotating a saved connection leaves running apps unchanged. Apply credentials
on each consuming app before revoking the old token at GitHub. A connection
cannot be deleted while referenced by a draft, active version or previous
managed version. Detach and apply a no-connection version, then apply it again
to replace the previous reference if deletion is intended. Deleting a saved
connection does not revoke its token at GitHub.

Managed runtime secrets become ordinary process environment variables via
Nomad secret interpolation; applications do not need to load a dotenv file.
The legacy `SecretEnv` mount is preserved exactly for existing applications.
Disable that mount in Edit before adopting managed environment secrets. The
old host file is neither read nor removed automatically. Registry-only managed
configuration can coexist with the legacy file mount.

## Storage and authorization

- The browser sends credentials over HTTPS directly to Oracle with existing
  operator authentication. CI deploy tokens cannot read or modify credentials.
- Nomad Variables provides authenticated delivery, ACL isolation and encrypted
  storage. Oracle uses its existing CA-verified Nomad HTTPS connection. No
  private SSH key or credential is sent to Vercel or Clerk.
- Operator-only draft records live at `myprod/apps/<name>`, registry connections
  at `myprod/registries`. These paths have no implicit workload access.
- Each apply creates a random immutable version at
  `nomad/jobs/<app>/web/app-<version>`. The matching task identity can read only
  its own version, through Nomad's exact-path workload policy. Job HCL contains
  references and key names, never secret values or registry passwords.
- Application values populate `env_<NAME>` items; registry credentials use
  separate items and are referenced only by Docker's auth configuration. They
  are not injected into application environment variables. Operators and host
  administrators remain trusted; this is not hostile multi-tenant isolation.
- Updates use compare-and-set indexes and draft revisions. API errors are
  generic and do not include upstream credential response bodies. Responses
  use `Cache-Control: no-store`; forms clear values on close/sign-out. Credential
  forms are excluded from dashboard local-storage snapshots.
- Audit logs record action, app/connection and version identifiers, never values.
- Previous immutable variables are retained so old allocations can restart and
  historical job rollback remains possible. They consume encrypted Nomad
  storage. App deletion purges its Myprod-owned drafts and immutable versions
  after stopping the workload. Registry token revocation remains at GitHub.
- Back up Nomad state **and its encryption key material** using the existing
  server backup process. Encryption does not protect against a compromised
  Nomad server/root account. Do not copy decrypted variables into repository
  files, agent output, tickets, or backups of ordinary Myprod configuration.

## Safe rollout

Run `go test ./...`, build Linux ARM64, and back up the current agent binary and
environment. Install atomically and restart **only poolctl-agent** with
`POOLCTL_MANAGED_CREDENTIALS=true`. Do not restart Nomad, Docker, WireGuard or
application allocations. Verify recovery authentication, both new capabilities,
public smoke checks, node readiness and unchanged existing allocation IDs.
Publish the capability-gated dashboard through the configured SSH Git remote.
Either dashboard/agent deployment order is supported.

If local agent health fails, restore the saved binary and environment together.
Do not downgrade to a pre-feature agent and then deploy an app that has opted
into managed credentials: old agents cannot render those references. Existing
running allocations continue independently of an agent rollback. Keep the
feature enabled for ordinary deploys of opted-in apps.

## API

All routes below require operator authentication, except existing image update
routes whose unchanged CI authorization remains app-scoped.

- `GET /apps/<name>/credentials`: metadata only.
- `PUT /apps/<name>/credentials`: `{revision, values, registry}`; values map
  keys to strings or `null` for deletion. Registry is an optional connection
  name; empty string detaches it. Omitted fields preserve existing values.
- `POST /apps/<name>/credentials/apply`: deploy the pending version.
- `POST /apps/<name>/credentials/rollback`: deploy the previous managed version.
- `GET /registries`: connection metadata only.
- `PUT /registries/<name>`: `{host:"ghcr.io", username, password, revision}`.
- `POST /registries/<name>/test`: `{image}`; checks saved GHCR access.
- `DELETE /registries/<name>`: refuses referenced connections.

Paths are relative to `/__poolctl/api`. New records use an empty revision.
Limits: 64 secret keys, 16 KB per value, 48 KB request/applied bundle and 32
registry connections. No NUL values, invalid environment names, `NOMAD_` or
`MYPROD_` names, or overlaps with ordinary app environment variables.

Implementation references:
[Nomad Variables](https://developer.hashicorp.com/nomad/docs/concepts/variables),
[secret blocks](https://developer.hashicorp.com/nomad/docs/job-specification/secret),
[Docker registry authentication](https://developer.hashicorp.com/nomad/docs/job-declare/task-driver/docker#authentication).

GHCR credential setup follows [GitHub's Container registry authentication
guide](https://docs.github.com/en/packages/working-with-a-github-packages-registry/working-with-the-container-registry#authenticating-to-the-container-registry).
Do not save the short-lived Actions `GITHUB_TOKEN` as a reusable registry connection.
