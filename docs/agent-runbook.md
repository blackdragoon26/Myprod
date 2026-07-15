# Agent Runbook: Add A VPS Worker

This is the source of truth for Codex agents adding a manually created VPS to
the Myprod compute pool.

## Architecture Boundary

The production dashboard at
[`myprod-control.vercel.app`](https://myprod-control.vercel.app/) monitors the
pool and controls already registered nodes through the Oracle-local agent. It
does not create cloud resources or perform the first SSH join.

New VPS registration and joining currently run from an SSH-capable checkout of
this repository using the local `poolctl web` operator surface. This is
intentional: private SSH keys stay on the operator machine and are never copied
to Vercel.

## Sources Of Truth

- Repository: <https://github.com/blackdragoon26/Myprod>
- Production dashboard: <https://myprod-control.vercel.app/>
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
curl -fsS https://myprod-control.vercel.app/api/smoke
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

Unlock the production dashboard, refresh it, and confirm the new node appears.
Do not copy the agent token or private SSH keys into the repository.

## 8. Register And Deploy An Application

Use the hosted application flow for ordinary containerized backends. A project reservation is not required and would prevent Nomad from scheduling shared applications on that worker.

Preflight requirements:

- publish an immutable public container image for the target architecture;
- make the service listen on `0.0.0.0` and record its container port;
- expose an unauthenticated health endpoint that returns HTTP 2xx;
- point the chosen application hostname to Oracle public IP `140.245.5.201`;
- choose an exact target node with enough available CPU, memory, and disk;
- verify the target is joined, eligible, not draining, and not reserved.

From the hosted dashboard:

1. Unlock with the Oracle agent token and refresh live state.
2. Review **Resource Utilization**. CPU, memory, and root disk are actual host measurements from Nomad client stats.
3. Select **Add application** under **Managed Apps**.
4. Enter the app name, public image, domain, target node, container port, CPU reservation, memory reservation, and health path.
5. Select **Register application**. This persists configuration only and does not start the container.
6. Confirm the application appears with status `configured` and review its target and reservations.
7. Select **Deploy**, read the live-workload warning, and confirm.
8. Wait for Agent Output to report a healthy Nomad allocation on the selected node.
9. Refresh and verify the app status is `deployed`, then test its public HTTPS URL.

Do not enter secrets or private-registry credentials. This release supports public images and ephemeral container filesystems only; it does not yet model environment variables, encrypted secrets, persistent volumes, application deletion, or configuration edits.

Registration uses exact-node placement. This constrains one app without reserving the entire worker, so unrelated Nomad applications can share remaining capacity.

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
ssh -i ~/.ssh/keys/openclaw-oracle.key ubuntu@188.166.182.174
```

Do not run project installation commands on `oracle-main`; it remains the shared control plane and ingress host.

### System-wide installation boundary

System-wide installation on a reserved worker is allowed when the project requires it. Installing Ubuntu packages, compilers, P4 toolchains, libraries, containers, and project-owned systemd services does not affect `oracle-main` or workloads on other workers.

Prefer project-owned paths such as `/opt/<project>` and `/srv/<project>` where the installer permits it. Before a large or invasive toolchain installation, take a DigitalOcean snapshot so the worker can be restored without reconstructing the pool membership.

Do not modify or remove these Myprod lifelines unless the worker is intentionally being rebuilt:

- the `ubuntu` user's SSH key and `sshd` configuration;
- DigitalOcean firewall access to TCP 22;
- WireGuard interface `wg0`, its routes, or UDP 51820;
- the Nomad client configuration and service;
- Docker, while existing project tooling depends on it;
- the host default route, netplan configuration, bootloader, or root filesystem layout.

Also monitor free disk space and memory during large builds. A reservation prevents shared scheduling on the worker, but it cannot prevent a system-wide installer from breaking that worker itself. If the worker becomes unrecoverable, rebuild only `do-worker-1`; do not troubleshoot project packages on `oracle-main`.

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

## 10. Action Semantics

- **Control status** reads real systemd service state.
- **Deploy** runs a real Nomad job submission and status verification.
- **Freeze** changes real Nomad scheduling eligibility to ineligible.
- **Unfreeze** changes eligibility to eligible and is refused for reserved or draining nodes.
- **Drain** starts a real detached Nomad drain.
- **Cancel drain** stops the drain but keeps the node ineligible.
- **Reserve** requires an empty worker and records exclusive project ownership.
- **Release** clears ownership but does not silently make the node schedulable.

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
