# Reserved Worker Context

This document is the handoff contract for agents using project-reserved Myprod workers.

## Current Assignment

| Project | Worker | Public IP | Architecture | Scheduler state |
| --- | --- | --- | --- | --- |
| `splidt` | `do-worker-1` | `188.166.182.174` | `x86_64` | Nomad ineligible, reserved |

The reservation covers the entire worker. It keeps shared Nomad workloads away; it does not make unsafe host changes reversible.

## Required Entry Checks

Connect only to the reserved worker:

```sh
ssh -i ~/.ssh/keys/openclaw-oracle.key ubuntu@188.166.182.174
```

Then run:

```sh
cat /opt/splidt/AGENTS.md
hostname
uname -m
systemctl is-active ssh nomad docker wg-quick@wg0
df -h /
```

Expected identity is `do-worker-1` and `x86_64`. Stop if either differs, if `/opt/splidt/AGENTS.md` is missing, or if SSH/WireGuard is unhealthy.

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

No pre-install DigitalOcean snapshot exists as of 2026-07-14; the user explicitly declined the recurring snapshot cost and temporary shutdown. Agents must not assume image-level rollback is available.

Before a risky change, capture the affected files, package list, and service configuration under `/opt/splidt/checkpoints/<UTC timestamp>/`. File-level checkpoints do not replace a machine snapshot, but they provide a scoped rollback path without adding cloud cost.

Creating or restoring a future snapshot is a billed/destructive infrastructure action: inspect current data and obtain explicit user approval first.
