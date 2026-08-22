# Reserved Worker Context

This document is the handoff contract for agents using project-reserved Myprod workers.

## Current Assignment

There is no active project reservation. `splidt-showcase` is an ordinary
managed application on `oracle-worker-1`; the worker remains eligible for
Nomad scheduling.

Do not treat managed-app placement as permission to install project tooling on
the host. Create an explicit reservation first if future work needs host-level
packages or exclusive ownership.

## Required Entry Checks

After an explicit reservation, connect only to the worker named by current pool
state. For the present Oracle worker:

```sh
ssh -i ~/.ssh/keys/openclaw-oracle.key ubuntu@140.245.228.146
```

Then run:

```sh
hostname
uname -m
systemctl is-active ssh nomad docker wg-quick@wg0
df -h /
```

Expected identity is `oracle-worker-1` and `aarch64`. Stop if either differs or
if SSH, Nomad, Docker, or WireGuard is unhealthy. If a future reservation adds
project-specific instructions under `/opt/<project>/AGENTS.md`, read them
before making host changes.

## Allowed Project Work

- Install required Ubuntu packages, compilers, P4 toolchains, libraries, and containers.
- Create project data under `/opt/splidt`, `/srv/splidt`, and the `ubuntu` home directory.
- Add project services named with a `splidt-` prefix.
- Use `sudo` when an upstream installer genuinely requires host-level changes.

Record material package, service, kernel, and networking changes in `/opt/splidt/CHANGELOG.md`, including the command and rollback note.

## Protected Myprod Lifelines

Do not modify or remove these without explicit approval to rebuild the worker:

- `/home/ubuntu/.ssh`, `sshd`, and TCP 22 access;
- `/etc/wireguard/wg0.conf`, the `wg0` interface, its routes, or UDP 51820;
- `/etc/nomad.d`, the Nomad client service, or its node identity;
- Docker configuration, storage, networks, or service;
- netplan, the host default route, firewall policy, bootloader, disk partitions, or root filesystem;
- Myprod reservation state on `oracle-main`.

Do not expose new public ports by default. Keep project listeners on localhost, private addresses, or behind approved Myprod ingress.

## Change Protocol

1. Verify the entry checks.
2. Read the upstream installer before running it with `sudo`.
3. Prefer `/opt/splidt` or `/srv/splidt` over scattering project files across the host.
4. Capture the before state for packages or configuration being changed.
5. Make one logical installation change at a time.
6. Record the change in `/opt/splidt/CHANGELOG.md`.
7. Re-run the health checks below.

Reboot, power-off, restore, resize, reservation release, and node deletion always require explicit user approval.

## Completion Checks

```sh
systemctl is-active ssh nomad docker wg-quick@wg0
ip -4 addr show wg0
df -h /
curl -fsS https://api.sankalpjha.dev/ >/dev/null
```

The public API check verifies the shared control plane remains healthy; a project installation must not require changes there.

## Recovery

No image-level rollback is documented for `oracle-worker-1`. Agents must not
assume a boot-volume backup or snapshot exists.

Before a risky change, capture the affected files, package list, and service configuration under `/opt/splidt/checkpoints/<UTC timestamp>/`. File-level checkpoints do not replace a machine snapshot, but they provide a scoped rollback path without adding cloud cost.

Creating or restoring a future snapshot is a billed/destructive infrastructure action: inspect current data and obtain explicit user approval first.
