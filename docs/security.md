# Security Notes

This project assumes all VPS machines are public internet hosts, so scheduler and service-discovery ports must never be casually exposed.

## V1 Requirements

- Nomad server/client traffic binds to WireGuard addresses.
- Nomad ACLs are bootstrapped during control-plane setup.
- Nomad mTLS CA is generated during control-plane setup.
- Each node receives its own client certificate during join.
- Public firewall allows only SSH, HTTP, HTTPS, and WireGuard.
- Worker application ports are allowed only on `wg0` from `10.44.0.0/24` and
  are never opened on the public VPS interface.
- Managed app secrets use encrypted Nomad Variables. Legacy root-managed node
  files remain supported; neither is stored in ordinary plaintext app YAML.

## Current Bootstrap Behavior

- The control-plane CA private key is stored at `/etc/nomad.d/tls/nomad-agent-ca-key.pem` with root-only permissions.
- The Nomad bootstrap token is stored at `/var/lib/poolctl/nomad-acl/bootstrap.token` with root-only permissions, outside Nomad's config directory.
- If Nomad ACLs were bootstrapped during an interrupted setup but no local token was saved, the bootstrap script writes Nomad's reported reset index to Nomad's ACL reset file, restarts Nomad, and then stores a fresh management token.
- If that reset-file recovery still fails before `/var/lib/poolctl/control-plane.ready` exists, the bootstrap archives `/opt/nomad/server` to `/opt/nomad/server.bootstrap-recovery.<timestamp>` and creates a fresh single-node Nomad server state. It refuses this archive path after the control plane has been marked ready.
- Nomad runs as root in v1 because the same agent acts as a client and must manage Docker workloads, cgroups, and allocation mounts on the node. Network exposure is still limited by WireGuard binding, TLS, ACLs, and the public firewall.
- Traefik receives a verified Nomad token in `/etc/traefik/traefik.yml`, readable only by root and the `traefik` group.
- Traefik receives only the public Nomad CA certificate, not Nomad private keys.

## SSH

V1 assumes the operator already has SSH access to each VPS. `poolctl` should accept an explicit `--ssh-key` path later. It should not automatically reuse GitHub deploy keys.

## Web Dashboard

`poolctl web` is the SSH-capable local setup surface. It disables auth only for loopback development and requires `POOLCTL_WEB_PASSWORD` when bound to a non-loopback address. Its node scheduling actions execute against Oracle's real Nomad API through the configured operator SSH key.

The hosted dashboard calls the Oracle-local agent and never receives SSH
private keys or the Nomad ACL token. Human operators normally authenticate with
an invite-only Clerk email-OTP session. The browser sends Clerk's short-lived
session JWT cross-origin, and the agent verifies its RSA signature, issuer,
expiry, session ID, exact authorized party, and immutable user-ID allowlist.
The Clerk secret key never reaches Oracle; public JWKS keys are sufficient.
The agent bearer token remains a break-glass recovery credential stored in the
operator browser only after `/status` validates it. Invalid recovery tokens are
removed. Use **Sign out** or **Lock** to clear the active browser authorization.
See [operator-authentication.md](operator-authentication.md).

Powerful actions display specific confirmations describing scheduler or
workload impact. Confirmations are an operator-safety mechanism, not an
authorization boundary. Clerk JWT verification or the recovery agent token,
the exact CORS allowlist, Nomad TLS, and Nomad ACLs enforce access.

Project reservation validates a constrained project ID, refuses the control plane, refuses workers with active allocations, and disables Nomad eligibility before persisting ownership. Release leaves the node frozen so cleanup and scheduler re-entry remain separate decisions.

Hosted application registration accepts only constrained identifiers, registry image references, DNS hostnames, numeric resource limits, exact configured node names, and restricted health-check paths. These values are rendered into Nomad HCL, so newline, quote, shell, and HCL interpolation characters are rejected before persistence. Registration never deploys automatically.

Managed DNS credentials exist only in Oracle's root-readable
`/etc/poolctl-agent.env`. The hosted browser receives a capability flag, zone,
and ingress target, never the Netlify token. DNS automation performs only an
idempotent exact-host A-record create/verify operation. It refuses the zone apex,
hostnames outside the configured zone, and any conflicting A, AAAA, or CNAME
record. Record deletion remains manual.

Ordinary app forms are not secret-entry surfaces. Dedicated operator-only
**Secrets & registry** and **Registry connections** screens are available when
capabilities are enabled. Their API uses encrypted Nomad Variables, immutable
task-scoped credential versions, write-only values, and explicit apply with
health verification and job restoration. CI deploy tokens cannot access them.
See [managed-credentials.md](managed-credentials.md) for authorization, backup,
retention, rotation, rollback and legacy compatibility boundaries.

The legacy fixed `/etc/poolctl/apps/<validated-app-name>.env` bind mount remains
unchanged. The file is owned by `65532:65532`, mode `0400`, and mounted read-only.
No arbitrary host path can be supplied by the app form. Ordinary environment
variables remain bounded and secret-shaped names are rejected.

Repository-triggered image deployments use app-scoped credentials minted by
the authenticated Oracle agent. A token contains 256 bits of randomness, is
returned to the browser once with `Cache-Control: no-store`, and is persisted
only as a SHA-256 digest in the root-readable agent store. SHA-256 is suitable
here because these are uniformly random high-entropy tokens, not human
passwords. Metadata responses expose only token ID, label, creation time,
last-used time, and legacy-import status.

Mint, list, and revoke operations require the full operator token; an
app-scoped token cannot issue credentials. Each token is bound to one app path,
and image updates remain restricted to an immutable SHA-256 digest in that
app's already registered repository. Revocation is live and does not restart
the agent. Deleting an app removes its deploy tokens. The full operator token
must never be copied into project CI.

Legacy plaintext environment tokens are hash-imported exactly once for a
backward-compatible rollout. The token store records that import, so a revoked
legacy token cannot reappear merely because its deprecated environment value
has not yet been removed. Application-consumed secrets remain outside the CI-token issuance surface;
use managed app secrets or the legacy operator-installed file.

The hosted dashboard may retain a sanitized last-successful status snapshot in
browser local storage for locked, read-only visibility. The snapshot is limited
to displayed app configuration, node identity and state, service status, and
resource measurements. It excludes the agent token, SSH usernames, and SSH key
paths. Cached state never enables actions and must be labeled with its capture
time because it is not an authorization or liveness signal.

## Guard Behavior

The guard protects against resource-risk, not exact cloud billing in v1. It can freeze new placements when local thresholds are crossed, but it does not stop running apps automatically.
