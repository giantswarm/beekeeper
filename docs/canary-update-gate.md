# Canary update gate

The installer requires a second, separately signed release asset before replacing
an executable. This applies to latest-release discovery, exact-version execution
host updates and coordinator-supplied releases, including privileged installs.
Discovery and `self-update --check` intentionally remain available before success.
Local desk compatibility commands still run after this gate.

For each `beekeeper-<os>-<arch>` binary (including `.exe` on Windows), publish
`<binary>.canary.json` and `<binary>.canary.json.bundle`. The bundle must verify
with the same Sigstore public-good trust root, transparency log, timestamp and
repository-bound CircleCI certificate identity used for binary verification.
An unsigned GitHub status, release body, coordinator assertion or command exit
code is never sufficient. The signing job and its credentials are part of the
trust boundary: it must only sign results produced by the isolated staging
workflow, never caller-supplied results.

Schema 1 contains these JSON fields:

```json
{
  "schema": 1,
  "version": "v1.2.3",
  "sha256": "lowercase SHA-256 of the exact signed binary bytes",
  "os": "linux",
  "arch": "amd64",
  "status": "success",
  "run": "sandbox run identifier",
  "merge_commit": "observed sandbox merge commit"
}
```

Success requires the run and merge observation. A terminal `failed` result
requires a nonempty `reason`; `pending` never permits installation. Version,
digest, OS and architecture must all match. Unknown schemas and fields, trailing
JSON, missing assets and invalid signatures are rejected. Evidence does not
expire with wall-clock time: it attests to immutable artifact bytes, not current
service health. Replays for a different version, platform or digest fail. A new
scenario contract must increment the schema and deploy updated gates first.

Only authenticated, identity-matching terminal failure creates durable local
quarantine, keyed by digest under `$XDG_STATE_HOME` (default `~/.local/state`) in
`beekeeper/canary-quarantine`. The signed failure and reason are retained. Later
success cannot override it. Operators may remove the matching quarantine file
after investigating; prefer a newly built and staged candidate. Preserve this
state across desk restarts. Missing/pending evidence, lookup and budget failures
remain retryable. Live desk budget, status and capacity checks do not quarantine
an otherwise eligible candidate.

## Publishing and rollout prerequisites

Issue #2256 supplies the isolated runner and merge observation. It is not yet
implemented. Until that publisher is integrated, releases lack evidence and this
gate intentionally refuses replacement. This change alone does **not** complete
protected release rollout or the publishing acceptance criterion of #2257.

The release workflow must wait for the CI workflow regression check, publish the
signed candidate, run the isolated staging scenario against that exact artifact,
and only then sign and upload evidence and its bundle. The candidate may be
visible while staging is pending. Every supported binary/platform needs its own
matching evidence; a Linux canary cannot authorize a Darwin binary. Transient
sandbox/service/budget errors must leave evidence absent or pending, never emit
terminal failure. A missing merge observation must never produce success.

Bootstrap this gate to existing desks through a reviewed manual deployment
before claiming protected rollout. Pre-gate clients still install signed releases
without staging evidence; they are an explicit rollout limitation. Keep a manual
installation/recovery path because old releases do not carry evidence. No
automatic rollback is provided by this gate.
