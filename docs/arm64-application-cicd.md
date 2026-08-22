# ARM64 Application And CI/CD Contract

This is the source of truth for an agent preparing an external backend for the
current Myprod production pool. Read it before changing an application's
Dockerfile, release workflow, native binaries, or Myprod deployment workflow.

## Non-Negotiable Production Boundary

The machines that execute current Myprod applications are Linux ARM64:

| Node | Role | Architecture | Public application ports |
| --- | --- | --- | --- |
| `oracle-main` | Nomad control plane and Traefik ingress | `linux/arm64` (`aarch64`) | 80 and 443 |
| `oracle-worker-1` | Nomad application worker | `linux/arm64` (`aarch64`) | none; traffic arrives through `oracle-main` |

The dashboard at <https://control.sankalpjha.dev/> is a Vercel-hosted control
surface. Vercel serving the dashboard does not make backend workloads x86_64.
Nomad runs the registered container on its selected Oracle node, and both
current Oracle nodes are ARM64.

Myprod will not change a node's architecture for one application. An
application agent must not resize or replace an Oracle instance, add an AMD64
worker, install production QEMU, or alter Nomad/WireGuard to make an x86-only
artifact run. Fix the application's artifact instead:

- publish `linux/arm64` when the application is used only by Myprod; or
- preferably publish one multi-platform image containing both `linux/amd64`
  and `linux/arm64` when developers, tests, or other installations use AMD64.

An immutable digest may identify a multi-platform OCI index. Docker on the
Oracle worker automatically selects the ARM64 manifest from that index. A
digest that resolves only to an AMD64 manifest is not compatible.

## Responsibility Boundary

An application agent owns:

- application source, tests, Dockerfile, and architecture-compatible binaries;
- the public container image and its immutable registry digest;
- the health endpoint, container port, resource recommendation, and app-level
  configuration;
- repository CI that builds, publishes, and requests deployment.

The Myprod operator owns:

- app registration, target-node selection, CPU and memory reservations;
- Oracle, Nomad, Traefik, WireGuard, DNS credentials, and host firewall state;
- app-scoped CI-token issuance and application-secret installation;
- production investigation requiring SSH or Nomad ACL access.

Do not cross this boundary merely because an image fails on ARM64. A project
agent should return a compatible image and handoff manifest, not reconfigure
the production host.

## Architecture Preflight In The Application Repository

Before writing CI, inspect every executable artifact and base image:

```sh
uname -m
file path/to/backend-binary
readelf -h path/to/backend-binary | sed -n '/Machine:/p'
docker buildx imagetools inspect IMAGE:TAG
```

Interpret common results as follows:

- `x86-64`, `x86_64`, or `amd64` means the binary cannot execute natively on
  the current Myprod nodes.
- `ARM aarch64`, `aarch64`, or `arm64` is the required native architecture.
- shell scripts and interpreted source can still fail when they download or
  invoke an architecture-specific helper.
- a multi-platform base image does not repair an AMD64 binary copied into it.
- an ARM64 binary may still fail when its dynamic loader or native libraries
  are absent. Prefer a compatible runtime base or a static binary where the
  project's licensing and functionality allow it.

Audit Dockerfiles for hard-coded downloads containing `amd64`, `x86_64`, or
`x64`. Map BuildKit's `TARGETARCH` to the upstream release's naming convention
and verify checksums. Never silently download the AMD64 asset as a fallback.

## Building Native Code Correctly

### Go example

Use BuildKit target arguments so one Dockerfile produces both architectures:

```dockerfile
# syntax=docker/dockerfile:1
FROM --platform=$BUILDPLATFORM golang:1.25-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG TARGETOS
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags='-s -w' -o /out/backend ./cmd/backend

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/backend /backend
USER nonroot:nonroot
ENTRYPOINT ["/backend"]
```

If the application requires CGO, use a real cross-toolchain or native ARM64 CI
runner and test the resulting shared-library linkage. Do not force
`CGO_ENABLED=0` if that changes required behavior.

### Python, Node.js, Java, and similar runtimes

Official runtime images are commonly multi-platform, but native wheels, npm
addons, JNI libraries, downloaded CLIs, and OS packages may not be. Build each
platform through Buildx, fail the build when a dependency has no ARM64 release,
and run at least the health smoke on ARM64 or under build-time emulation.

### Prebuilt upstream binaries

Select the artifact explicitly from `TARGETARCH`. For example, translate
BuildKit `arm64` to an upstream asset named `aarch64` only when that asset is
actually published. Verify its checksum before installation. If the upstream
project publishes no ARM64 artifact and cannot be compiled for ARM64, stop and
report the incompatibility; the production architecture remains unchanged.

## Required Container Contract

Every Myprod backend must:

1. include a `linux/arm64` manifest;
2. run as a non-root user unless a reviewed application requirement proves
   otherwise;
3. listen on `0.0.0.0` at one documented container port;
4. expose an unauthenticated, read-only health path returning HTTP 2xx;
5. start without interactive setup;
6. write only ephemeral data locally or use an external durable service;
7. contain no credentials;
8. be anonymously pullable, unless the operator has separately installed a
   read-only registry credential on the exact target node;
9. be deployed by immutable `@sha256:` digest, never only `latest` or another
   mutable tag.

Use the complete handoff manifest in
[application-onboarding.md](application-onboarding.md). Registration and
deployment are separate actions: editing or registering configuration does not
start the new image.

## GitHub Actions: Test, Publish, And Deploy

The following pattern publishes a multi-platform GHCR image and asks Myprod to
deploy the resulting immutable OCI-index digest. Replace the uppercase example
values and project test command. Keep the registry/repository identical to the
repository already registered for the Myprod app.

```yaml
name: Publish and deploy backend

on:
  push:
    branches: [main]
  workflow_dispatch:

permissions:
  contents: read
  packages: write

concurrency:
  group: production-backend
  cancel-in-progress: false

env:
  IMAGE: ghcr.io/your-lowercase-owner/your-backend
  MYPROD_APP: your-registered-app-name
  MYPROD_IMAGE_ENDPOINT: https://api.sankalpjha.dev/__poolctl/api

jobs:
  publish:
    runs-on: ubuntu-24.04
    outputs:
      image: ${{ steps.reference.outputs.image }}
    steps:
      - uses: actions/checkout@v4

      - name: Run project tests
        run: ./your-project-test-command

      - name: Set up QEMU for cross-platform build steps
        uses: docker/setup-qemu-action@v3

      - name: Set up Docker Buildx
        uses: docker/setup-buildx-action@v3

      - name: Log in to GHCR
        uses: docker/login-action@v3
        with:
          registry: ghcr.io
          username: ${{ github.actor }}
          password: ${{ secrets.GITHUB_TOKEN }}

      - name: Build and publish immutable multi-platform image
        id: build
        uses: docker/build-push-action@v6
        with:
          context: .
          file: Dockerfile
          platforms: linux/amd64,linux/arm64
          push: true
          tags: ${{ env.IMAGE }}:sha-${{ github.sha }}
          provenance: mode=max
          sbom: true

      - name: Record immutable image reference
        id: reference
        env:
          DIGEST: ${{ steps.build.outputs.digest }}
        run: |
          echo "image=${IMAGE}@${DIGEST}" >> "$GITHUB_OUTPUT"
          echo "Published ${IMAGE}@${DIGEST}" >> "$GITHUB_STEP_SUMMARY"

  deploy:
    needs: publish
    runs-on: ubuntu-24.04
    environment: production
    steps:
      - name: Request verified Myprod deployment
        env:
          IMAGE_REFERENCE: ${{ needs.publish.outputs.image }}
          DEPLOY_TOKEN: ${{ secrets.MYPROD_DEPLOY_TOKEN }}
        run: |
          payload="$(jq -n --arg image "$IMAGE_REFERENCE" '{image: $image}')"
          curl --fail-with-body --silent --show-error \
            --request POST \
            --header "Authorization: Bearer ${DEPLOY_TOKEN}" \
            --header "Content-Type: application/json" \
            --data "$payload" \
            "${MYPROD_IMAGE_ENDPOINT}/apps/${MYPROD_APP}/image"
```

Pin third-party Actions to reviewed commit SHAs when the application
repository's security policy requires it. Keep `cancel-in-progress: false` so a
new push does not kill a deployment after it has already started. For stricter
ordering, protect the GitHub `production` environment and require approval.

The publish job must succeed before deployment. Do not deploy a tag and then
resolve it later: pass `steps.build.outputs.digest` from the exact build that
was tested.

## One-Time Myprod CI Setup

The application must already be registered in Myprod with the same image
registry/repository. Then the operator:

1. signs in at <https://control.sankalpjha.dev/>;
2. finds the app under **Managed Apps** and opens **CI tokens**;
3. generates a token labeled for the repository or workflow;
4. copies the plaintext once;
5. saves it in GitHub under **Settings → Secrets and variables → Actions** as
   `MYPROD_DEPLOY_TOKEN`;
6. closes the one-time token display without pasting it into chat, logs, an
   issue, application configuration, or Git.

Myprod stores only a digest of this token. It is scoped to one app and cannot
change another app. The image endpoint also requires the new reference to use
the already registered image repository and an immutable SHA-256 digest.

Never put the dashboard-wide `POOLCTL_AGENT_TOKEN`, a Clerk session, a Nomad
ACL token, an application runtime secret, or a registry write credential in
application CI. If the **CI tokens** control is absent, inspect the authenticated
agent capabilities. `appDeployTokensV1` must be true. Stop and ask the Myprod
operator for a controlled agent rollout rather than substituting a broader
credential.

For a private GHCR package, the GitHub workflow's login only authorizes the
build runner. It does not authorize the Oracle worker. Prefer making the
runtime image public. Otherwise, an operator must separately install a
read-only pull credential on every eligible target node; it never belongs in
the app record or deploy request.

## What Myprod Does With The Request

CI calls:

```txt
POST /__poolctl/api/apps/{name}/image
Authorization: Bearer <app-scoped deploy token>
Content-Type: application/json

{"image":"ghcr.io/org/repository@sha256:<64 lowercase hex>"}
```

Myprod validates the app scope, digest syntax, and existing repository
allowlist. It renders the candidate Nomad job for the app's configured node,
submits it, and waits for a healthy allocation. Only after health succeeds does
it persist the new digest in the app store. Failure output includes available
Nomad evaluation and allocation evidence.

The current service shape uses one allocation, so do not promise mathematically
zero downtime. Keep startup and health checks fast, handle termination signals,
and test backward-compatible configuration so a replacement becomes healthy
quickly. Myprod must not restart unrelated applications or change node
architecture during an image deployment.

## Verification Before And After Deployment

Before publishing:

```sh
docker buildx build --platform linux/amd64,linux/arm64 --push \
  -t ghcr.io/org/repository:sha-GIT_SHA .
docker buildx imagetools inspect ghcr.io/org/repository:sha-GIT_SHA
```

Confirm the inspection output contains both `linux/amd64` and `linux/arm64`.
Then verify an anonymous pull in a logged-out environment when the package is
intended to be public.

After deployment, verify:

- the API response says the deployment is healthy;
- the public HTTPS health URL returns 2xx;
- application behavior matches the source commit;
- the running allocation is on the configured node;
- the deployed reference is the exact digest produced by CI;
- there are no unexpected restarts or architecture errors.

Only a Myprod operator should use SSH or authenticated Nomad inspection. An
ordinary project agent reports the digest, workflow run, public smoke result,
and any evidence returned by the deployment API.

## Common Failures

### `no matching manifest for linux/arm64`

The pushed tag or digest contains no ARM64 manifest. Add `linux/arm64` to the
Buildx platforms, ensure every build stage supports it, republish, and inspect
the registry index before retrying.

### `exec format error`

The image manifest may be ARM64 while an executable copied into it is AMD64.
Run `file`/`readelf` on the binary and fix the project's download or compiler
target. Do not add emulation to production.

### Container starts but health never becomes ready

Check that the process listens on `0.0.0.0`, the registered port is correct,
the health path is unauthenticated and returns 2xx, required non-secret
configuration is present, and the process is not waiting for durable local
state. Use the allocation evidence returned by Myprod.

### Image pull is unauthorized

Make the runtime package public or ask the operator to install a read-only node
credential. Never add a registry token to the image, deploy JSON, or app env.

### Myprod returns unauthorized

Confirm the Actions secret contains the one-time app-scoped token for this exact
app, not a token for another app. Rotate it in the dashboard if its value is
unknown; plaintext cannot be recovered.

### Myprod rejects the repository

The endpoint permits a new digest, not an arbitrary registry/repository move.
An operator must review and edit the registered app separately before CI can
deploy from a different repository.

## Rollback And Token Rotation

Keep the previous known-good multi-platform digest in the release record. To
roll back, send that immutable reference through the same app-scoped image
endpoint and verify health. Never retag `latest` and assume it changes the
stored deployment.

Rotate a CI token without restarting Myprod:

1. mint a second app-scoped token;
2. replace `MYPROD_DEPLOY_TOKEN` in GitHub Actions;
3. run a deployment or authorized no-change verification with the new token;
4. confirm its **last used** time in Myprod;
5. revoke the old token.

Multiple tokens may coexist during rotation. Do not revoke the old token before
the replacement workflow succeeds.

## Verified SpliDT Reference

SpliDT demonstrates the required pattern:

- source repository: <https://github.com/blackdragoon26/splidt>
- source revision: `48cfecc5cdd7963fc63ad62e53533f106cc355e0`
- image: `ghcr.io/blackdragoon26/splidt-showcase`
- multi-platform digest:
  `sha256:4efb4907cf4ce1e18e2a9b2540368b5c02a877a97aa07193bff0a964413443f7`
- platforms: `linux/amd64` and `linux/arm64`
- production node: `oracle-worker-1`
- public health: <https://splidt-api.sankalpjha.dev/api/system/capabilities>
- verified runtime machine: `aarch64`

This example proves the desired solution: change the application's build and
published image, then deploy the ARM64-capable digest. The Oracle system itself
does not change for the application.
