---
name: alerts
description: Check alerts after a rollout or live proof on a beekeeper desk. Load before the post-roll or post-proof alert check.
---

# alerts — checking after a rollout or proof

After a rollout or live proof, take an alert snapshot:

```sh
KUBECONFIG="$HOME/.kube/config:$HOME/.local/state/beekeeper/kube/teleport.yaml" beekeeper alerts snapshot
```

Use this route, never Alertmanager/Mimir directly. Report the snapshot's findings
with the rollout or proof evidence; surface problems to the supervisor.
