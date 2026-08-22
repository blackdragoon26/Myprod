# Application Onboarding Handoff

Use this contract when handing a project to another coding agent before adding
it to Myprod. The project agent prepares a deployable artifact; the Myprod
operator separately registers and deploys it.

The current application nodes are Linux ARM64. Before preparing an image or CI
workflow, read [arm64-application-cicd.md](arm64-application-cicd.md). Myprod
does not change node architecture to accommodate an AMD64-only application.

## Generic Project-Agent Contract

The project agent must:

1. Identify the production backend process and keep unrelated project behavior
   unchanged.
2. Produce a non-root Linux container for the target node architecture.
3. Make the process listen on `0.0.0.0` at one documented container port.
4. Provide an unauthenticated HTTP health path returning 2xx without mutating
   data or requiring external credentials.
5. Bound request size, concurrency, runtime work, and generated files where the
   service accepts public traffic.
6. Build and test the image locally.
7. Publish an immutable image tag and record its registry digest.
8. Confirm the image can be pulled without registry credentials.
9. Document minimum CPU, memory, disk, architecture, and ephemeral-storage
   behavior.
10. Return a Myprod handoff manifest containing every field below.

The project agent must not:

- change Myprod, Oracle, Nomad, WireGuard, Traefik, or cloud firewalls;
- register or deploy the app without the operator's explicit instruction;
- place secrets in the image or handoff manifest;
- claim persistent storage when the Myprod app contract is ephemeral;
- bundle privileged host toolchains into an ordinary public backend image.

## Required Handoff Manifest

```txt
name:
source commit:
image:
image digest:
architecture:
container port:
health path:
recommended CPU MHz:
recommended memory MB:
ephemeral data behavior:
required environment variables:
required secrets:
publicly pullable without authentication: yes/no
local container smoke command:
health-check result:
project test command and result:
known limitations:
```

Applications that require secrets may opt into the fixed, operator-installed
runtime environment file. The dashboard stores only `secret_env: true`; it
never receives secret values. Before deployment, an operator must install
`/etc/poolctl/apps/<app-name>.env` on the exact target node with owner
`65532:65532` and mode `0400`. The container receives it read-only at
`/run/secrets/cutable.env`. The application must explicitly load that file.

Persistent volumes remain unsupported. Stop when durable application data
cannot live in an external managed service.

Plain, non-secret environment variables may be included in the handoff. If a
variable name looks credential-bearing (for example, it contains `TOKEN`,
`PASSWORD`, `SECRET`, `API_KEY`, `PRIVATE_KEY`, or `CREDENTIAL`), or if required
secrets cannot use the fixed operator-installed file, stop and report that the
hosted app form cannot safely represent the workload.

After registration, a Myprod operator may mint an app-scoped CI deploy token
from the hosted dashboard and place it in the repository's masked Actions
secrets. Project agents must never request the dashboard-wide operator token.
The CI workflow may call only the generic immutable-image endpoint for its own
app and repository. Deploy-token issuance does not change the rule above:
credentials consumed by the application itself remain SSH-installed.

## SpliDT Agent Context

Give the SpliDT agent the following task:

```txt
Prepare the SpliDT public showcase backend for ordinary Myprod managed-app
deployment. Do not install BMv2, Mininet, p4-guide, Open-P4Studio, or any
host-level package. Do not modify Myprod or any VPS.

Start from the current SpliDT showcase work and preserve its explicit evidence
boundaries. The lightweight public backend is showcase/server.py and the image
definition is showcase/Dockerfile. It must remain an unprivileged Linux
container for both AMD64 and ARM64, listening on 0.0.0.0:8765. Use
/api/system/capabilities as the read-only HTTP health path.

Run the showcase Python tests, build the image for linux/amd64 and linux/arm64,
start it locally, call the health endpoint, and run one curated API smoke flow.
Add a GitHub Actions workflow that publishes immutable commit-tagged images to GHCR.
Do not use only latest. Report the exact image digest and verify an anonymous
docker pull works after the package is made public.

The public image must contain only the lightweight software-reference backend
and retained evidence projection. BMv2 live and Tofino-model tooling remain
outside this container. Generated run data is ephemeral under
/var/lib/splidt/runs; state that clearly. Do not add secrets. The current server
defaults to public CORS, so report that boundary rather than inventing
configuration the application does not actually support.

Do not register or deploy the image in Myprod. Finish by returning the complete
Myprod handoff manifest from docs/application-onboarding.md.
```

Expected SpliDT values, subject to verification by that agent:

```txt
name: splidt-showcase
architecture: linux/amd64, linux/arm64
container port: 8765
health path: /api/system/capabilities
recommended CPU MHz: 1000
recommended memory MB: 2048
ephemeral data behavior: generated runs are lost when the allocation is replaced
required environment variables: none for the current public-CORS release
required secrets: none
```

The production deployment was verified on 2026-08-22 using multi-platform
digest
`ghcr.io/blackdragoon26/splidt-showcase@sha256:4efb4907cf4ce1e18e2a9b2540368b5c02a877a97aa07193bff0a964413443f7`.
Nomad places it on ARM64 node `oracle-worker-1`; the public capability endpoint
reports `aarch64` and build revision
`48cfecc5cdd7963fc63ad62e53533f106cc355e0`. DNS remains operator-controlled,
but Myprod can create and verify the Netlify A record during registration. The
backend hostname is `splidt-api.sankalpjha.dev`, pointing to Oracle ingress
`140.245.5.201`.
