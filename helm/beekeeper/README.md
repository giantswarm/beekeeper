# beekeeper

The beekeeper engine - observes a GitHub project board and its pull requests, decides deterministically and records every decision; a separate webhook receiver feeds it through a Postgres inbox

## CRDs

The chart's CustomResourceDefinitions are in `crds/`. Helm installs them with the release but never
upgrades or deletes them, so a new chart version's schema would not reach the cluster on its own.

With Flux, let the HelmRelease create and replace them on install and on every upgrade:

```yaml
apiVersion: helm.toolkit.fluxcd.io/v2
kind: HelmRelease
spec:
  install:
    crds: CreateReplace
  upgrade:
    crds: CreateReplace
```

With plain Helm, apply them before every upgrade: `kubectl apply --server-side -f helm/beekeeper/crds/`.

**Homepage:** <https://github.com/giantswarm/beekeeper>

## Maintainers

| Name | Email | Url |
| ---- | ------ | --- |
| Giant Swarm | <team-bumblebee@giantswarm.io> |  |

## Source Code

* <https://github.com/giantswarm/beekeeper>

## Values

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| fullnameOverride | string | `""` | The prefix of every object's name. Empty: the release's name. |
| image.registry | string | `"gsoci.azurecr.io"` | The registry of the beekeeper image. |
| image.repository | string | `"giantswarm/beekeeper"` | The repository of the beekeeper image. |
| image.tag | string | `""` | The image tag. Empty: the chart's appVersion. |
| image.pullPolicy | string | `"IfNotPresent"` | The image pull policy. |
| imagePullSecrets | list | `[]` | The names of the Secrets every pod pulls its image with. |
| github.apiURL | string | `"https://api.github.com"` | GitHub's REST API (GraphQL at <apiURL>/graphql). |
| github.app.secretName | string | `""` | The Secret holding the GitHub App's id, its installation's id and its private key. Empty: <fullname>-github-app. |
| github.app.appIDKey | string | `"app-id"` | The key of the App's id. |
| github.app.installationIDKey | string | `"installation-id"` | The key of the id of the App's installation on the org. |
| github.app.privateKeyKey | string | `"private-key.pem"` | The key of the App's private key (PEM). |
| webhook.secretName | string | `""` | The Secret holding the GitHub App's webhook secret, which the receiver checks X-Hub-Signature-256 with. Empty: <fullname>-webhook. |
| webhook.secretKey | string | `"secret"` | The key of the webhook secret. |
| jev.enabled | bool | `true` | Ask JEV at the decision points in shadow mode. Off: the engine asks no JEV and mounts no key. |
| jev.secretName | string | `""` | The Secret holding the JEV API key. Empty: <fullname>-jev. |
| jev.secretKey | string | `"key"` | The key of the JEV API key. |
| postgres.cluster | string | `""` | The CloudNativePG Cluster, which the host, the engine's Secret and the Postgres network policy derive from. Empty: <fullname>-db. |
| postgres.host | string | `""` | The Postgres host. Empty: <cluster>-rw, the Cluster's read-write Service. |
| postgres.port | int | `5432` | The Postgres port. |
| postgres.database | string | `"beekeeper"` | The database, owned by the role engine. The migrations grant the roles engine and receiver their privileges. |
| postgres.sslMode | string | `"require"` | libpq's sslmode. CloudNativePG serves TLS with its own CA. |
| postgres.engine.secretName | string | `""` | The basic-auth Secret of the role engine, the owner (it migrates). Empty: <cluster>-app, CloudNativePG's. |
| postgres.engine.usernameKey | string | `"username"` | The key of the role's name. |
| postgres.engine.passwordKey | string | `"password"` | The key of the role's password. |
| postgres.receiver.secretName | string | `""` | The basic-auth Secret of the role receiver, which may only insert into the inbox. Empty: <cluster>-receiver. |
| postgres.receiver.usernameKey | string | `"username"` | The key of the role's name. |
| postgres.receiver.passwordKey | string | `"password"` | The key of the role's password. |
| engine.replicas | int | `2` | Engine replicas. One leads (leader election) and decides; every one serves metrics. |
| engine.leaderElectionID | string | `""` | The Lease the replicas elect their leader with. Empty: <fullname>-engine. |
| engine.logLevel | string | `"info"` | The log level (debug, info, error). |
| engine.ports.metrics | int | `8080` | The metrics port (/metrics). |
| engine.ports.health | int | `8081` | The health port (/healthz, /readyz). |
| engine.resources | object | `{"limits":{"memory":"512Mi"},"requests":{"cpu":"50m","memory":"128Mi"}}` | The engine container's resources. |
| engine.migrate.resources | object | `{"limits":{"memory":"128Mi"},"requests":{"cpu":"10m","memory":"32Mi"}}` | The resources of the init container that runs `beekeeper migrate`. |
| engine.extraArgs | list | `[]` | Further arguments of `beekeeper engine`. |
| engine.extraEnv | list | `[]` | Further environment variables of the engine container. |
| engine.podAnnotations | object | `{}` | Annotations of the engine pods. |
| engine.podLabels | object | `{}` | Further labels of the engine pods. |
| engine.nodeSelector | object | `{}` | The engine pods' node selector. |
| engine.tolerations | list | `[]` | The engine pods' tolerations. |
| engine.affinity | object | `{}` | The engine pods' affinity. Empty: a soft anti-affinity that spreads the replicas over nodes. |
| engine.topologySpreadConstraints | list | `[]` | The engine pods' topology spread constraints. |
| receiver.replicas | int | `2` | Receiver replicas. Each stores a verified delivery in the Postgres inbox before it answers. |
| receiver.path | string | `"/github/webhook"` | The only path the receiver answers, with POST. |
| receiver.maxBodyBytes | int | `10485760` | The largest body the receiver accepts, in bytes (inclusive). |
| receiver.maxInFlightBytes | int | `41943040` | The bodies a receiver holds at once, in bytes by their declared size (at least maxBodyBytes): a delivery that doesn't fit is answered 503 before its body is read, so unsigned bodies can't exhaust the memory limit. Default: four of the largest. |
| receiver.projectNodeIDs | list | `[]` | Required in production: the node ids of the boards (ProjectV2, PVT_...) the engine observes. Empty: every board's item deliveries are stored. |
| receiver.logLevel | string | `"info"` | The log level (debug, info, error). |
| receiver.ports.webhook | int | `8080` | The webhook port, the only one the public route reaches. |
| receiver.ports.metrics | int | `8081` | The metrics and health port (/metrics, /healthz, /readyz). |
| receiver.service.port | int | `8080` | The receiver Service's port, the backend of the webhook route. |
| receiver.service.annotations | object | `{}` | Annotations of the receiver Service. |
| receiver.resources | object | `{"limits":{"memory":"128Mi"},"requests":{"cpu":"10m","memory":"32Mi"}}` | The receiver container's resources. |
| receiver.extraArgs | list | `[]` | Further arguments of `beekeeper receiver`. |
| receiver.extraEnv | list | `[]` | Further environment variables of the receiver container. |
| receiver.podAnnotations | object | `{}` | Annotations of the receiver pods. |
| receiver.podLabels | object | `{}` | Further labels of the receiver pods. |
| receiver.nodeSelector | object | `{}` | The receiver pods' node selector. |
| receiver.tolerations | list | `[]` | The receiver pods' tolerations. |
| receiver.affinity | object | `{}` | The receiver pods' affinity. Empty: a soft anti-affinity that spreads the replicas over nodes. |
| receiver.topologySpreadConstraints | list | `[]` | The receiver pods' topology spread constraints. |
| serviceAccount.engineAnnotations | object | `{}` | Annotations of the engine's ServiceAccount. |
| serviceAccount.receiverAnnotations | object | `{}` | Annotations of the receiver's ServiceAccount, which may do nothing in Kubernetes. |
| podSecurityContext | object | `{"runAsGroup":1000,"runAsNonRoot":true,"runAsUser":1000,"seccompProfile":{"type":"RuntimeDefault"}}` | The pod security context of every pod. |
| securityContext | object | `{"allowPrivilegeEscalation":false,"capabilities":{"drop":["ALL"]},"readOnlyRootFilesystem":true}` | The security context of every container. |
| podMonitor.enabled | bool | `true` | Create a PodMonitor (prometheus-operator) for the engine and one for the receiver. |
| podMonitor.labels | object | `{}` | Labels a Prometheus selects PodMonitors by, e.g. release: kube-prometheus-stack. |
| podMonitor.interval | string | `"30s"` | The scrape interval. |
| prometheusRule.enabled | bool | `true` | Create the alert rules (a PrometheusRule). |
| prometheusRule.labels | object | `{}` | Labels a Prometheus selects rules by, e.g. release: kube-prometheus-stack. |
| prometheusRule.alertLabels | object | `{}` | Labels on every alert, e.g. cluster, layer and component. |
| prometheusRule.runbookURL | string | `""` | A runbook's URL. Each alert links <runbookURL>#<alert name in lower case>. Empty: no links. |
| prometheusRule.severities.invariantViolated | string | `"warning"` | BeekeeperInvariantViolated: an invariant is violated for 10 minutes. |
| prometheusRule.severities.invariantsMissing | string | `"warning"` | BeekeeperInvariantsMissing: the engine is up but exports no invariants. |
| prometheusRule.severities.reconcileErrors | string | `"warning"` | BeekeeperReconcileErrors: a reconciler fails for 15 minutes. |
| prometheusRule.severities.noLeader | string | `"warning"` | BeekeeperNoLeader: replicas are up, none leads. |
| prometheusRule.severities.engineDown | string | `"warning"` | BeekeeperEngineDown: no engine replica is scraped. |
| prometheusRule.severities.receiverDown | string | `"warning"` | BeekeeperReceiverDown: no receiver replica is scraped. |
| prometheusRule.severities.receiverUnavailable | string | `"warning"` | BeekeeperReceiverUnavailable: the receiver answers 503 for 10 minutes. |
| prometheusRule.severities.sourceDegraded | string | `"warning"` | BeekeeperSourceDegraded: a source is degraded for 30 minutes. |
| prometheusRule.severities.historyDropped | string | `"warning"` | BeekeeperHistoryDropped: history rows were dropped. |
| prometheusRule.watchdog.enabled | bool | `true` | An alert that fires while an engine replica leads, for a dead-man's switch. |
| prometheusRule.watchdog.labels | object | `{}` | Labels of the watchdog alert, e.g. the one your Alertmanager routes to the dead-man's switch. |
| prometheusRule.selectors.engine | string | `""` | The PromQL selector of the engine's series. Empty: its namespace and the job of its PodMonitor. |
| prometheusRule.selectors.receiver | string | `""` | The PromQL selector of the receiver's series. Empty: its namespace and the job of its PodMonitor. |
| ciliumNetworkPolicies.enabled | bool | `false` | Create CiliumNetworkPolicies for the engine, the receiver and (postgres.enabled) Postgres. A cluster with a default-deny policy needs them. |
| ciliumNetworkPolicies.authenticationMode | string | `"required"` | The authentication mode of every fromEndpoints ingress rule (Cilium mutual authentication). Empty: none. |
| ciliumNetworkPolicies.probeEntities | list | `["host"]` | The entities kubelet probes come from. |
| ciliumNetworkPolicies.prometheus.selector | object | `{"app.kubernetes.io/name":"prometheus","io.kubernetes.pod.namespace":"monitoring"}` | The Prometheus pods' labels. Empty: no scrapes. |
| ciliumNetworkPolicies.gateway.selector | object | `{"io.kubernetes.pod.namespace":"envoy-gateway-system"}` | The pods of the gateway the webhook route goes through. Empty: no webhooks. |
| ciliumNetworkPolicies.dns.selector | object | `{"io.kubernetes.pod.namespace":"kube-system","k8s-app":"kube-dns"}` | The labels of the cluster's DNS pods. |
| ciliumNetworkPolicies.dns.ports | list | `[53,1053]` | The DNS pods' ports (UDP and TCP). |
| ciliumNetworkPolicies.engine.fqdns | list | `["api.github.com","api.typesafe.ai"]` | The hosts the engine reaches over HTTPS: GitHub's API and JEV. |
| ciliumNetworkPolicies.postgres.enabled | bool | `true` | Create the policy of the Postgres pods too. |
| ciliumNetworkPolicies.postgres.selector | object | `{}` | The labels of the Postgres pods. Empty: cnpg.io/cluster: <postgres.cluster>. |
| ciliumNetworkPolicies.postgres.operatorSelector | object | `{"app.kubernetes.io/name":"cloudnative-pg","io.kubernetes.pod.namespace":"cnpg-system"}` | The operator's pods. |
| ciliumNetworkPolicies.postgres.statusPort | int | `8000` | The instance manager's status port (the operator and kubelet probes). |
| ciliumNetworkPolicies.postgres.metricsPort | int | `9187` | The instance manager's metrics port. |
