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

## Vercel CI/CD

The desired production flow is:

```txt
git push origin main -> GitHub -> Vercel production deploy
```

There are two supported ways to make that true.

## Preferred: Vercel Git Integration

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

## Fallback: GitHub Actions

The repo includes `.github/workflows/vercel-production.yml`, which deploys to Vercel on pushes to `main`.

Add these GitHub repository secrets:

```txt
VERCEL_TOKEN
VERCEL_ORG_ID
VERCEL_PROJECT_ID
```

Current known project values:

```txt
VERCEL_ORG_ID=team_PcwkUVWQ8AC7CPb6vP0fC5nI
VERCEL_PROJECT_ID=prj_DItWG40QOEJsCD6IQgLHxO9t1qRr
```

Do not commit `VERCEL_TOKEN`; create it in Vercel account settings and store it only as a GitHub Actions secret.

## Manual Deploy

Manual deploys through the Codex Vercel connector are useful for quick iteration, but they are not the long-term source of truth. Production should be Git-driven so the deployed dashboard always matches `main`.
