# Agent Runbook: Add A VPS Worker

This is the source of truth for Codex agents adding a manually created VPS to
the Myprod compute pool.

## Architecture Boundary

The production dashboard at
[`control.sankalpjha.dev`](https://control.sankalpjha.dev/) monitors the
pool and controls already registered nodes through the Oracle-local agent. It
does not create cloud resources or perform the first SSH join.

`https://myprod-control.vercel.app/` is the fallback Vercel alias. Use the
custom production domain for ordinary operation and authentication.

New VPS registration and joining currently run from an SSH-capable checkout of
this repository using the local `poolctl web` operator surface. This is
intentional: private SSH keys stay on the operator machine and are never copied
to Vercel.

## Sources Of Truth

- Repository: <https://github.com/blackdragoon26/Myprod>
- Production dashboard: <https://control.sankalpjha.dev/>
- Vercel fallback: <https://myprod-control.vercel.app/>
- Oracle control plane: `ubuntu@140.245.5.201`, overlay `10.44.0.1`
- Worker guide: [`digitalocean-worker.md`](digitalocean-worker.md)
- Local state: `.poolctl/config.yaml` and `.poolctl/state.yaml` (not committed)

Cloud plans, regional capacity, prices, credits, and account limits change.
Verify them in the provider console at execution time. Never infer a guaranteed
credit duration from a monthly list price.

## 1. Preflight The Repository

From the repository root:

```sh
git status --short --branch
go test ./...
go build -o work/poolctl ./cmd/poolctl
./work/poolctl doctor
./work/poolctl node list
```

Do not overwrite unrelated changes. Back up the local operator state before
changing it:

```sh
cp -R .poolctl "work/poolctl-backup-$(date +%Y%m%d-%H%M%S)"
```

The local operator and hosted Oracle agent currently have separate state files.
Capture Oracle's current copies before making changes:

```sh
mkdir -p work/oracle-agent-before-join
ssh -i ~/.ssh/keys/openclaw-oracle.key ubuntu@140.245.5.201 \
  'sudo cat /opt/poolctl/.poolctl/config.yaml' \
  > work/oracle-agent-before-join/config.yaml
ssh -i ~/.ssh/keys/openclaw-oracle.key ubuntu@140.245.5.201 \
  'sudo cat /opt/poolctl/.poolctl/state.yaml' \
  > work/oracle-agent-before-join/state.yaml
diff -u .poolctl/config.yaml work/oracle-agent-before-join/config.yaml || true
diff -u .poolctl/state.yaml work/oracle-agent-before-join/state.yaml || true
```

Reconcile meaningful differences before continuing. In particular, preserve
newer hosted `frozen`, `draining`, and app deployment state. Do not replace
local files automatically without reviewing the diff.

## 2. Create One Worker

For a general backend worker, start with a shared-CPU Basic/Regular instance
near the control plane or users. The current DigitalOcean baseline is:

```txt
Ubuntu 24.04 LTS x64
4 vCPU / 8 GiB RAM
approximately 160 GiB disk
public IPv4
monitoring enabled
backups disabled for an ephemeral worker
```

Use Premium AMD only when it is available at a sensible price. Use dedicated
CPU only after metrics show sustained CPU contention. Create one worker first;
measure it before adding another.

Record these values without committing secrets:

```txt
node name
provider
public IPv4
SSH user
absolute private-key path on the operator machine
next unused 10.44.0.x overlay address
```

## 3. Restrict The Cloud Firewall

Worker inbound rules:

```txt
22/tcp     operator's current public IPv4/32
51820/udp  Oracle public IPv4 140.245.5.201/32
```

Allow outbound traffic. Do not open worker ports `80`, `443`, `4646`, `4647`,
`4648`, or Docker's API. Oracle remains the only public ingress node.

If the operator's public IP changes, update the SSH rule before retrying. A
single IPv4 address uses `/32`; the slash suffix is the firewall CIDR mask, not
part of the address returned by an IP-check service.

## 4. Prepare The SSH User

Create the `ubuntu` operator user from the provider's initial root session and
copy the authorized key:

```sh
adduser ubuntu
usermod -aG sudo ubuntu
install -d -o ubuntu -g ubuntu -m 0700 /home/ubuntu/.ssh
cp /root/.ssh/authorized_keys /home/ubuntu/.ssh/authorized_keys
chown ubuntu:ubuntu /home/ubuntu/.ssh/authorized_keys
chmod 0600 /home/ubuntu/.ssh/authorized_keys
printf 'ubuntu ALL=(ALL) NOPASSWD:ALL\n' > /etc/sudoers.d/90-poolctl
chmod 0440 /etc/sudoers.d/90-poolctl
visudo -cf /etc/sudoers.d/90-poolctl
```

Passwordless sudo is required because Join is non-interactive. Limit SSH to key
authentication and the operator firewall source before enabling it.

Verify from the operator machine:

```sh
ssh -o BatchMode=yes -i /absolute/path/to/worker.key ubuntu@WORKER_PUBLIC_IP \
  'hostname && sudo -n true && echo sudo-ready'
```

Do not continue until this succeeds without a password prompt.

## 5. Register And Join

Start the local operator dashboard:

```sh
./work/poolctl web --addr 127.0.0.1:8088
```

Open <http://127.0.0.1:8088>. In **Add VPS Node**, enter the recorded values.
Use the absolute SSH-key path and the dashboard's suggested overlay address
unless it conflicts with `./work/poolctl node list`.

Click **Register Node**, then click **Join** on that node. Do not start a second
Join while the first is running. Keep the command output for diagnosis, but
never paste secrets into issues or commits.

## 6. Verify End To End

Local checks:

```sh
./work/poolctl node list
./work/poolctl control-plane status
./work/poolctl doctor
```

Worker checks:

```sh
ssh -i /absolute/path/to/worker.key ubuntu@WORKER_PUBLIC_IP \
  'sudo systemctl is-active docker nomad wg-quick@wg0 && sudo wg show'
```

Public checks:

```sh
curl -fsS https://api.sankalpjha.dev/
curl -fsS https://control.sankalpjha.dev/api/smoke
curl -fsS https://api.sankalpjha.dev/__poolctl/api/health
```

Success requires all of the following:

- worker SSH and `sudo -n` work;
- Docker, Nomad, and `wg-quick@wg0` are active;
- the worker appears as joined in the local pool state;
- Oracle sees the Nomad client over WireGuard;
- HTTP and HTTPS smoke checks return success;
- existing public apps remain reachable.

## 7. Refresh The Hosted Agent Store

The hosted dashboard reads Oracle's agent store at
`/opt/poolctl/.poolctl`. After a successful join, compare the verified local
config and state against the pre-join Oracle copies again. Preserve any hosted
changes that occurred while Join was running. Only after that reconciliation,
copy the merged files to Oracle, install them with restrictive permissions, and
restart only the poolctl agent:

```sh
scp -i ~/.ssh/keys/openclaw-oracle.key \
  .poolctl/config.yaml .poolctl/state.yaml \
  ubuntu@140.245.5.201:/tmp/

ssh -i ~/.ssh/keys/openclaw-oracle.key ubuntu@140.245.5.201 \
  'sudo install -o root -g root -m 0600 /tmp/config.yaml /opt/poolctl/.poolctl/config.yaml &&
   sudo install -o root -g root -m 0600 /tmp/state.yaml /opt/poolctl/.poolctl/state.yaml &&
   sudo systemctl restart poolctl-agent &&
   sudo systemctl is-active poolctl-agent'
```

Sign in to the production dashboard, refresh it, and confirm the new node
appears. Use **Recovery token** only for break-glass access or while Clerk is
being rolled out.
Do not copy the agent token or private SSH keys into the repository.

## 8. Register And Deploy An Application

Use the hosted application flow for ordinary containerized backends. A project reservation is not required and would prevent Nomad from scheduling shared applications on that worker.

Preflight requirements:

- publish an immutable public container image for the target architecture;
- make the service listen on `0.0.0.0` and record its container port;
- expose an unauthenticated health endpoint that returns HTTP 2xx;
- either configure Netlify automation on Oracle or point the chosen application
  hostname to Oracle public IP `140.245.5.201` externally;
- choose an exact target node with enough available CPU, memory, and disk;
- verify the target is joined, eligible, not draining, and not reserved.

From the hosted dashboard:

1. Sign in with the invited Clerk email and refresh live state. If Clerk is
   unavailable, use **Recovery token** with the Oracle agent token.
2. Review **Resource Utilization**. CPU, memory, and root disk are actual host measurements from Nomad client stats.
3. Select **Add application** under **Managed Apps**.
4. Enter the app name, public image, domain, target node, container port, CPU reservation, memory reservation, and health path.
5. Keep **Create and verify DNS automatically** enabled for a hostname in the configured Netlify zone. Disable it only when DNS is managed externally.
6. Select **Register application** and confirm the external DNS mutation when prompted. Registration persists configuration and may create the A record, but it does not start the container.
7. Confirm the application appears with status `configured`; review its target, reservations, and DNS state.
8. If DNS is `pending`, wait for propagation and select **Check DNS**. Stop on `conflict`; Myprod will not overwrite the existing record.
9. When DNS is `ready` or `manual`, select **Deploy**, read the live-workload warning, and confirm.
10. Wait for Agent Output to report a healthy Nomad allocation on the selected node.
11. Refresh and verify the app status is `deployed`, then test its public HTTPS URL.

Do not enter secrets or private-registry credentials in the dashboard. The
dashboard accepts public images and internal images whose read-only pull
credential was already installed by an operator on the target node; it never
collects registry credentials itself. For an app that explicitly loads
`/run/secrets/cutable.env`, an operator may first install the fixed
`/etc/poolctl/apps/<app-name>.env` file on the exact target node as
`65532:65532` with mode `0400`, then enable the runtime-environment mount.
Plain non-secret environment variables can also be stored and rendered into
the Nomad task; secret-shaped names are rejected. Persistent volumes remain
outside the hosted app contract.

Use **Edit** to correct an app's domain, image, target, resources, health path,
DNS mode, or non-secret environment. Saving marks the app configured; deploy it
separately. Use **Delete** only after confirmation. A deployed app is stopped
and purged from Nomad before its configuration is removed. Myprod deliberately
preserves managed DNS records for separate review.

### Generic immutable image deployments

Every registered application can update and deploy its image without a Myprod
code change:

```txt
POST /__poolctl/api/apps/{name}/image
Authorization: Bearer <app-scoped deploy token or POOLCTL_AGENT_TOKEN>
{"image":"ghcr.io/org/repository@sha256:<64 lowercase hex>"}
```

The new image must use the application's existing repository and an immutable
SHA-256 digest. Myprod renders and submits that candidate, waits for a healthy
allocation on the configured node, and only then persists the new digest. A
failed deployment leaves the stored image unchanged. Failure output includes
`nomad eval status` and available `nomad alloc status` evidence.

For repository CI, open the app's **CI tokens** control in the hosted dashboard,
enter a label such as `GitHub Actions`, and select **Generate deploy token**.
Copy the plaintext immediately into the repository's masked Actions secret; it
is returned once and cannot be recovered. Myprod stores only its SHA-256 digest
in `/opt/poolctl/.poolctl/deploy-tokens.json`, with mode `0600`. The token
authorizes only the matching app path, and the stored image repository is its
automatic allowlist. Do not give a source repository the dashboard-wide
operator token.

The same control lists creation and last-used times and supports immediate
revocation without restarting the agent. Multiple tokens may coexist during a
safe CI rotation. Token plaintext must never be pasted into Agent Output,
application fields, issues, or Git.

On the first agent version supporting `appDeployTokensV1`, existing
`POOLCTL_APP_DEPLOY_TOKENS_JSON`, `POOLCTL_P4LENS_DEPLOY_TOKEN`, and
`POOLCTL_CUTABLE_DEPLOY_TOKEN` values are hash-imported exactly once. Existing
CI therefore continues to work. After minting and testing replacement tokens,
revoke the imported credentials, remove the deprecated environment variables,
and perform one final controlled `poolctl-agent` restart. Revoked legacy values
are not re-imported.

The older P4Lens repository integration remains available for compatibility:

```txt
POST /__poolctl/api/deploy/p4lens
Authorization: Bearer <POOLCTL_P4LENS_DEPLOY_TOKEN>
{"image":"ghcr.io/openlabnetworks/p4lens-backend@sha256:<64 lowercase hex>"}
```

That compatibility endpoint cannot deploy another application, tag, registry,
or mutable image reference. New P4Lens CI should use a dashboard-issued token.
The environment credential remains only as a hash-imported migration path;
never reuse the dashboard operator token.

Cutable uses the same verified deployment path with an independent token and
an independently constrained application/image pair:

```txt
POST /__poolctl/api/deploy/cutable
Authorization: Bearer <POOLCTL_CUTABLE_DEPLOY_TOKEN>
{"image":"ghcr.io/blackdragoon26/cutable-api@sha256:<64 lowercase hex>"}
```

New Cutable CI should use a dashboard-issued token. A successful request
updates the stored digest only after the `cutable-api` allocation is healthy,
so subsequent dashboard actions and automatic deployments use the same
verified artifact.

Internal GHCR images also require a read-only registry credential configured
on every eligible target node through Nomad's Docker driver. Do not put that
credential in the application record, Nomad job, dashboard, or repository.

Registration uses exact-node placement. This constrains one app without reserving the entire worker, so unrelated Nomad applications can share remaining capacity.

Netlify credential installation and recovery procedures are in
[netlify-dns.md](netlify-dns.md). Never paste the Netlify token into the
dashboard, application fields, repository, or agent output.

## 9. Reserve A Worker For A Project

Use a reservation when one project needs the entire machine, especially for host-level package installation, network namespaces, kernel modules, or other privileged work.

Preflight requirements:

- the target must be a worker, never the control plane;
- the worker must be joined and visible in Nomad;
- the worker must have no active allocations;
- the project ID may contain only letters, numbers, dash, and underscore.

From the hosted dashboard:

1. Unlock with the Oracle agent token.
2. Refresh and inspect the worker's current state.
3. Select **Reserve** and enter the project ID.
4. Read the confirmation: reservation disables real Nomad scheduling for the whole worker.
5. Confirm and wait for the action output.
6. Refresh and verify both `reserved` and `project: <id>` appear.

The reservation is now the infrastructure boundary. Continue the project work by SSHing directly to the reserved worker and installing inside that machine. Myprod will not schedule shared Nomad workloads there while the reservation remains active:

```sh
ssh -i ~/.ssh/keys/openclaw-oracle.key ubuntu@140.245.228.146
```

Do not run project installation commands on `oracle-main`; it remains the shared control plane and ingress host.

### System-wide installation boundary

System-wide installation on a reserved worker is allowed when the project requires it. Installing Ubuntu packages, compilers, P4 toolchains, libraries, containers, and project-owned systemd services does not affect `oracle-main` or workloads on other workers.

Prefer project-owned paths such as `/opt/<project>` and `/srv/<project>` where the installer permits it. Before a large or invasive toolchain installation, create a scoped checkpoint and obtain approval for an Oracle boot-volume backup when image-level rollback is required.

Do not modify or remove these Myprod lifelines unless the worker is intentionally being rebuilt:

- the `ubuntu` user's SSH key and `sshd` configuration;
- Oracle security-list access to TCP 22;
- WireGuard interface `wg0`, its routes, or UDP 51820;
- the Nomad client configuration and service;
- Docker, while existing project tooling depends on it;
- the host default route, netplan configuration, bootloader, or root filesystem layout.

Also monitor free disk space and memory during large builds. A reservation prevents shared scheduling on the worker, but it cannot prevent a system-wide installer from breaking that worker itself. If the worker becomes unrecoverable, rebuild only the reserved worker; do not troubleshoot project packages on `oracle-main`.

Verify independently on Oracle:

```sh
ssh -i ~/.ssh/keys/openclaw-oracle.key ubuntu@140.245.5.201 \
  'token="$(sudo awk "NF {print; exit}" /var/lib/poolctl/nomad-acl/bootstrap.token)"; \
   sudo env NOMAD_ADDR=https://10.44.0.1:4646 \
     NOMAD_CACERT=/etc/nomad.d/tls/nomad-agent-ca.pem \
     NOMAD_TOKEN="$token" nomad node status'
```

The reserved node must show `ineligible`. Oracle's `/opt/poolctl/.poolctl/state.yaml` must contain the matching `reserved_for` value.

**Release** clears project ownership but intentionally keeps the node ineligible. Inspect and clean the worker first, then use the separately confirmed **Unfreeze** action to return it to the shared scheduler.

## 10. Provision A Sandbox Partition

Use a sandbox when an LLM or automation agent needs a real Ubuntu ARM64 shell
for building, testing, or reproduction, and does not need a whole machine. Read
[llm-sandbox.md](llm-sandbox.md) first; it is the full contract.

Preflight requirements:

- the target must be a worker, never the control plane;
- the worker must be joined, eligible, not draining, and not reserved;
- the worker must be enrolled as a sandbox host, within an explicit budget;
- public egress additionally requires the host isolation bundle.

Enable a host once:

```sh
go run ./cmd/poolctl sandbox render-isolation
scp -i ~/.ssh/keys/openclaw-oracle.key -r work/rendered/sandbox ubuntu@140.245.228.146:~/
ssh -i ~/.ssh/keys/openclaw-oracle.key ubuntu@140.245.228.146 '~/sandbox/sandbox-isolation.sh --verify'
ssh -i ~/.ssh/keys/openclaw-oracle.key ubuntu@140.245.228.146 '~/sandbox/sandbox-isolation.sh --apply'
ssh -i ~/.ssh/keys/openclaw-oracle.key ubuntu@140.245.228.146 '~/sandbox/sandbox-isolation.sh --verify'
```

Then run the section 6 end-to-end verification for that worker before
enrolling it: SSH, passwordless `sudo`, WireGuard, Nomad registration, the
production agent health route, and both public smoke checks.

Then enroll it through the operator-authenticated action endpoint with
`sandbox-host-enroll` and a value such as `egress,max=2,cpu=1000,mem=1024`.
Omit `egress` for loopback-only sandboxes. `sandbox-host-remove` withdraws the
enrollment and is refused while a sandbox is still live.

Create and use sandboxes from the hosted dashboard under **Sandbox
Partitions**, or through `POST /__poolctl/api/sandboxes`. The scoped session
token is displayed once; it authorizes only status, exec, logs, and destroy on
that one sandbox.

Destroy sandboxes when the work is done rather than waiting for the TTL, and
confirm afterwards that every managed application is still healthy. Never
weaken sandbox confinement, enroll the control plane, or grant egress on a node
whose isolation bundle has not been verified.

## 11. Action Semantics

- **Control status** reads real systemd service state.
- **Deploy** runs a real Nomad job submission and status verification.
- **Freeze** changes real Nomad scheduling eligibility to ineligible.
- **Unfreeze** changes eligibility to eligible and is refused for reserved or draining nodes.
- **Drain** starts a real detached Nomad drain.
- **Cancel drain** stops the drain but keeps the node ineligible.
- **Reserve** requires an empty worker and records exclusive project ownership.
- **Release** clears ownership but does not silently make the node schedulable.
- **sandbox-host-enroll** records a worker's sandbox budget and egress policy; it never changes Nomad state.
- **sandbox-host-remove** withdraws that enrollment and is refused while a sandbox is live.
- **New sandbox** submits a real, hardened Nomad batch job and returns a one-time scoped token.
- **Destroy sandbox** purges only jobs named `poolctl-sbx-*`; it can never stop a managed application.

Do not infer success from the dashboard label alone. Read command output and verify Nomad after powerful operations.

## Efficiency Rules

- Keep centralized ingress on Oracle; workers spend resources on app workloads.
- Prefer one right-sized shared-CPU worker over several idle workers.
- Put persistent data outside ephemeral credit-backed workers.
- Enable provider monitoring and resize only from observed CPU, memory, disk,
  and load trends.
- Drain before resize or deletion. Confirm Nomad allocations moved and public
  smoke checks pass before removing the provider resource.
- Credits are applied to eligible hourly usage until exhausted or expired;
  add-ons and taxes may be treated differently by the provider.
