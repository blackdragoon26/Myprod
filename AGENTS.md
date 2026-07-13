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

## Verification

Run `go test ./...` for code changes. For infrastructure changes, also verify
SSH, passwordless sudo, WireGuard, Nomad registration, the production agent
health route, and both public smoke checks described in the runbook.

## Publishing

Push GitHub changes over the configured SSH remote. Production dashboard
deployment is Git-driven from `main` through Vercel Git Integration.
