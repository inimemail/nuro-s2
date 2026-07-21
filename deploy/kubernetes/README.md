# Automatic Scaling

This directory contains the KEDA policy for a paired Go + Rust Edge gateway
Pod. Pairing is deliberate: Edge uses `127.0.0.1` for prepare/retry/complete/
abort, so every in-memory lease returns to its owner during scaling. Both
containers expose `/metrics`, `/readyz`, and a health endpoint.

`drain-patch.yaml` is a strategic-merge patch for the deployment that owns your
actual image names, database/Redis Secrets, resource requests, and node pools;
it is intentionally not a standalone Deployment and must not be applied by
itself. Apply it through the deployment repository's Kustomize overlay, then
apply `service-monitor.yaml`, `autoscaling.yaml`, and `ingress.yaml`. Keeping
provider credentials and image coordinates outside this source repository is a
deployment boundary, not an application default.

The paired Pod must set `SUB2API_EDGE_CONTROL_BASE_URL=http://127.0.0.1:8080`
and `SUB2API_EDGE_GO_BASE_URL=http://127.0.0.1:8080`. Go must set
`gateway.openai_edge_rs.listen_addr=127.0.0.1:18080`. The external Service sends
OpenAI streaming paths to the Edge container port; Edge falls back to the Go
container on localhost when the frozen Go scheduling contract says so.
The target values in `autoscaling.yaml` are starting points only and must be
replaced with measured per-node limits from the load test.

Scale-out is deliberately fast and scale-in only begins after the configured
stabilization window. KEDA then sends the normal Kubernetes termination signal;
the application stays in `draining` for a short, bounded window so existing
SSE/WS streams and internal lease callbacks can finish without delaying
replacement for many minutes.
`/internal/runtime/drain` is available for an external drain controller and is
protected by `X-Sub2API-Edge-Secret`. Edge uses `/internal/drain` with the same
secret.

## Warm headroom and scale signals

Autoscaling maintains 30% already-running headroom by targeting at most 70% of
the load-tested per-Pod stream/RPS capacity. This headroom absorbs the interval
between a burst and a new Pod becoming ready; autoscaling itself cannot satisfy
a sub-second first-token budget while images, connections, and snapshots warm.
When headroom is exhausted, the bounded Edge queue rejects excess work instead
of creating a multi-minute wait.

Only controllable stages trigger scale-out: active streams, Edge queue depth,
Go request pressure, CPU/GC/FD pressure when exported by the platform, and
Redis claim p99. `upstream_header_ms`, end-to-end TTFT, and provider latency are
diagnostic signals only. An upstream circuit-open signal must suppress the
corresponding pool's external autoscaler before that signal is enabled. The
hard `maxReplicaCount` is the cost ceiling; exceeding it means shedding load,
not continuing to add Pods.

Every autoscaler change is retained in Kubernetes HPA/KEDA events. Production
log collection must retain the trigger query, observed value, old replica
count, desired replica count, and the hard-ceiling/circuit state for audit.

Pod startup is jittered by up to 15 seconds for both Go and Edge so a large
scale-out wave does not register, open Redis pools, and hydrate snapshots at
the same instant. Scheduler startup uses the persisted Redis snapshot as its
baseline, then replays the existing ordered outbox watermark before relying on
incremental events; it never rebuilds the snapshot on a request path.

Drain is bounded by `terminationGracePeriodSeconds` and each process's drain
timeout. Readiness is removed immediately, settlement callbacks remain
available during drain, and Kubernetes terminates a Pod that exceeds the hard
deadline so one abandoned client cannot block scale-in forever.

Redis admission Cells are not resized by HPA. Cell ownership is an append-only
account directory, not a hash ring: existing accounts remain on their Cell and
new accounts use newly provisioned Cells. Online account migration and Cell
scale-in are intentionally disabled until claim-side quiesce fencing can make
drain-and-commit atomic. The global scheduler still sees every account, so Cell
ownership never changes priority or cache affinity.

The repository includes the fixed ownership directory and safe scale-out
recommendation. Physical Redis provisioning is intentionally delegated to the
Redis Operator/cloud adapter configured by the deployment; without that
provider API no application can truthfully create a Redis instance on its own.
