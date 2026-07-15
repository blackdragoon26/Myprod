# Myprod Operator FAQ

This document explains the production concepts exposed by the hosted Myprod
dashboard. Use the agent runbook for command-level infrastructure procedures.

## What Is `sample-api`?

`sample-api` is the pool's baseline smoke workload. It runs Traefik's small
`whoami` image and proves that the complete production path works:

1. Nomad accepts and schedules a container job.
2. The allocation joins the expected container network.
3. Traefik discovers the job and routes Oracle ingress traffic to it.
4. DNS and TLS reach the Oracle control plane.
5. Public HTTP and HTTPS smoke checks return `200 OK`.

It is not a framework, database, or dependency for other projects. Keep it
while it is useful as a known-good control workload. Removing it should be an
explicit operational decision because the current smoke endpoint uses its
public route.

## What Is A Managed Application?

A managed application is a public container image represented in Myprod's
configuration and deployed as a Nomad job. It has:

- a stable app name;
- a public image reference;
- a public hostname;
- one container port and HTTP health path;
- an exact target node;
- CPU and memory reservations.

Several managed applications may share a node. Nomad accounts for their
reservations when placing workloads.

## What Is A Project Reservation?

A reservation assigns an entire empty worker to one project and disables
shared Nomad scheduling on that worker. Use it only for work that needs the
host itself, such as system package installation, privileged networking,
Mininet namespaces, kernel changes, or a large hardware-model toolchain.

Do not reserve a whole worker for an ordinary containerized backend. Register
the backend as a managed application so other workloads can use the remaining
capacity.

## What Must Be Ready Before App Registration?

Verify all of the following:

1. The image is immutable and publicly pullable without credentials.
2. The image supports the target node architecture.
3. The process listens on `0.0.0.0`, not only `127.0.0.1`.
4. The declared container port is correct.
5. The health path is unauthenticated and returns HTTP 2xx.
6. Either managed Netlify DNS is configured on Oracle, or the externally
   managed hostname already resolves to `140.245.5.201`.
7. The target node is joined, eligible, not draining, and not reserved.
8. Resource Utilization shows enough CPU, memory, and disk headroom.

Prefer an image tag containing the source commit and retain the corresponding
digest. Do not deploy a mutable `latest` tag when reproducibility matters.

## What Does Each Application Field Mean?

- **Name** is the stable Myprod and Nomad identifier.
- **Domain** is the public hostname without `https://`, a path, or a port.
- **Container image** is a public registry reference such as
  `ghcr.io/owner/project:git-sha`.
- **Target node** is the exact Nomad client where the workload will run.
- **Container port** is the port the process opens inside the container.
- **CPU reservation** is scheduler capacity in MHz.
- **Memory reservation** is scheduler capacity in MB.
- **Health path** is the HTTP path Nomad checks before deployment is healthy.
- **Create and verify DNS automatically** asks Oracle to manage an exact
  Netlify A record for the hostname. Leave it off for externally managed DNS.

CPU and memory values are reservations, not live process-usage measurements.

## Registration Versus Deployment

**Register application** validates and persists configuration. It does not
start or replace a container. With managed DNS enabled, registration also
creates or verifies the A record and records one of `ready`, `pending`,
`conflict`, `error`, or `unconfigured`.

**Deploy** renders the Nomad job, submits it to the live scheduler, and waits
for a healthy allocation. It may replace a running allocation and therefore
requires a separate confirmation. Deployment is blocked until managed DNS is
`ready`; **Check DNS** retries creation or propagation verification.

After deployment, refresh the dashboard and verify:

- the app state is `deployed`;
- Nomad and Traefik remain active;
- the expected allocation runs on the selected node;
- the health path and public HTTPS route return HTTP 2xx.

## Resource Utilization

The dashboard reads actual client statistics from Nomad for every node:

- CPU percentage currently consumed across the host;
- memory used, available, and percentage consumed;
- root-disk used, available, and percentage consumed.

These are point-in-time host measurements. Short CPU spikes can occur between
refreshes. Capacity decisions should consider sustained behavior and all app
reservations, not one sample alone.

## Locked And Cached Views

The agent token authorizes live status and mutation operations. When locked,
all powerful controls are disabled.

After one successful status refresh, the browser stores a sanitized snapshot
containing the visible app inventory, node state, system status, and resource
measurements. It does not store SSH usernames, SSH key paths, or an agent token
inside the snapshot. The header shows when the snapshot was captured and the
resource section is labeled **Cached snapshot - not live**.

Locking removes the saved agent token but deliberately keeps the last snapshot
for visibility. Browser storage is local to that browser profile. Cached state
can be stale and must never be used as proof that a node is currently safe to
deploy, drain, release, or unfreeze.

Public ingress smoke checks do not require the agent token and continue to
refresh while the dashboard is locked.

## Node Actions

- **Freeze** makes a node ineligible for new allocations while existing
  allocations continue.
- **Unfreeze** returns a clean, non-draining, unreserved node to scheduling.
- **Drain** migrates or stops allocations and prevents new placement.
- **Cancel drain** stops draining but leaves the node frozen for inspection.
- **Reserve** assigns an empty worker exclusively to one project.
- **Release** clears project ownership but leaves the worker frozen.

Read every confirmation. A confirmation reduces operator mistakes but does not
replace preflight checks.

## Adding A VPS To The Pool

The hosted Vercel dashboard controls nodes only after they are joined. Initial
registration requires an SSH-capable operator environment because private SSH
keys must not be uploaded to Vercel.

The general process is:

1. Create the VPS at the cloud provider.
2. Restrict its cloud firewall.
3. Prepare the non-root SSH operator account and passwordless sudo.
4. Reconcile local and Oracle pool state.
5. Register and join the node through the local Myprod operator flow.
6. Verify WireGuard, Nomad registration, agent status, and public smoke checks.
7. Use the hosted dashboard for ongoing visibility and controls.

Follow [agent-runbook.md](agent-runbook.md) and the provider-specific worker
guide. Never paste a private SSH key into the hosted dashboard.

## Current Limitations

The app form intentionally does not support:

- secrets or environment variables;
- private registry credentials;
- persistent volumes;
- arbitrary Nomad HCL;
- app editing or deletion;
- cloud-instance creation;
- DNS record deletion, non-A records, or DNS providers other than Netlify.

Do not encode credentials in image names, hostnames, or health paths. These
features require dedicated encrypted storage, redaction, and lifecycle design.
