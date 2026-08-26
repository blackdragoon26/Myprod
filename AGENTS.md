# Myprod Agent Instructions

This repository operates real Oracle and worker VPS infrastructure. Read
`docs/agent-runbook.md` before creating, joining, resizing, draining, or
destroying a node.

Before changing the hosted dashboard, operator authentication, managed apps,
CI deploy tokens, Oracle agent binary, or Oracle agent environment, read
`docs/llm-operator-guide.md` and the specific document it routes to. The
canonical dashboard is `https://control.sankalpjha.dev/`; the Vercel URL is a
fallback alias.

Both current Nomad application nodes are Linux ARM64. Before onboarding an app
or writing its CI/CD, read `docs/arm64-application-cicd.md`. Do not change the
Oracle architecture or add production emulation for an AMD64-only app; publish
an ARM64 or multi-platform application image instead.

## Required Safety

- Never commit agent tokens, cloud credentials, passwords, private SSH keys, or
  Nomad ACL tokens.
- Treat `.poolctl/` as operator state. Back it up before manual edits and keep it
  out of Git.
- Reconcile local `.poolctl/` state with Oracle's agent store before joining a
  node. Never overwrite newer hosted freeze/drain state blindly.
- Use dry-run or read-only checks before infrastructure mutations.
- Do not destroy or resize a live node without explicit user approval and a
  verified drain.
- Keep Nomad and WireGuard ports private. Workers do not expose public HTTP or
  HTTPS in the current architecture.
- Preserve unrelated working-tree changes.

## Sandbox Partitions

Before creating, enrolling, or handing out a sandbox, read
`docs/llm-sandbox.md`. A sandbox is a disposable Ubuntu ARM64 container, not a
machine and not a managed app.

- Never create a sandbox on `oracle-main`; sandbox hosting is worker-only.
- Never weaken the rendered job to make a task pass: no privileged mode, no
  host bind mounts, no Docker socket, no host networking, no Traefik route, no
  added capabilities, and no non-Ubuntu image.
- Enroll a node for egress only after running the host isolation bundle on it
  and verifying with `sandbox-isolation.sh --verify`.
- Treat a sandbox session token as scoped to one sandbox. Never substitute the
  operator token for it, and never give an operator token to sandbox work.
- Destroy sandboxes when finished. Do not extend a sandbox to keep state alive;
  the filesystem is ephemeral by contract.

## Reserved Project Workers

Before using a reserved worker, read `docs/reserved-worker-context.md`. There
is currently no reserved project worker. `splidt-showcase` is a managed app on
`oracle-worker-1`, not permission to modify that host for project work.

- Project installation is allowed only on the worker named by the reservation.
- Never run project installers on `oracle-main` (`140.245.5.201`).
- Never release or unfreeze a reservation just to make an installation work.
- Preserve SSH, WireGuard, Nomad, Docker, host routing, and provider firewall
  access unless the user explicitly approves rebuilding the worker.
- If a future reservation has `/opt/<project>/AGENTS.md`, read it before using
  `sudo` and record material host changes in the project changelog.
- Create a scoped file/configuration checkpoint before risky changes and never
  claim image-level rollback exists without verifying a current backup.
- Ask before rebooting, powering off, resizing, restoring, or deleting a worker.

## Verification

Run `go test ./...` for code changes. For infrastructure changes, also verify
SSH, passwordless sudo, WireGuard, Nomad registration, the production agent
health route, and both public smoke checks described in the runbook.

## Publishing

Push GitHub changes over the configured SSH remote. Production dashboard
deployment is Git-driven from `main` through Vercel Git Integration.
