# MCP tools for SSH workers

The coordinator exposes selected tools from its MCP servers to managed remote Claude
and Codex workers. Configure `agents.remoteMCP` on the coordinator before starting
the worker:

```yaml
agents:
  remoteMCP:
    catalog:
      url: http://127.0.0.1:8090/mcp
      tools: [search, describe]
    connector:
      command: /usr/local/bin/example-connector
      args: [serve, --stdio]
      tools: [lookup]
```

A server uses either a Streamable HTTP URL or an absolute stdio command. The
command runs on the coordinator and uses its local connector configuration and
login. Configure an existing connector's stdio launcher here when it needs local
credentials; credentials are never included in the worker's MCP configuration.
Do not put credentials into URLs or command arguments.

Only exact tool names are exposed. An empty list exposes nothing; wildcards are
refused. Workers see `catalog__search`, `catalog__describe` and
`connector__lookup` under the `coordinator` MCP server. Discovery forwards the
upstream schemas, descriptions and annotations; tool results retain their MCP
content, structured content and error flags.

The runner installs one stdio proxy automatically when the coordinator has a
nonempty `remoteMCP` map at start. It uses the worker's existing authenticated SSH
callback route, execution identity and process ownership checks; no listening
port, forwarded agent, copied connector token or separate worker gateway
configuration is needed. Existing workers started without MCP must be started
anew to gain it. Hosts must run a version supporting `agents mcp-proxy`, then
refresh their policy with `beekeeper agents hosts update` before starting them.

Every call checks the current coordinator allowlist again, so removing an exposed
tool revokes access even if the harness cached its discovery result. Stopped,
completed, terminal or unconfigured executions cannot use the gateway. A GitHub
hold blocks the gateway too, since an exposed connector may call GitHub. Other
MCP server namespaces remain refused by the managed execution guard.

Upstream servers are initialized afresh for each discovery or tool call. Discovery
must succeed for all servers exposing tools; an unavailable upstream fails proxy
startup visibly. Tool calls are bounded and never retried automatically: after a
transport failure their side effects may already have happened. Server-initiated
sampling, elicitation, resources and prompts are not forwarded by this tool gateway.
