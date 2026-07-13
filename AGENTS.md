# Myprod Agent Instructions

This repository operates real Oracle and worker VPS infrastructure. Read
`docs/agent-runbook.md` before creating, joining, resizing, draining, or
destroying a node.

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

## Reserved Project Workers

Before using a reserved worker, read `docs/reserved-worker-context.md`. The
current `splidt` workspace is the whole `do-worker-1` machine at
`188.166.182.174`; it is not a directory on `oracle-main`.

- Project installation is allowed only on the worker named by the reservation.
- Never run project installers on `oracle-main` (`140.245.5.201`).
- Never release or unfreeze a reservation just to make an installation work.
- Preserve SSH, WireGuard, Nomad, Docker, host routing, and DigitalOcean
  firewall access unless the user explicitly approves rebuilding the worker.
- On `do-worker-1`, read `/opt/splidt/AGENTS.md` before using `sudo` and append
  material host changes to `/opt/splidt/CHANGELOG.md`.
- No pre-install snapshot exists for `splidt`. Create a scoped file/configuration
  checkpoint before risky changes and never claim image-level rollback exists.
- Ask before rebooting, powering off, resizing, restoring, or deleting a worker.

## Verification

Run `go test ./...` for code changes. For infrastructure changes, also verify
SSH, passwordless sudo, WireGuard, Nomad registration, the production agent
health route, and both public smoke checks described in the runbook.

## Publishing

Push GitHub changes over the configured SSH remote. Production dashboard
deployment is Git-driven from `main` through Vercel Git Integration.
