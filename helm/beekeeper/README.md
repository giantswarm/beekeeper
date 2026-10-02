# beekeeper

beekeeper serve, the central instance - leases, holds, merge lanes, notes and the agent roster as MCP tools behind muster, over beekeeper.giantswarm.io resources

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
| affinity | object | `{}` |  |
| database.secretKey | string | `"uri"` |  |
| database.secretName | string | `""` |  |
| image.pullPolicy | string | `"IfNotPresent"` |  |
| image.registry | string | `"gsoci.azurecr.io"` |  |
| image.repository | string | `"giantswarm/beekeeper"` |  |
| image.tag | string | `""` |  |
| nodeSelector | object | `{}` |  |
| podSecurityContext.runAsGroup | int | `1000` |  |
| podSecurityContext.runAsNonRoot | bool | `true` |  |
| podSecurityContext.runAsUser | int | `1000` |  |
| podSecurityContext.seccompProfile.type | string | `"RuntimeDefault"` |  |
| rbac.readerGroups | list | `[]` |  |
| resources.limits.memory | string | `"256Mi"` |  |
| resources.requests.cpu | string | `"50m"` |  |
| resources.requests.memory | string | `"64Mi"` |  |
| securityContext.allowPrivilegeEscalation | bool | `false` |  |
| securityContext.capabilities.drop[0] | string | `"ALL"` |  |
| securityContext.readOnlyRootFilesystem | bool | `true` |  |
| serve.channels | object | `{}` |  |
| serve.clientIDs | list | `[]` |  |
| serve.gateway.answerTool | string | `"x_beekeeper_note_answer"` |  |
| serve.gateway.tokenFile | string | `"/var/run/secrets/klaus-gateway/token"` |  |
| serve.gateway.url | string | `""` |  |
| serve.issuer | string | `""` |  |
| serve.kagent | object | `{}` |  |
| serve.muster | string | `""` |  |
| serve.organization | string | `""` |  |
| serve.people | object | `{}` |  |
| serve.supervisors | object | `{}` |  |
| serve.teams | object | `{}` |  |
| service.port | int | `8080` |  |
| serviceAccount.annotations | object | `{}` |  |
| teamNamespaces.create | bool | `true` |  |
| tolerations | list | `[]` |  |
| watch.interval | string | `"30s"` |  |
