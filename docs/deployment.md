# Deployment

`myprod-control` is the hosted dashboard for this repo.

Production URL:

```txt
https://myprod-control.vercel.app
```

GitHub repo:

```txt
https://github.com/blackdragoon26/Myprod
```

Agent provisioning documentation:

```txt
https://github.com/blackdragoon26/Myprod/blob/main/docs/agent-runbook.md
```

Project owner:

```txt
https://sankalpjha.dev/
```

## Vercel CI/CD

The desired production flow is:

```txt
git push origin main -> GitHub -> Vercel production deploy
```

Use Vercel Git Integration as the production deployment path. The old GitHub
Actions fallback was removed because a missing `VERCEL_TOKEN` secret made every
push show a failed deployment even when Vercel had already deployed from Git.

## Vercel Git Integration

Use this when the Vercel dashboard can access the GitHub account:

1. Open the Vercel project `myprod-control`.
2. Go to `Settings -> Git`.
3. Connect `blackdragoon26/Myprod`.
4. Set production branch to `main`.
5. Keep framework preset as `Other`.
6. Leave build command empty.
7. Leave output directory empty.
8. Confirm that pushes to `main` create production deployments.

The dashboard is a static `public/index.html` plus the serverless smoke endpoint at `api/smoke.js`.

## Oracle Agent Deployment

The static dashboard and Oracle agent are deployed separately. A Git push updates Vercel, but it does not replace `/usr/local/bin/poolctl` on Oracle.

For an agent change:

1. Run `go test ./...`.
2. Build a static Linux ARM64 binary for Oracle.
3. Copy it to `/tmp` over SSH.
4. Back up the current binary, atomically install the new binary, and restart only `poolctl-agent`.
5. Verify the agent health route, authenticated status, Nomad state, and public smoke checks.

Never place `POOLCTL_AGENT_TOKEN` in a build command, repository file, shell history, or deployment log.

## Manual Deploy

Manual deploys are useful for quick iteration, but they are not the long-term source of truth. Production should be Git-driven so the deployed dashboard always matches `main`.
