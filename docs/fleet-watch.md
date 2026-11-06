# Fleet watch

`beekeeper serve` can read the fleet through its central muster. Enable it
only after the Dex identity has read grants on every installation:

```yaml
serve:
  # Set muster, issuer and identity as for `serve identity`.
  fleetWatch:
    kubernetes: x_kubernetes_list
    kagent: x_kagent_list_agent_instances
    alertmanager: x_prometheus_get_alertmanager_alerts
    concurrency: 4
    timeout: 30s
    minCallInterval: 100ms
watch:
  interval: 30s
  stuckAfter: 15m
```

Tool names are the registered muster names. Leaving all three unset disables
the watch; a partially configured watch refuses startup. Durations and
concurrency default to the values above. Concurrency is limited to 32.

Each round lists the coordination store's Environments. Every backend call
carries `cluster`, from `spec.musterCluster` or `spec.installation`. Kubernetes
list calls carry `resourceType` and `allNamespaces: true`. The kagent tool must
return every state in `instances`, with `id` (or `agent_instance_id`), `state`
and `current_task`. The Alertmanager tool returns `alerts`, including labels,
fingerprint and tenant. Kubernetes tools return complete `items` lists.
Malformed, missing and explicitly truncated lists are failures, never empty
snapshots. Backends must allow full lists within their response limits.

Workers process installations concurrently and each installation's sources
sequentially. The global minimum call interval paces starts across backends;
set it and concurrency to the deployment's rate limits. Calls have deadlines.
Rounds never overlap or catch up: the next begins one `watch.interval` after
the previous completes. Results are applied in installation order.

The watch uses only its own exchanged Dex token for muster. It never uses a
person's token or presents its projected ServiceAccount token to muster.
Backend read failures are logged and keep the last successful baseline.

Changes are Kubernetes Events on the Environment, and therefore appear in
`beekeeper://feed` and its subscriptions. Failed, stuck and disappeared agents,
admission failures and stale leases are deduplicated within the running watch.
A task unchanged in a working state for `watch.stuckAfter` is stuck. A stale
remote lease is reported when its previously observed agent disappears; it is
not automatically released. Local session liveness belongs to the local watch.
Alert transitions use the local watch's flap damping and two-reading resolution
rules, with recent merges and lease claims (30 minutes, at most 20 context
entries). Capacity and workload health come only from alerts.

HelmRelease readings drive the existing lane readiness and settle timeout
rules. Cluster API readings include control planes and worker pools and use
the existing upgrade detector. Upgrade holds are reconciled individually;
a failed CAPI source leaves all upgrade holds untouched. Expired grants are
removed per Environment using the existing release-time TTL rule.

Run focused coverage with:

```sh
go test ./cmd -run TestFleet -count=1
go build ./...
```

The muster fixture exercises two installations and a failure in the second,
including Dex exchange and the central feed. It is not a live agentlab proof.
Before enabling this in production, verify a failure in a second lab cluster
through the first cluster's muster, and verify the identity is refused writes.
The watch's agent and alert baselines are in memory; a process restart starts
new baselines. Run one fleet watch per coordination store.
