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
- The Nomad bootstrap token is stored at `/etc/nomad.d/acl/bootstrap.token` with root-only permissions.
- Nomad runs as root in v1 because the same agent acts as a client and must manage Docker workloads, cgroups, and allocation mounts on the node. Network exposure is still limited by WireGuard binding, TLS, ACLs, and the public firewall.
- Traefik receives a Nomad token through `/etc/traefik/traefik.env`, readable only by the `traefik` user.
- Traefik receives only the public Nomad CA certificate, not Nomad private keys.

## SSH

V1 assumes the operator already has SSH access to each VPS. `poolctl` should accept an explicit `--ssh-key` path later. It should not automatically reuse GitHub deploy keys.

## Guard Behavior

The guard protects against resource-risk, not exact cloud billing in v1. It can freeze new placements when local thresholds are crossed, but it does not stop running apps automatically.
