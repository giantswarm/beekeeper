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
| image.registry | string | `"gsoci.azurecr.io"` |  |
| image.repository | string | `"giantswarm/beekeeper"` |  |
| image.tag | string | `""` |  |
| image.pullPolicy | string | `"IfNotPresent"` |  |
| serve.issuer | string | `""` |  |
| serve.clientIDs | list | `[]` |  |
| serve.organization | string | `""` |  |
| serve.teams | object | `{}` |  |
| serve.supervisors | object | `{}` |  |
| serve.muster | string | `""` |  |
| serve.kagent | object | `{}` |  |
| serve.people | object | `{}` |  |
| serve.channels | object | `{}` |  |
| serve.gateway.url | string | `""` |  |
| serve.gateway.tokenFile | string | `"/var/run/secrets/klaus-gateway/token"` |  |
| serve.gateway.answerTool | string | `"x_beekeeper_note_answer"` |  |
| watch.interval | string | `"30s"` |  |
| database.secretName | string | `""` |  |
| database.secretKey | string | `"uri"` |  |
| teamNamespaces.create | bool | `true` |  |
| rbac.readerGroups | list | `[]` |  |
| serviceAccount.annotations | object | `{}` |  |
| service.port | int | `8080` |  |
| resources.requests.cpu | string | `"50m"` |  |
| resources.requests.memory | string | `"64Mi"` |  |
| resources.limits.memory | string | `"256Mi"` |  |
| podSecurityContext.runAsNonRoot | bool | `true` |  |
| podSecurityContext.runAsUser | int | `1000` |  |
| podSecurityContext.runAsGroup | int | `1000` |  |
| podSecurityContext.seccompProfile.type | string | `"RuntimeDefault"` |  |
| securityContext.allowPrivilegeEscalation | bool | `false` |  |
| securityContext.readOnlyRootFilesystem | bool | `true` |  |
| securityContext.capabilities.drop[0] | string | `"ALL"` |  |
| nodeSelector | object | `{}` |  |
| tolerations | list | `[]` |  |
| affinity | object | `{}` |  |
