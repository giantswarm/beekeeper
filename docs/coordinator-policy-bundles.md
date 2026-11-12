# Coordinator policy bundles

`execution.BuildCoordinatorBundle` captures an explicit list of approved files
under a source root. Callers enumerate the currently applicable AGENTS.md and
CLAUDE.md documents and every file needed by each selected skill; no directory
or home is recursively copied. Source paths in the manifest are relative to the
approved root, so manifests contain no machine identity. Artifact paths, source
paths, schema, bytes and per-file SHA-256 digests are bound by the bundle digest.
Inputs are sorted and JSON map keys canonicalized. There are no timestamps.
The caller must review the approved source list; a digest is not a signature or
permission to install.

Schema 1 maps content to harnesses as follows; settings apply to Codex only:

- Rules: `rules/<name>/AGENTS.md` or `CLAUDE.md`, appended in manifest order to host AGENTS.md
- Skills and resources: `skills/<name>/...`, requiring a SKILL.md per skill
- Harness settings: `model_reasoning_effort`: low, medium, high, xhigh
- Plugins: `plugins/<name>/skills/<skill>/...`, requiring a SKILL.md per skill, and `plugins/<name>/commands/<file>.md`
- Plugin/tool/connector declarations: Named capabilities (for example `plugin:review` or
  `tool:search`); a required `plugin:<name>` passes when the bundle carries that plugin's files, any
  other required capability refuses by name; optional declarations are metadata only

A plugin travels as its documents: its skills and commands. Its hooks, MCP servers
and anything else that would execute on a host are refused, since the host's
guards stay the only ones; a plugin that needs them does not work remotely.

Every supplied setting is required: unsupported names or values refuse rather
than silently disappear. Optional capabilities are explicitly unavailable, not
installed. Connector endpoints, credentials, plugin installation commands and
arbitrary harness configuration have no representation in this schema.

Bundles are bounded to 256 artifacts, 256 KiB per artifact and 960 KiB serialized,
so one wire request carries a bundle.
Validation checks canonical relative paths, ordering, duplicate paths, skill
entrypoints, schema, digests, UTF-8 and the shared safe-rules credential shapes
(with no fixture exemption). Capture rejects symlinks and configured secret
stores. Use the coordinator's secret-file exclusions when building a bundle.
Do not include secrets even when their format has no known credential shape.

## Reaching the next start

Name the approved files on the coordinator:

```yaml
agents:
  policyBundle:
    root: /srv/desk
    sources:
      - source: AGENTS.md
        artifact: rules/desk/AGENTS.md
      - source: skills/review/SKILL.md
        artifact: skills/review/SKILL.md
    settings:
      model_reasoning_effort: medium
```

Every remote start reads these files anew, so an edit reaches the next worker
with no change on the host. The list in this configuration is the operator's
approval; a digest is not a signature. With no `policyBundle`, a worker has the
host's prepared policy alone.

Before a start is reserved, the coordinator asks the host about the bundle's
digest and transfers it only if the host lacks it. A coordinator or transfer
failure, an oversized bundle, a required capability or a changed installed bundle
refuses the start: no reservation, no worker, and no local fallback. The start
request binds the digest, and the host verifies every managed file against it at
admission and again before the harness runs.

The bundle is not part of the host's pinned policy. A rule change moves no pin,
and `Prepare` covers host configuration and guards only. Each bundle has its own
directory, `bundles/<digest>` under the host's managed state, staged whole and
published with one rename under the host's admission lock, so a concurrent start
sees all of it or none. Installing only adds: an active or parked worker keeps the
rules, skills and Codex home it was admitted under, and its thread with them. A
prepare rewrites the managed files of every bundle for the new release's host
rules, leaving what workers wrote there.

| Harness | What it reads |
| --- | --- |
| Codex | The bundle's own `CODEX_HOME`: rules in `AGENTS.md`, skills as files, settings in `config.toml` |
| Claude, omp | The bundle's `CLAUDE.md`: host rules, then bundle rules and skill files inline |
| agy | The same file, linked as `GEMINI.md` in its per-execution home |

`hosts check` adds `policyBundle`: the expected and installed digests, whether
the host is current, the artifacts that differ and any problem. A host behind the
coordinator still passes, since its next start receives the bundle. A changed
installed bundle is never repaired by a sync, because a worker may be bound to it;
it stays a reported problem and refuses starts of that digest.

A bundle no unretired execution is bound to goes once it is past the host's
retention, a few per sync. Until then it appears in `resourceAudit` as `shared`.
A host holds at most 256 bundles. At that bound a sync removes the oldest unbound
ones regardless of retention, before publishing; when every bundle is bound to an
unretired execution it refuses the new one, and the start with it, until one
retires. Status and prepare keep working throughout.

Operator-managed exclusions include coordinator trust keys, repository admission,
security guards, authentication, executable/version pins, host environment,
project trust, model/provider routing, MCP configuration, hooks, sandbox and
approval settings. They cannot be overwritten by a bundle. Host rules remain
in the prompt and portable settings cannot disable hooks, enable plugins or
relax execution guards. Prompt rules themselves are untrusted text; the host's
security boundary is its pinned guard configuration, not prompt precedence.

Existing `agents hosts bundle` and `prepare` commands retain their host-policy
contract and carry no coordinator bundle.
