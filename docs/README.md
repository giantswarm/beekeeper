# beekeeper documentation

The [README](../README.md) is the front page. Everything else lives here, one topic per page.
A change that adds or changes behaviour documents it on its topic page.

## Getting started

- [How beekeeper works](overview.md)
- [Install](install.md)
- [Commands](commands.md)
- [Configuration](configuration.md) and its [example](examples/config.yaml)

## Running a desk

- [The supervisor loop](supervisor-loop.md)
- [The merge gate](merge-gate.md)
- [The guide and the desk](guide.md)
- [Beekeeper and the desk](desk-boundary.md): what belongs in the product, what in a desk
- [Agents and their turns](agents.md)
- [Worker sessions](worker-sessions.md)
- [omp, agy and codex agents](harnesses.md)
- [Capacity, models and subscriptions](capacity.md)
- [Notifications and reports](notifications.md)
- [Session reports and state snapshots](session-reports.md)
- [Session transcripts](transcripts.md)
- [Delivery cycle and work in progress](delivery.md)
- [Disk](disk.md)

## Remote hosts and the central instance

- [Remote hosts and the central instance](remote-hosts.md)
- [SSH execution hosts for agents](ssh-execution-hosts.md) (design)
- [Operating SSH execution hosts](ssh-execution-hosts-operations.md)
- [MCP tools for SSH workers](remote-mcp.md)
- [Remote build slots](remote-builds.md)
- [Remote worker resource retirement](remote-worker-cleanup.md)
- [Coordinator policy bundles](coordinator-policy-bundles.md)
- [Fleet watch](fleet-watch.md)
- [The feed and the resources of `beekeeper serve`](feed.md)
- [The tools](tools.md) of `beekeeper serve` (generated)

## Safety

- [Threat model](threat-model.md)
- [Secrets](secrets.md)
- [GitHub Apps](github-apps.md)
- [The agent sandbox](sandbox.md)
- [Folders, deletes and guards](safety.md)
- [Clusters](clusters.md)
- [The browser](browser.md)
- [Short-lived API reads](api-reads.md)
- [Branch protection reads](branch-protection.md)
- [Security scans and Dependabot alert state](security-scans.md)

## Updates and recovery

- [Update recovery protocol](update-recovery.md)
- [Canary update gate](canary-update-gate.md)

## Development

- [Developing on beekeeper](development.md)
- [Behavioral scenarios](scenarios.md)
- [The scenario's GitHub](scenarios-github.md)
- [Performance budgets](performance.md)
- [Standstill replays](standstill-replays.md)

## Proofs and plans

- [Supervisor state-lock proof](state-lock-proof.md)
- [Temporary sandbox proof policy](sandbox-proof-policy.md)
- [Browse upload compatibility check](browse-upload-live-proof.md)
- [Remote Codex lifecycle evidence](proofs/2281-codex-execution.md)
- [Alert relevance after a release rollout](plans/1896-rollout-alert-relevance.md)
- [Remote workers with coordinator parity](plans/2078-remote-workers.md)
- [Release containment and loop recovery](plans/2231-release-containment.md)
