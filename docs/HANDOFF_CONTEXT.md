# Myprod / poolctl Handoff Context

This document is the full working context for continuing this project in a new Codex task.

Use it as the first message/context paste if the current chat becomes too messy:

```txt
Read docs/HANDOFF_CONTEXT.md first. Continue the Myprod/poolctl infrastructure project from the current repo state. Do not restart the project. The app stack is already running inside Oracle; the current blocker is public ingress from the internet to Oracle public IP 140.245.5.201 on TCP 80/443.
```

## User Goal

The user wants a free/near-free personal compute pool for hosting many personal backend services and websites.

Core goals:

- Use Oracle OCI PAYG/free-tier ARM instance as the stable control-plane and default compute node.
- Later add temporary free-credit VPS providers like DigitalOcean, AWS, etc.
- Let manually-created cloud servers join the system and become usable compute.
- Allow removing temporary nodes when credits expire.
- Keep infra overhead low so most RAM/CPU remains available for user apps.
- Learn DevOps/Kubernetes-like ideas, but the production system should stay lean.
- Build this as a real GitHub portfolio project named `Myprod`.

Important user preference:

- This is for personal projects, not enterprise.
- DB usage is expected to be small.
- Free-cost tooling is mandatory.
- Keep infrastructure as bare/simple as possible, but still secure enough for public VPSs.

## Project Identity

Repo path:

```txt
/Users/sankalpjha/Documents/Codex/2026-07-08/okay-so/Myprod
```

GitHub repo:

```txt
https://github.com/blackdragoon26/Myprod
```

CLI name:

```txt
poolctl
```

Language:

```txt
Go
```

Current branch:

```txt
main
```

Current latest local commit:

```txt
21d43b9 Add interface ingress smoke checks
```

Note: Some commits may not have pushed to GitHub because this Codex environment repeatedly failed DNS/network access to GitHub. Local repo is the source of truth.

## Architecture Chosen

V1 architecture:

```txt
Cloudflare DNS -> Oracle Traefik ingress -> Nomad apps on Oracle/workers over WireGuard
```

Chosen stack:

- Go CLI: `poolctl`
- Scheduler: Nomad
- Ingress: Traefik
- Overlay/private network: WireGuard
- DNS target: Cloudflare later
- Secrets target: SOPS + age later
- Monitoring/resource guard: local `poolctl guard`

Deliberately excluded from V1:

- Kubernetes/k3s for production pool
- Consul
- Provider API provisioning
- Per-node public ingress
- Restic automation
- DB failover
- Cloud billing API automation

Reasoning:

- Nomad is lighter than k8s/k3s for tiny mixed VPS nodes.
- Traefik can read Nomad services directly.
- Consul would double ACL/TLS/daemon complexity for little V1 benefit.
- Central Oracle ingress is simpler than per-node ingress and DNS changes.

Tradeoff:

- Oracle remains the public bandwidth/CPU ingress bottleneck.
- This is okay for personal-project scale.

## Oracle Control Plane

Oracle instance details from the user:

```txt
ssh: ubuntu@140.245.5.201
ssh key: ~/.ssh/keys/openclaw-oracle.key
OS: Ubuntu 22.04.5 LTS
arch: aarch64 / ARM64
public IP: 140.245.5.201
OCI private IP: 10.0.0.237
WireGuard overlay IP: 10.44.0.1
disk: about 193 GB
RAM: about 24 GB
```

Current public ingress expectation:

```sh
curl -H 'Host: sample-api.pool.test' http://140.245.5.201/
```

Current state:

- Works inside the VM through Traefik.
- Fails from outside with `curl: (7) Failed to connect to 140.245.5.201 port 80`.
- Therefore current blocker is public network path, probably OCI Security List / NSG / VNIC attachment mismatch, not Nomad, Docker, or Traefik.

## Implemented Commands

Current public CLI surface:

```txt
poolctl init
poolctl render
poolctl bootstrap-control-plane --dry-run
poolctl bootstrap-control-plane --apply
poolctl doctor
poolctl control-plane status
poolctl node list
poolctl node freeze <node>
poolctl node unfreeze <node>
poolctl node drain <node>
poolctl app render <app>
poolctl app deploy <app>
poolctl app status <app>
poolctl guard check
```

Important implemented behavior:

- `init` creates `.poolctl/config.yaml` and `.poolctl/state.yaml`.
- `render` writes generated files to `work/rendered`.
- `bootstrap-control-plane --apply` renders bundle, copies it over SSH, runs bootstrap on Oracle.
- `control-plane status` SSHs to Oracle and prints systemd, listeners, UFW, Nomad jobs, Nomad service API auth, Traefik config status, local/private ingress smoke, and logs.
- `app deploy sample-api` renders a Nomad job, copies it to Oracle, and runs `nomad job run`.
- `guard check` is currently local scaffold behavior.

## Important Files

```txt
cmd/poolctl/main.go                 CLI entry
internal/cli/cli.go                 command dispatch
internal/pool/model.go              config/state models
internal/pool/store.go              config/state store and defaults
internal/pool/render.go             generated bootstrap, Nomad, Traefik, systemd, WireGuard, jobs
internal/pool/remote.go             SSH/SCP remote apply, deploy, status
internal/pool/guard.go              guard logic
internal/pool/print.go              local status print helpers
internal/pool/store_test.go         current tests
docs/architecture.md                architecture notes
docs/oracle-control-plane.md        Oracle notes
docs/security.md                    security notes
README.md                           user-facing overview
```

Local ignored config:

```txt
.poolctl/config.yaml
.poolctl/state.yaml
```

Generated output:

```txt
work/rendered/bootstrap-control-plane.sh
work/rendered/nomad/server.hcl
work/rendered/systemd/nomad.service
work/rendered/traefik/traefik.yml
work/rendered/systemd/traefik.service
work/rendered/wireguard/wg0.conf
work/rendered/systemd/poolctl-guard.service
work/rendered/systemd/poolctl-guard.timer
work/rendered/nomad/jobs/sample-api.nomad.hcl
work/rendered/poolctl
```

## Default Config / Sample App

Default config contains:

```yaml
node:
  name: oracle-main
  role: control-plane
  provider: oracle
  public_ip: 140.245.5.201
  private_ip: 10.0.0.237
  overlay_ip: 10.44.0.1
  ssh_user: ubuntu
  ssh_key: ~/.ssh/keys/openclaw-oracle.key

app:
  name: sample-api
  image: traefik/whoami:v1.11.0
  domain: sample-api.pool.test
  port: 80
  placement:
    prefer_node: oracle-main
    allow_workers: false
```

Sample app purpose:

- Prove Docker + Nomad + service discovery + Traefik routing works.
- It is not the final real backend app.

## Current Verified Working State

The last useful user output showed:

Nomad job deploy:

```txt
sample-api deployment completed successfully
web Desired=1 Placed=1 Healthy=1 Unhealthy=0
```

`control-plane status` showed:

```txt
systemd: active,active,active
listeners:
10.44.0.1:4646/4647/4648 listening by nomad
*:80 and *:443 listening by traefik

nomad nodes:
oracle-main ready

nomad jobs:
sample-api running

nomad services api:
token: present
GET /v1/services -> 200

traefik config:
token rendered into /etc/traefik/traefik.yml

local ingress smoke:
Hostname: 963a82d7038c
Host: sample-api.pool.test
...
```

Meaning:

- Nomad server is running.
- Nomad client is running.
- Docker task driver works.
- Sample app container runs.
- Nomad service discovery works.
- Traefik can read Nomad API.
- Traefik routing works locally from inside VM.
- UFW allows 80/443/51820/22.

Current failing command:

```sh
curl -H 'Host: sample-api.pool.test' http://140.245.5.201/
```

Failure:

```txt
curl: (7) Failed to connect to 140.245.5.201 port 80
```

Interpretation:

- Since local ingress works and Traefik listens on `*:80`, the remaining failure is public path to the VM.
- Most likely OCI network config mismatch:
  - TCP 80/443 opened on wrong security list
  - instance attached to an NSG that still blocks TCP 80/443
  - route/subnet/VNIC mismatch
  - public IP association oddity
  - less likely: provider edge delay or stateful firewall issue

## Current Next Step

Run:

```sh
cd /Users/sankalpjha/Documents/Codex/2026-07-08/okay-so/Myprod

go build -a -o work/poolctl ./cmd/poolctl
./work/poolctl control-plane status
```

Inspect:

```txt
private-interface ingress smoke:
```

Expected:

- If private-interface smoke returns `whoami`, VM accepts traffic on its private VNIC. Public failure is definitely OCI Security List / NSG / public ingress config.
- If private-interface smoke fails, inspect Ubuntu/UFW/interface binding despite Traefik showing `*:80`.

Do not redeploy `sample-api` unless it stops running.

## OCI UI State Already Seen

The user showed screenshots:

Route table:

```txt
0.0.0.0/0 -> Internet Gateway main-vnic
```

Security List screenshot eventually showed ingress rules:

```txt
TCP 22 from 0.0.0.0/0
TCP 80 from 0.0.0.0/0
TCP 443 from 0.0.0.0/0
ICMP rules
egress all traffic
```

Stateless should remain unchecked/off for these rules.

Important caveat:

- OCI can also use NSGs attached directly to the VNIC. If an NSG exists, its ingress rules must allow TCP 80/443 too.
- Also verify the Security List being edited belongs to the exact subnet used by instance `main-vnic`.

## Major Debugging History

This section is important because many fixes are already in the codebase. Do not redo from scratch.

### 1. Missing Go Module Context

User initially ran commands outside repo and saw:

```txt
go: go.mod file not found
```

Resolution:

Use:

```sh
cd /Users/sankalpjha/Documents/Codex/2026-07-08/okay-so/Myprod
```

### 2. Nomad TLS CA Permission

Initial bootstrap error:

```txt
Error loading CA File: open /etc/nomad.d/tls/nomad-agent-ca.pem: permission denied
```

Fix:

- Adjusted permissions/ownership so Nomad CLI/system services can read the CA.
- Nomad service eventually runs as root because the same agent is server+client and must manage Docker/cgroups/alloc mounts.

### 3. Nomad Not Ready / Connection Refused

Error:

```txt
Put "https://10.44.0.1:4646/v1/acl/bootstrap": dial tcp 10.44.0.1:4646: connect: connection refused
```

Fix:

- Added readiness polling and HTTP leader fallback.

### 4. ACL Bootstrap Already Done Without Token

Repeated errors:

```txt
ACL bootstrap already done (reset index: N)
Nomad ACL is already bootstrapped but no local token was found
```

Fixes added over multiple commits:

- Store bootstrap token outside Nomad config directory:
  `/var/lib/poolctl/nomad-acl/bootstrap.token`
- Migrate older token/json files.
- Use Nomad ACL reset file recovery.
- If pre-ready and recovery fails, archive interrupted Nomad server state.
- Archive empty/invalid token files.
- Verify existing tokens against `/v1/services`.

### 5. Token Parser Failed

Nomad returned one-line JSON:

```json
{"ExpirationTTL":"","AccessorID":"...","SecretID":"615312d1-...","Name":"Bootstrap Token","Type":"management",...}
```

Old parser failed because it assumed line shape incorrectly.

Fix:

- Parse `SecretID` with `sed` that works on one-line JSON.

### 6. Traefik Token Placeholder / systemd `%`

Initial Traefik logs:

```txt
Unexpected response code: 403 (Permission denied)
```

Root cause:

- Generated config had `%` token placeholder.
- systemd treats `%` specially.
- Traefik ended up with invalid token.

Fix:

- Switched to systemd-safe placeholder.
- Later simplified further: bootstrap writes final `/etc/traefik/traefik.yml` directly with verified token.

### 7. Traefik Config Token Delivery

Better final approach:

- Bootstrap validates Nomad token against `/v1/services`.
- Bootstrap renders `/etc/traefik/traefik.yml` with actual token.
- File permissions:
  `root:traefik`, `0640`.
- Traefik service reads `/etc/traefik/traefik.yml` directly.

### 8. Stale Traefik CA Errors

Old logs:

```txt
x509: certificate signed by unknown authority
crypto/rsa: verification error
```

Cause:

- Old Traefik process/config was still trying to use stale CA/token while bootstrap rotated TLS/ACL.

Fix:

- Stop Traefik before restarting/reconfiguring Nomad during bootstrap.

### 9. `sample-api` Pending

At one point `sample-api` was pending.

Fix:

- Redeploy sample app after Nomad ACL/TLS/Traefik recovered:

```sh
./work/poolctl app deploy sample-api
```

Current result:

- `sample-api` now runs and is locally reachable through Traefik.

### 10. Public Curl Still Fails

Current final blocker:

```txt
curl: (7) Failed to connect to 140.245.5.201 port 80
```

Since local ingress works:

- Do not debug Nomad/Docker/Traefik first.
- Debug OCI public ingress path.

## Recent Commit Timeline

Important recent commits:

```txt
21d43b9 Add interface ingress smoke checks
479c273 Show Nomad app diagnostics in status
e5a35d5 Pass verified Nomad token to Traefik render
d5baaf2 Parse Nomad bootstrap token reliably
45c4e32 Recover empty Nomad ACL tokens
923e7ea Render Traefik config with verified Nomad token
b290f37 Fix Traefik Nomad token rendering
2791e7d Add ingress diagnostics to status
73d8347 Document OCI public ingress requirement
958d71e Fix app deploy remote path
e63b55c Make sample app deployable
ff8636f Allow missing Nomad token during bootstrap
0876f6f Recover interrupted first Nomad bootstrap
c2cf30c Harden Nomad ACL recovery diagnostics
9cdb397 Use Nomad ACL reset file recovery
3e0caa5 Add app deploy operator flow
1c7ad3a Recover missing Nomad bootstrap token
0e3edf6 Tolerate Nomad readiness startup delay
68fbdf4 Make Nomad ACL bootstrap idempotent
e37acd1 Run Nomad agent with client privileges
```

## How To Build/Test Locally

Use repo path:

```sh
cd /Users/sankalpjha/Documents/Codex/2026-07-08/okay-so/Myprod
```

Recommended local build/test:

```sh
mkdir -p work/go-cache work/go-mod work/tmp
env GOCACHE="$PWD/work/go-cache" \
  GOMODCACHE="$PWD/work/go-mod" \
  GOTMPDIR="$PWD/work/tmp" \
  go test ./...

env GOCACHE="$PWD/work/go-cache" \
  GOMODCACHE="$PWD/work/go-mod" \
  GOTMPDIR="$PWD/work/tmp" \
  go build -a -o work/poolctl ./cmd/poolctl
```

User usually runs simpler:

```sh
go build -a -o work/poolctl ./cmd/poolctl
```

## If New Codex Has SSH/Network Access

If a future Codex task has external terminal/network access, it can run:

```sh
cd /Users/sankalpjha/Documents/Codex/2026-07-08/okay-so/Myprod

go build -a -o work/poolctl ./cmd/poolctl
./work/poolctl control-plane status
```

Direct SSH checks:

```sh
ssh -i ~/.ssh/keys/openclaw-oracle.key ubuntu@140.245.5.201 \
  "sudo ss -ltnp | grep -E ':(80|443|4646|4647|4648)\\b' || true; sudo ufw status verbose"
```

Inside-VM smoke:

```sh
ssh -i ~/.ssh/keys/openclaw-oracle.key ubuntu@140.245.5.201 \
  "curl -v -H 'Host: sample-api.pool.test' http://127.0.0.1/"
```

Private interface smoke:

```sh
ssh -i ~/.ssh/keys/openclaw-oracle.key ubuntu@140.245.5.201 \
  "curl -v -H 'Host: sample-api.pool.test' http://10.0.0.237/"
```

Public smoke:

```sh
curl -v -H 'Host: sample-api.pool.test' http://140.245.5.201/
```

If direct SSH works from a less restricted Codex, use it to inspect OCI-facing network state faster.

## Current Sandbox Limitation

The current Codex session cannot SSH to Oracle because network access is restricted. It can:

- edit repo files
- run local tests/builds
- inspect pasted terminal output
- commit locally

It cannot:

- run remote SSH commands itself
- verify public network from all locations reliably
- push consistently to GitHub because DNS/network failed repeatedly

## What To Tell The User

The user is frustrated. Be direct and reassuring:

- The core app stack is working.
- We are not starting over.
- The current problem is narrowed to Oracle public ingress.
- No more Nomad/Traefik redeploys are needed unless status regresses.
- Ask for `control-plane status`, especially `private-interface ingress smoke`.

Avoid vague “wait more” advice. The public curl is immediate connection refused, so this is not normal propagation delay.

## Next Engineering Tasks After Public Ingress

Once public curl works:

1. Add HTTPS path:
   - real domain in Cloudflare
   - Traefik ACME or Cloudflare origin setup
2. Add `poolctl dns sync`.
3. Add worker join flow:
   - generate WireGuard keys
   - assign overlay IP
   - install Nomad client
   - add peer to Oracle
4. Add app move/drain behavior.
5. Add SOPS + age env injection.
6. Add real guard thresholds and remote guard status.
7. Make README portfolio-ready with architecture diagram and demo commands.

## Current Quick Summary

As of latest work:

```txt
Repo: Myprod
CLI: poolctl
Oracle bootstrap: works
Nomad: active
Traefik: active
WireGuard: active
Docker sample app: running
Traefik local route: works
Nomad token/ACL: recovered and works
Current blocker: public internet cannot connect to 140.245.5.201:80
Likely fix: OCI NSG/security-list/VNIC/subnet public ingress check
```

