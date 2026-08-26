# Sandbox Partitions

A sandbox partition is a disposable Ubuntu ARM64 container that an LLM or
automation agent can drive directly, without SSH, without a Nomad token, and
without any ability to change the rest of the pool.

Read this document before enrolling a sandbox host, creating a sandbox, or
handing a sandbox token to an agent.

## What A Sandbox Is

- one container, on one worker, inside a budget an operator set for that worker;
- Ubuntu ARM64 (`linux/arm64`), matching the only architecture the pool runs;
- reachable only through the Oracle agent's sandbox API;
- ephemeral: it has a mandatory TTL and is reclaimed automatically.

## What A Sandbox Is Not

- **not a managed app.** It is never registered in `config.yaml`, never given a
  domain, never given a Traefik route, and never appears in ingress.
- **not a project reservation.** A reservation hands an agent a whole machine
  through SSH. A sandbox hands it one contained process tree.
- **not a place for production data.** Its filesystem is discarded on expiry.
- **not a way to run arbitrary images.** Only Ubuntu base images are accepted.

If a project genuinely needs host-level packages, kernel modules, or network
namespaces on a real machine, that is a reservation
([`reserved-worker-context.md`](reserved-worker-context.md)), not a sandbox.

## Blast Radius

Each layer below is independent. A sandbox has to defeat all of them, not one,
to reach anything outside its own box.

| Layer | Control | Effect |
| --- | --- | --- |
| Placement | Job constraint pins `${node.unique.name}` to the enrolled worker; control-plane nodes are refused at enrollment, at creation, and at render time | A sandbox can never be scheduled on `oracle-main` |
| Node state | Frozen, draining, and reserved nodes are refused | Sandboxes never interfere with drains or project reservations |
| Capacity | Per-node budget: max concurrent sandboxes, max total CPU MHz, max total memory MB | Sandboxes cannot starve managed apps |
| Live pressure | Creation is refused when the node reports over 85% memory or root-disk use | A busy worker is not made worse |
| Scheduler priority | Sandbox jobs render at Nomad priority 10 | Managed apps outrank sandboxes for capacity |
| Process | `privileged = false`, `cap_drop = ["ALL"]`, `security_opt = ["no-new-privileges"]`, `pids_limit`, `ulimit`, `init = true`, `ipc_mode = "private"` | No privilege escalation, no fork bombs |
| Filesystem | No host bind mount, no Docker socket, no persistent volume; size-capped tmpfs `/workspace` and `/tmp` in both profiles; read-only rootfs in the strict profile | A sandbox cannot read or write host state, and its work area is bounded |
| Disk pressure | The agent reclaims every sandbox on a node that crosses 92% root disk or 96% memory | Nomad does not hard-cap ephemeral disk, so sandboxes are culled before managed apps suffer |
| Network | `network_mode = "none"` by default; egress mode uses a dedicated Docker bridge with inter-container communication disabled, public DNS resolvers, and a host firewall chain that drops the overlay, all RFC1918 space, and cloud metadata | A sandbox cannot reach Nomad, WireGuard peers, other apps, other sandboxes, the host, or instance metadata |
| Identity | `identity { env = false file = false }`; no Vault, no Consul, no Nomad token, no registry credentials | Nothing inside can authenticate as the allocation |
| Lifetime | Mandatory TTL, container-side `sleep` deadline, agent-side reaper, absolute 4-hour ceiling | A forgotten sandbox cannot hold capacity indefinitely |
| Credential | Session token scoped to one sandbox ID, stored only as a SHA-256 digest | A sandbox token cannot read pool status, manage apps, mint deploy tokens, or touch another sandbox |
| Job naming | Every sandbox job is `poolctl-sbx-<id>`; stop and purge paths refuse any other name, and app names may not use that prefix | The sandbox surface cannot stop a managed application |

## Profiles

| Profile | Root filesystem | Capabilities | Use it for |
| --- | --- | --- | --- |
| `strict` (default) | read-only | none | running and inspecting code, reproducing behavior |
| `workspace` | writable, ephemeral | `chown`, `dac_override`, `fowner`, `fsetid`, `kill`, `setgid`, `setuid`, `setfcap` only | `apt-get install`, building, toolchain work |

Both profiles get a size-capped tmpfs at `/workspace` (the working directory
and `HOME`) and at `/tmp`, each half of the requested disk size.

`workspace` never grants `SYS_ADMIN`, `NET_ADMIN`, `NET_RAW`, `SYS_PTRACE`,
`SYS_MODULE`, `SYS_TIME`, `SYS_BOOT`, `SYS_RAWIO`, `MKNOD`, or `AUDIT_CONTROL`.

## Networks

| Network | Behavior |
| --- | --- |
| `none` (default) | loopback only; no DNS, no package installs, no exfiltration path |
| `egress` | public internet only, through the isolated `poolctl-sandbox` bridge |

`egress` is available only on a node enrolled with egress, which requires the
host isolation bundle. Without that bundle the agent refuses the request rather
than silently giving a sandbox unrestricted network access.

## Enabling A Sandbox Host

Sandbox hosting is off by default and is enabled per worker. It never touches
the control plane.

1. Render the bundle locally:

   ```sh
   go run ./cmd/poolctl sandbox render-isolation
   ```

2. Review `work/rendered/sandbox/sandbox-isolation.sh`. It only creates one
   Docker network and one firewall chain, tags every rule with a
   `poolctl-sandbox` comment, and refuses to run on the control plane.

3. Copy it to the worker and check the current state before changing anything:

   ```sh
   scp -i ~/.ssh/keys/openclaw-oracle.key -r work/rendered/sandbox ubuntu@<worker-ip>:~/
   ssh -i ~/.ssh/keys/openclaw-oracle.key ubuntu@<worker-ip> '~/sandbox/sandbox-isolation.sh --verify'
   ```

4. Apply it, then verify again:

   ```sh
   ssh -i ~/.ssh/keys/openclaw-oracle.key ubuntu@<worker-ip> '~/sandbox/sandbox-isolation.sh --apply'
   ```

5. Confirm the pool is unchanged:

   ```sh
   ssh -i ~/.ssh/keys/openclaw-oracle.key ubuntu@<worker-ip> 'systemctl is-active ssh nomad docker wg-quick@wg0'
   curl -fsS https://api.sankalpjha.dev/__poolctl/api/health
   ```

6. Enroll the node through the operator-authenticated action endpoint:

   ```sh
   curl -fsS -X POST https://api.sankalpjha.dev/__poolctl/api/action \
     -H "Authorization: Bearer $OPERATOR_TOKEN" \
     -H "Content-Type: application/json" \
     -d '{"action":"sandbox-host-enroll","name":"oracle-worker-1","value":"egress,max=2,cpu=1000,mem=1024"}'
   ```

   Omit `egress` to enroll a node whose sandboxes get loopback only. Use
   `sandbox-host-remove` to withdraw enrollment; it is refused while a sandbox
   is still live on that node.

The isolation bundle is fully reversible with `--remove`, which deletes the
chain, the marker, the systemd unit, and the Docker network.

## Creating And Destroying A Sandbox

From the hosted dashboard, use **Sandbox Partitions → New sandbox**. The token
is displayed exactly once.

From the API, with the operator credential:

```sh
curl -fsS -X POST https://api.sankalpjha.dev/__poolctl/api/sandboxes \
  -H "Authorization: Bearer $OPERATOR_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"name":"llm-sandbox","node":"oracle-worker-1","profile":"workspace","network":"egress","cpu":500,"memoryMb":512,"ttlSeconds":1800}'
```

The response contains `issuedSandbox.token`. Myprod stores only its digest and
cannot show it again.

Destroy explicitly when the work is done; do not rely on the TTL:

```sh
curl -fsS -X DELETE https://api.sankalpjha.dev/__poolctl/api/sandboxes/<id> \
  -H "Authorization: Bearer $OPERATOR_TOKEN"
```

## Using A Sandbox As An Agent

The scoped token authorizes exactly four operations on exactly one sandbox:

| Method | Path | Purpose |
| --- | --- | --- |
| `GET` | `/__poolctl/api/sandboxes/<id>` | status |
| `POST` | `/__poolctl/api/sandboxes/<id>/exec` | run a command inside the box |
| `GET` | `/__poolctl/api/sandboxes/<id>/logs` | recent task logs |
| `DELETE` | `/__poolctl/api/sandboxes/<id>` | destroy it early |

```sh
curl -sS -X POST https://api.sankalpjha.dev/__poolctl/api/sandboxes/$SANDBOX_ID/exec \
  -H "Authorization: Bearer $SANDBOX_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"command":"uname -m && cat /etc/os-release"}'
```

`command` runs under `/bin/sh -c` inside the container. `argv` is available for
direct execution without a shell. The host never interprets either: the request
is passed to Nomad as an argument vector, so quoting inside the command cannot
escape into the agent's process. Output is capped at 64 KiB and each call has a
timeout of 60 seconds by default and 120 seconds at most.

Everything else returns `401`. The same token cannot read `/status`, register
an app, run a node action, mint a deploy token, extend its own lifetime, list
sandboxes, or reach another sandbox ID.

## Budgets, Expiry, And Reclamation

- Every sandbox has a TTL between 60 seconds and 4 hours; the default is 30
  minutes.
- The container itself exits at its deadline, and the agent reaper purges
  expired jobs on a 30-second interval.
- **Extend** is operator-only and can never push a sandbox past 4 hours from
  creation.
- Draining a node reclaims its sandboxes first, so drain behavior stays
  predictable and no budget is orphaned.
- The agent reclaims every sandbox on a node that crosses 92% root-disk or 96%
  memory use. Sandboxes are disposable; managed applications are not.
- A sandbox that fails to start releases its budget immediately and is recorded
  as `failed`.

## Not Supported On Purpose

- non-Ubuntu images, private registries, and image pull credentials;
- persistent volumes, host paths, and the Docker socket;
- inbound network access, published ports, DNS records, and Traefik routes;
- privileged mode, `SYS_ADMIN`, and user-namespace escapes;
- sandboxes on the control plane;
- sandbox tokens that outlive their sandbox.

Do not add any of these to make a task easier. Publish a proper managed
application, or request a project reservation, instead.

## Residual Risks

State these plainly rather than implying a sandbox is a security boundary it is
not:

- A container is not a virtual machine. A kernel vulnerability reachable from an
  unprivileged container would still be a real escape path; the profile limits
  reachable surface, it does not remove it.
- `egress` sandboxes can reach the public internet. Treat anything placed inside
  one as published.
- The host isolation bundle enforces the network boundary. Enrolling a node with
  egress without running the bundle would leave that boundary unenforced, which
  is why the agent refuses egress on nodes not enrolled for it and why the
  enrollment output tells the operator to verify.
- A sandbox shares a kernel and a disk with whatever else runs on that worker.
  Nomad's `ephemeral_disk` is a scheduling reservation, not an enforced limit,
  so writes outside the tmpfs work areas are bounded only by the node-pressure
  reclamation above. Budgets, the pre-launch check, and that backstop reduce
  noisy-neighbor impact; they do not eliminate it.

## Verification

For code changes:

```sh
go test ./...
```

After enabling a sandbox host, verify in this order:

```sh
~/sandbox/sandbox-isolation.sh --verify
systemctl is-active ssh nomad docker wg-quick@wg0
curl -fsS https://api.sankalpjha.dev/__poolctl/api/health
curl -fsS https://control.sankalpjha.dev/api/smoke
```

Then create one throwaway sandbox and prove the boundary holds from inside it:

```sh
# expected: aarch64
{"command":"uname -m"}
# expected: failure, not a Nomad response
{"command":"curl -m 5 -sS https://10.44.0.1:4646/v1/status/leader || echo blocked"}
# expected: failure, not instance metadata
{"command":"curl -m 5 -sS http://169.254.169.254/ || echo blocked"}
```

Destroy it, then confirm every managed application is still healthy.
