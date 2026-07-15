# Application Onboarding Handoff

Use this contract when handing a project to another coding agent before adding
it to Myprod. The project agent prepares a deployable artifact; the Myprod
operator separately registers and deploys it.

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

If required environment variables, secrets, or persistent volumes are not
empty, stop and report that the current Myprod app form cannot safely represent
the workload.

## SpliDT Agent Context

Give the SpliDT agent the following task:

```txt
Prepare the SpliDT public showcase backend for ordinary Myprod managed-app
deployment. Do not install BMv2, Mininet, p4-guide, Open-P4Studio, or any
host-level package. Do not modify Myprod or any VPS.

Start from the current SpliDT showcase work and preserve its explicit evidence
boundaries. The lightweight public backend is showcase/server.py and the image
definition is showcase/Dockerfile. It must remain an unprivileged Linux AMD64
container listening on 0.0.0.0:8765. Use
/api/system/capabilities as the read-only HTTP health path.

Run the showcase Python tests, build the image for linux/amd64, start it
locally, call the health endpoint, and run one curated API smoke flow. Add a
GitHub Actions workflow that publishes immutable commit-tagged images to GHCR.
Do not use only latest. Report the exact image digest and verify an anonymous
docker pull works after the package is made public.

The public image must contain only the lightweight software-reference backend
and retained evidence projection. BMv2 live and Tofino-model tooling remain
outside this container. Generated run data is ephemeral under
/var/lib/splidt/runs; state that clearly. Do not add secrets. The current server
defaults to public CORS, so report that boundary rather than inventing an
environment variable that Myprod cannot yet inject.

Do not register or deploy the image in Myprod. Finish by returning the complete
Myprod handoff manifest from docs/application-onboarding.md.
```

Expected SpliDT values, subject to verification by that agent:

```txt
name: splidt-showcase
architecture: linux/amd64
container port: 8765
health path: /api/system/capabilities
recommended CPU MHz: 1000
recommended memory MB: 2048
ephemeral data behavior: generated runs are lost when the allocation is replaced
required environment variables: none for the current public-CORS release
required secrets: none
```

The image and digest are deliberately omitted until a CI publication run proves
them. DNS is an operator step; the recommended backend hostname is
`splidt-api.sankalpjha.dev`, pointing to Oracle ingress `140.245.5.201`.

