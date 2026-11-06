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

Schema 1 supports Codex:

| Content | Mapping |
| --- | --- |
| Rules | `rules/<name>/AGENTS.md` or `CLAUDE.md`, appended in manifest order to host AGENTS.md |
| Skills and resources | `skills/<name>/...`, requiring a SKILL.md per skill |
| Harness settings | `model_reasoning_effort`: low, medium, high, xhigh |
| Plugin/tool/connector declarations | Named capabilities (for example `plugin:review` or `tool:search`); all required capabilities currently refuse by name; optional declarations are metadata only |

Every supplied setting is required: unsupported names or values refuse rather
than silently disappear. Optional capabilities are explicitly unavailable, not
installed. Connector endpoints, credentials, plugin installation commands and
arbitrary harness configuration have no representation in this schema.

Bundles are bounded to 256 artifacts, 256 KiB per artifact and 2 MiB serialized.
Validation checks canonical relative paths, ordering, duplicate paths, skill
entrypoints, schema, digests, UTF-8 and the shared safe-rules credential shapes
(with no fixture exemption). Capture rejects symlinks and configured secret
stores. Use the coordinator's secret-file exclusions when building a bundle.
Do not include secrets even when their format has no known credential shape.

`Server.SetCoordinatorBundle` makes a validated private snapshot. `Policy()`
binds its digest alongside the existing host configuration and security digest;
`Prepare` still requires that exact policy. Codex files live in a directory
named by the content digest, preventing removed skills from leaking into a new
bundle. Prepare generates the host's guard hooks and restrictive feature table,
then writes the policy manifest last. InstalledPolicy verifies every artifact;
hostRuntime restores the snapshot for subsequent guard invocations. Old bundle
homes remain available for operator cleanup; this API does not perform garbage
collection. Do not prepare or replace policy while workers are active.

Operator-managed exclusions include coordinator trust keys, repository admission,
security guards, authentication, executable/version pins, host environment,
project trust, model/provider routing, MCP configuration, hooks, sandbox and
approval settings. They cannot be overwritten by a bundle. Host rules remain
in the prompt and portable settings cannot disable hooks, enable plugins or
relax execution guards. Prompt rules themselves are untrusted text; the host's
security boundary is its pinned guard configuration, not prompt precedence.

This is a packaging and materialization API. Per-start transfer, admission and
installer orchestration are separate work. Existing `agents hosts bundle` and
`prepare` commands retain their host-policy contract; a future coordinator
consumer calls this API and obtains explicit operator approval of the new digest.
