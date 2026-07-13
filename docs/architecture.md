# Architecture

## V1 Request Flow

```txt
User
  |
Cloudflare DNS
  |
Oracle public IP
  |
Traefik on Oracle
  |
WireGuard overlay
  |
Nomad allocation on Oracle or worker VPS
```

For the first real target, Oracle is:

```txt
public IP: 140.245.5.201
ssh user: ubuntu
OCI private IP: 10.0.0.237
WireGuard overlay IP: 10.44.0.1
```

## Why Central Ingress First?

V1 keeps public ingress on Oracle because it is easier to secure and reason about:

- one public HTTP/HTTPS entrypoint
- one place for TLS and routing
- workers do not need public app ports
- Cloudflare records do not need to change on every reschedule

The tradeoff is that Oracle remains the bandwidth and ingress CPU bottleneck. Per-node ingress can be added later once the pool needs it.

## Why Nomad Without Consul?

Nomad has native service discovery and Traefik has a Nomad provider, so Consul is unnecessary for the first version. Dropping Consul reduces:

- ACL surface
- gossip/TLS setup
- daemon count on small VPS nodes
- failure modes

Consul can be introduced later if the project needs KV storage, Connect, or multi-datacenter features.

## Node States

- `ready`: node may receive new placements.
- `frozen`: Nomad scheduling eligibility is disabled; existing allocations stay.
- `draining`: Nomad drain is active and allocations are being migrated or stopped.
- `reserved`: an empty worker is owned exclusively by one project and remains Nomad-ineligible.

The Oracle agent reconciles `frozen` and `draining` from Nomad whenever the hosted dashboard requests status. These are scheduler states, not presentation-only labels.

## Project Reservations

Reservations are generic capacity ownership, independent of application type or toolchain:

1. The operator chooses any registered worker and supplies a safe project ID.
2. The agent refuses control-plane nodes and workers with active allocations.
3. The agent disables the real Nomad node's scheduling eligibility.
4. The Oracle state records `reserved_for: <project>`.
5. The project can use the whole worker through its existing SSH access without sharing it with Myprod workloads.
6. Release clears ownership but keeps the node frozen. A separate, confirmed Unfreeze action is required before Nomad can reuse it.

This machine boundary is appropriate for projects that install host packages, manipulate network namespaces, or need privileged runtimes. A reservation protects other pool nodes; it cannot prevent a project administrator from damaging its own reserved worker.

## Operator Flow

V1 is intentionally SSH-first:

1. `poolctl bootstrap-control-plane --apply` installs Oracle's base runtime.
2. `poolctl control-plane status` verifies Nomad, Traefik, WireGuard, nodes, and jobs.
3. `poolctl app deploy <app>` renders the Nomad job, copies it to Oracle, and runs it through Nomad's WireGuard-bound HTTPS API.
4. Later worker-node commands will add more Nomad clients behind the same Oracle ingress.

## WireGuard Lifecycle

The intended implementation:

1. Control plane owns the overlay CIDR, default `10.44.0.0/24`.
2. A joining node generates its own private key.
3. `poolctl` reads the node public key over SSH.
4. The control plane assigns the next unused overlay IP.
5. The control plane writes the peer entry.
6. The joining node receives Oracle peer details and its assigned IP.
7. Removed nodes have their peer entry removed and their IP marked retired.
