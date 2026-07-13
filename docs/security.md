# Security Notes

This project assumes all VPS machines are public internet hosts, so scheduler and service-discovery ports must never be casually exposed.

## V1 Requirements

- Nomad server/client traffic binds to WireGuard addresses.
- Nomad ACLs are bootstrapped during control-plane setup.
- Nomad mTLS CA is generated during control-plane setup.
- Each node receives its own client certificate during join.
- Public firewall allows only SSH, HTTP, HTTPS, and WireGuard.
- App secrets are stored using SOPS + age, not plaintext YAML.

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

The hosted dashboard calls the Oracle-local agent and never receives SSH private keys or the Nomad ACL token. The agent bearer token is stored in the operator browser only after `/status` validates it. Invalid tokens are removed. Use **Lock** to remove a valid token from browser storage.

Powerful actions display specific confirmations describing scheduler or workload impact. Confirmations are an operator-safety mechanism, not an authorization boundary. The agent token, CORS allowlist, Nomad TLS, and Nomad ACLs enforce access.

Project reservation validates a constrained project ID, refuses the control plane, refuses workers with active allocations, and disables Nomad eligibility before persisting ownership. Release leaves the node frozen so cleanup and scheduler re-entry remain separate decisions.

## Guard Behavior

The guard protects against resource-risk, not exact cloud billing in v1. It can freeze new placements when local thresholds are crossed, but it does not stop running apps automatically.
