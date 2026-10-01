# Findings for a future insta-sandbox / insta-vm

These are POC findings and proposed requirements, not requests to modify insta-compute. No compute source changes are part of this fork.

## What the POC establishes

The existing E2B API can delegate sandbox lifecycle through its existing gRPC orchestrator boundary to an external runtime. A separate process-protocol adapter lets an unmodified E2B SDK run foreground commands. This validates two integration boundaries; it does not establish compatibility with all E2B features or performance claims.

## Gaps and proposed contracts

| Area | Observed POC constraint | Proposed future runtime capability |
|---|---|---|
| Sandbox lifecycle | Compute exposes long-lived services; bridge runs an always-on worker and polls readiness | Explicit create/status/delete with stable runtime IDs and readiness |
| Durable leases | Timeout lives in E2B/bridge; while both are down a service can continue running | Runtime-enforced expires_at and atomic lease extension |
| Exactly-once creation | Name preflight, local journal and conflict handling cover ordinary retries, but cannot fully fence delayed remote creates after client timeout | Idempotency key, operation status and ownership/fencing token |
| Process protocol | `/exec` buffers a result; bridge emulates only `Process.Start` | Process ID, streamed output, stdin, wait, signal, cancellation and reconnect |
| Execution identity | POC supports only root and shell-based cwd/env | Explicit uid/gid/user and workdir/env contract |
| Filesystem | No envd filesystem service or persistent sandbox disk semantics implemented | File read/write/stat/list/watch and explicit ephemeral/persistent disk lifecycle |
| Networking | Port 0 workers; SDK process URL overridden to one local listener | Per-sandbox authenticated routing, port forwarding and enforceable egress/ingress policy |
| Snapshot/templates | E2B API/schema retain Firecracker/kernel/envd assumptions | Image/template resolver and explicit snapshot capabilities, with durable snapshot IDs |
| Isolation | POC consumes compute's existing service isolation; no E2B-equivalent isolation verification | Documented sandbox trust model, resource enforcement, network isolation and teardown guarantees |
| Limits | Four admitted sandboxes by default; static capacity and one configured image | Capacity/admission API, quotas, supported CPU/RAM/disk sizes |
| Events/observability | Polling and local logs; E2B log/metric endpoints are blocked | Lifecycle events, exit reason, usage/metrics and correlated runtime/sandbox IDs |
| Control-plane recovery | Single bridge journal, local Postgres/Redis; bridge restart tested, full distributed recovery untested | Durable runtime inventory, reconciliation, HA ownership and crash testing |

## E2B coupling that remains

`seed-template.sql` inserts one successful build so the native API can resolve a template. `kernel_version=external-runtime`, `firecracker_version=v0.0.0` and `envd_version=0.2.0` are compatibility placeholders required by the existing schema and parsing/SDK behavior. They do not describe a Firecracker VM or a deployed envd binary. Disk sizes of zero in the seed are placeholders, not enforced runtime disk quotas.

The bridge advertises an orchestrator node and synthetic admission capacity so E2B's existing scheduler can select it. Multi-node placement, utilization-aware scheduling and arbitrary E2B templates are untested. A production design should introduce explicit runtime capabilities and image references instead of relying on these placeholders.

The API allowlist is enabled only by `RUNTIME_BRIDGE_POC=true`. Without it, unsupported operations can enter upstream workflows before the bridge rejects an RPC. The guard deliberately limits the whole local API surface, including dashboards, template APIs and metrics, to the POC's implemented routes.

## Limits of failure handling

Pending creates/deletes are retried on bridge startup and reconciliation; failed cleanup remains journaled. The journal must survive until deletion is confirmed. Local state loss, a delayed backend create that materializes after cleanup, or an externally recreated service with the same name cannot be made safe solely by this adapter. A dedicated tenant/branch avoids unrelated resources, but does not replace idempotent runtime operations and ownership IDs.

Commands are buffered and limited to 60 seconds. The Start wire request does not reliably distinguish SDK `background=True`; such calls do not have usable background-process semantics here. HTTP cancellation and timeout may leave a process running until the sandbox is deleted. No production claim is made for throughput, startup latency, snapshot performance, full SDK compatibility, or isolation guarantees.

## Swapping the backend

Keep the E2B-facing `bridge` package and replace `compute.Client` with an implementation of `backend.Runtime`. This is sufficient for the POC's lifecycle and buffered exec subset. Expanding to full sandbox functionality requires extending that neutral contract and the protocol adapter together. Prefer a native process/file agent or equivalent streaming runtime APIs, durable leases, idempotent operations and an explicit capability model before adding pause/resume and snapshots.
