# LLM Operator Guide

This is the short entry point for an LLM or automation agent working on the
live Myprod repository. It routes to the detailed runbooks and records the
production boundaries that must not be inferred from old chat history.

## Canonical Production Surfaces

- Dashboard: <https://control.sankalpjha.dev/>
- Vercel fallback alias: <https://myprod-control.vercel.app/>
- Public agent base: <https://api.sankalpjha.dev/__poolctl/api>
- Repository: <https://github.com/blackdragoon26/Myprod>
- Oracle control plane: `ubuntu@140.245.5.201`
- Oracle architecture: Linux ARM64 (`GOOS=linux`, `GOARCH=arm64`)
- Oracle worker: `oracle-worker-1`, `ubuntu@140.245.228.146`, overlay
  `10.44.0.3`, Linux ARM64
- Current pool: two Oracle nodes; no DigitalOcean worker is registered

Do not claim access to Clerk, Vercel, Netlify, GitHub, Oracle, or a DNS provider
until the current environment actually proves that access. Never transcribe a
credential from a screenshot.

## Read Before Acting

| Work | Required document |
| --- | --- |
| Human sign-in, OTP, Clerk, passkeys, recovery | [`operator-authentication.md`](operator-authentication.md) |
| Dashboard or Oracle agent deployment | [`deployment.md`](deployment.md) |
| App registration, edit, image update, env, deploy | [`application-onboarding.md`](application-onboarding.md) |
| App architecture, native binaries, image publishing, CI/CD | [`arm64-application-cicd.md`](arm64-application-cicd.md) |
| CI deploy-token mint, rotate, revoke | [`agent-runbook.md`](agent-runbook.md) sections 8–9 |
| DNS credentials or managed records | [`netlify-dns.md`](netlify-dns.md) |
| Join, freeze, drain, resize, or destroy a node | [`agent-runbook.md`](agent-runbook.md) |
| Reserve or perform host-level work on a worker | [`reserved-worker-context.md`](reserved-worker-context.md) |
| Create, enroll, or hand out an LLM sandbox partition | [`llm-sandbox.md`](llm-sandbox.md) |

## Credential Boundaries

Keep these three credential classes separate:

1. Clerk session JWTs authenticate a human operator. The browser obtains them;
   Oracle verifies them with public JWKS. The Clerk secret key stays in Vercel
   or an explicitly authorized temporary operator process and never goes to
   Oracle.
2. Myprod CI deploy tokens authenticate one repository to one application.
   Myprod mints 32 random bytes, shows the plaintext once, and stores only a
   digest in the Oracle agent store. Mint, rotate, and revoke them live through
   **CI tokens**; do not restart the agent.
3. Application runtime secrets belong only in the root-managed target-node file
   `/etc/poolctl/apps/<app-name>.env`. They never enter the dashboard, Clerk,
   Vercel, the agent store, or repository.

4. Sandbox session tokens authorize exec, logs, status, and destroy on exactly
   one sandbox partition. They are shown once, stored as a digest, and die with
   their sandbox. They never authorize pool status, app management, node
   actions, or another sandbox.

The legacy `POOLCTL_AGENT_TOKEN` is a recovery credential. Do not give it to an
application repository and do not replace app-scoped CI tokens with it. Never
hand it to sandbox work in place of a sandbox session token.

## Compatibility And Uptime Rules

- A push to `main` deploys the static dashboard through Vercel. It does not
  update Oracle's `/usr/local/bin/poolctl`.
- Dashboard controls must remain gated by agent capability flags so dashboard
  and agent can roll out in either order.
- Updating only `poolctl-agent` must not submit, stop, or restart Nomad jobs.
- Back up the current agent binary and `/etc/poolctl-agent.env`, install the new
  ARM64 binary atomically, restart only `poolctl-agent`, and roll back both
  files if local health fails.
- Editing app configuration does not deploy it. **Deploy** remains a separate,
  explicit action.
- Image changes use the generic app endpoint and an immutable digest. Do not
  add another app-specific deploy handler.
- Non-secret environment variables may be stored in app configuration. Reject
  secret-shaped names. Runtime secrets stay SSH-installed.
- Never destroy, resize, drain, release, or unfreeze infrastructure merely to
  make a deployment pass.
- Never weaken sandbox confinement to make a sandbox task pass. Sandboxes are
  worker-only, unprivileged, unroutable, and TTL-bound by contract.

## Minimum Verification

For code changes, run:

```sh
go test ./...
```

For a dashboard push, verify the Git commit's Vercel status, hard-reload the
canonical domain, and run:

```sh
curl -fsS https://control.sankalpjha.dev/api/smoke
curl -fsS https://api.sankalpjha.dev/__poolctl/api/health
```

For an Oracle agent rollout, additionally verify:

- SSH and passwordless `sudo`;
- `poolctl-agent`, Nomad, Traefik, and WireGuard are active;
- authenticated status reports the expected capability flags;
- recovery authentication still works;
- existing managed applications and public routes remain healthy.

Do not infer success from a dashboard label or a Nomad evaluation ID alone.
Read allocation state and surface task exit details when deployment health
fails.

## Publishing

Preserve unrelated working-tree changes. Commit no credentials or `.poolctl/`
state. Push `main` through the configured SSH Git remote; Vercel production is
Git-driven. Report exactly which production surfaces changed and whether any
service restart occurred.
