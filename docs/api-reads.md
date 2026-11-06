# Short-lived API reads

`beekeeper api-read example-api <service>/<method> --data '{}'` runs `grpcurl`
with a private, audience-bound service-account token. The default lifetime
is ten minutes; `--duration` accepts 10m through 15m (Kubernetes requires
at least ten minutes). Direct production
`kubectl create token` remains refused, including tokens for reader accounts.

An operator must provision a dedicated service account and configure the API
to authorize that identity for reads only, with no write methods. The API must
validate Kubernetes tokens for the configured audience. API authorization is
separate from Kubernetes RBAC: beekeeper cannot infer an external API's
permissions from a service account's name or Kubernetes roles. Do not configure
an existing administrator identity as a reader.

Add a named entry under `kube.apiReads` in the machine configuration:

```yaml
kube:
  apiReads:
    example-api:
      context: <production-context>
      namespace: <api-namespace>
      serviceAccount: <dedicated-reader>
      audience: <api-token-audience>
      endpoint: <TLS-endpoint-and-port>
      methods:
        - <fully-qualified-service>/<read-method>
```

Only exact configured methods can run. Supply the API's actual fully qualified
RPC names; wildcards and arbitrary grpcurl options are not supported. TLS
verification is mandatory for both Kubernetes and grpcurl; the API certificate
must be trusted by grpcurl's system trust store. Install `grpcurl` on PATH.

Before minting, beekeeper inspects all cluster role bindings and role bindings
across all namespaces, including grants to the account's standard groups. Every
applicable role must contain only get/list/watch verbs, with the exception
of Kubernetes’ built-in self-subject review POSTs (permission introspection). Wildcard resources,
credential reads and subresources are refused. Missing roles, aggregated roles
and failed policy reads fail closed. The calling Kubernetes identity needs
permission to list these bindings, read referenced roles, and create the named
service account's token. This intentionally requires complete policy visibility.
Use an RBAC-authorized cluster; additional authorization systems and subsequent
policy changes are outside this snapshot check. Keep the dedicated reader's
policy under operator control.

The token request has a single API audience and bounded expiration. An issuer
that returns a token beyond the requested lifetime is refused. The token goes
to grpcurl through a private environment variable, never a command argument or
file. Successful combined stdout/stderr is buffered (up to 16 MiB) and credential values
are redacted before printing, including base64 encodings. Failed token requests
return fixed diagnostics rather than issuer output. The command times out at
the requested token lifetime. It prints only the RPC response or a sanitized
failure (consumer output is discarded on failure); there is no command to print the token.
