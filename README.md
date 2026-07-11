# poolctl

`poolctl` is an experimental CLI for turning cheap/free VPS machines into a small personal compute pool for backend services.

The project is designed around a practical v1:

```txt
DNS -> Oracle Traefik ingress -> Nomad apps on Oracle/workers over WireGuard
```

The first target is personal projects, not enterprise high availability.

## Goals

- Bring any manually-created VPS into the pool after SSH access exists.
- Prefer a stable Oracle OCI free-tier node as the control plane.
- Use temporary credit-based servers as workers that can be drained later.
- Keep infra overhead low enough that most compute remains available for apps.
- Make the repo readable as a DevOps portfolio project.

## V1 Stack

- Go CLI: `poolctl`
- Scheduler: Nomad
- Ingress: Traefik on Oracle
- Private networking: WireGuard
- DNS: Netlify DNS for the current `sankalpjha.dev` domain; Cloudflare sync can be added later
- Secrets: SOPS + age encrypted env files

Consul, provider API provisioning, per-node ingress, automatic database failover, and backup automation are intentionally out of v1.

## Current Commands

Local commands:

```sh
go run ./cmd/poolctl init
go run ./cmd/poolctl render
go run ./cmd/poolctl bootstrap-control-plane --dry-run
go run ./cmd/poolctl bootstrap-control-plane --apply
go run ./cmd/poolctl doctor
go run ./cmd/poolctl control-plane status
go run ./cmd/poolctl node list
go run ./cmd/poolctl node freeze oracle-main
go run ./cmd/poolctl app status sample-api
go run ./cmd/poolctl app render sample-api
go run ./cmd/poolctl app deploy sample-api
go run ./cmd/poolctl guard check
go run ./cmd/poolctl web
```

The default `sample-api` uses `traefik/whoami` so the first deployment can prove the control plane works before you bring a real backend image. After deploying it, smoke-test centralized ingress with:

```sh
curl https://api.sankalpjha.dev/
```

If that curl times out while `poolctl control-plane status` shows Nomad/Traefik active and the job is healthy, verify `api.sankalpjha.dev` still points to `140.245.5.201`, then inspect the Oracle VCN security list or NSG attached to the instance. If the error is immediate `Connection refused` while Traefik is listening on `*:80` or `*:443`, inspect Oracle's host `iptables` INPUT chain; some images reject public traffic before UFW's allow rules run. The generated bootstrap now inserts persistent `poolctl-ingress-http` and `poolctl-ingress-https` rules ahead of that reject.

To include the guard binary in the Oracle bootstrap bundle:

```sh
go build -o work/poolctl ./cmd/poolctl
go run ./cmd/poolctl bootstrap-control-plane --dry-run
```

## Web Dashboard

`poolctl web` starts a small operator dashboard powered by the same CLI command surface:

```sh
go build -o work/poolctl ./cmd/poolctl
./work/poolctl web --addr 127.0.0.1:8088
```

The dashboard reads `.poolctl/config.yaml` and `.poolctl/state.yaml`, runs HTTP/HTTPS smoke checks for configured apps, and exposes buttons for:

- control-plane status
- app render/deploy
- node freeze/unfreeze/drain
- guard check
- bundle render

For local use, auth is disabled when bound to loopback. If binding to a public interface, set an admin password first:

```sh
POOLCTL_WEB_PASSWORD='change-me' ./work/poolctl web --addr 0.0.0.0:8088
```

Do not expose the dashboard on `admin.sankalpjha.dev` until the backend is running in a deployment-safe mode. The current dashboard controls Oracle through the existing SSH-based CLI flow, which is perfect from this repo on your Mac; the hosted version should use Oracle-local Nomad/systemd operations instead of copying a private SSH key to the server.

## Roadmap

1. Local config/state and CLI behavior.
2. Nomad job generation.
3. Traefik provider configuration.
4. SSH bootstrap for Oracle control plane.
5. Deploy first app through Nomad and Traefik.
6. SSH bootstrap for worker nodes.
7. DNS sync for the active DNS provider.
8. SOPS/age secret injection.
9. Guard systemd timer.
10. WireGuard key/IP lifecycle for joined workers.

## Current Oracle Target

The default sample config is prefilled for the first Oracle control-plane VM:

```txt
user: ubuntu
public IP: 140.245.5.201
private OCI IP: 10.0.0.237
overlay IP: 10.44.0.1
ssh key: ~/.ssh/keys/openclaw-oracle.key
OS: Ubuntu 22.04.5 LTS aarch64
disk: ~193 GB
memory: ~24 GB
```

Run this locally first:

```sh
go run ./cmd/poolctl bootstrap-control-plane --dry-run
```

That command only writes reviewable files under `work/rendered/`; it does not SSH into the server yet.

After review, this command copies the bundle to Oracle and runs the bootstrap script:

```sh
go run ./cmd/poolctl bootstrap-control-plane --apply
```

The generated bootstrap currently installs:

- Docker through Docker's convenience installer
- Nomad `2.0.4` from HashiCorp release archives with checksum verification
- Traefik `3.7.7` from GitHub release archives with checksum verification
- WireGuard `wg0` at `10.44.0.1/24`
- Nomad TLS and initial ACL bootstrap token
- Traefik Let's Encrypt HTTP-challenge certificates stored at `/var/lib/traefik/acme.json`
- host firewall rules for SSH, HTTP, HTTPS, and WireGuard ingress
- systemd units for Nomad, Traefik, and the optional `poolctl` guard timer

Running `work/rendered/bootstrap-control-plane.sh` on Oracle will mutate the server.

## Safety Defaults

- Nomad should bind to WireGuard/private addresses, not public interfaces.
- Only SSH, HTTP, HTTPS, and WireGuard should be public.
- Free-tier guard freezes new placements instead of killing live apps.
- Draining a node marks it frozen first, then prepares workloads to move away.
