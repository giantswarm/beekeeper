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
  "commit": "0123456789abcdef0123456789abcdef01234567",
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

## Staged canary runner (`self-update --stage-canary`)

The staged runner (`beekeeper self-update --stage-canary <path>`) executes the
verified candidate binary in an isolated temporary environment (`HOME`,
`XDG_CONFIG_HOME`, `XDG_STATE_HOME`, `TMPDIR`, `BEEKEEPER_CONFIG`, and a
restricted `PATH` isolated from live desk state and credentials) from its first
invocation (`version`) onward, and refuses if `<path>` resolves to the running
executable. When a sandbox GitHub token is present, the isolated environment
provisions a sandbox-scoped Git credential helper (`git-credential-canary` and
`GIT_ASKPASS`) inside its isolated `GIT_CONFIG_GLOBAL` that reads `GH_TOKEN` from
the subprocess runtime environment without restoring host Git config or embedding
tokens into files or remote URLs. In live sandbox mode, `--canary-bundle <bundle>`
is required and the candidate binary (plus any `--canary-sibling <sibling>` and
`<sibling>.bundle`) is verified against its Sigstore bundle before any candidate
subprocess runs. Recorded fixture runs (`--canary-recorded`) prefix `run` with
`recorded:`, cannot emit sibling release evidence, and are rejected by
`ValidateEvidence` and the installer gate so fixture runs can never grant release
eligibility.

The staged scenario exercises the full workflow boundary with real
`supervisor loop --once` passes:

1. Board scan (`board next --json`) and deterministic worker start
   (`supervisor loop --once` -> `agents start` -> `claude --bg`), creating a
   fresh candidate-bound sandbox branch (`canary/<run>/cand-<sha256[:12]>`) and
   pull request.
2. Worker branch creation, authenticated HTTPS commit push, pull request
   publication with the exact ownership record
   (`beekeeper-canary-owner: repo=<repo> run=<run> branch=<branch>` and
   `candidate-sha256: <sha256>`), and
   `agents park --on <owner/repo#n> "desk review"`.
3. Independent reviewer dispatch (`supervisor loop --once` -> `agents start` for
   `Review ...` -> `claude --bg`), exact-head review approval
   (`Head reviewed: `<sha>``), `supervisor.review` event recording, and required
   check rollup verification.
4. Coordinator merge validation (`checkReviewedMerge`), merge queue inspection,
   exact-head merge (`gh pr merge --match-head-commit <sha>`), and verification
   of durable candidate state records (`st.Starts`, `agents.start`,
   `agents.park`, `supervisor.review`, `supervisor.merge`/`merged`) plus the
   observed merge commit SHA on the created pull request and origin base branch.

Any failure, timeout, missing permission, unavailable sandbox, cleanup failure,
or missing merge observation emits `status: "failed"` with a non-empty `reason`
and exits non-zero.

## Sandbox GitHub App permissions and setup

Live sandbox runs require explicit `--canary-repo <owner>/<repo>`,
`--canary-allow-org <org>`, `--canary-project <number>`, and
`--canary-bundle <bundle>`. The runner refuses empty values, repositories
outside `--canary-allow-org`, and production repositories
(`giantswarm/beekeeper`, `giantswarm/devctl`). No personal desk defaults are
ever used.

Configure a dedicated GitHub App installed **only** on the sandbox repository and
project with least-privilege permissions:

- Repository permissions:
  - `Contents: Read and write` (branch creation, commit push, squash merge, branch deletion)
  - `Pull requests: Read and write` (PR creation, review comments, exact-head merge, cleanup close)
  - `Issues: Read and write` (sandbox issue intake and claim markers)
  - `Statuses: Read and write` / `Checks: Read-only` (required check rollup inspection)
  - `Metadata: Read-only`
- Organization permissions:
  - `Projects: Read and write` (ProjectV2 board status read/move on the dedicated sandbox project)

Required sandbox repository settings:

- Default branch `main` protected with required status check `test` and squash
  merge enabled.
- Automatic branch deletion or cleanup enabled; `CleanupSandboxRun` enumerates
  open pull requests with pagination (`gh api --paginate`) on entry and exit,
  closes and deletes only branches with the delimited prefix `canary/<run>/` and
  matching `beekeeper-canary-owner: repo=<repo> run=<run> branch=<branch>` line,
  enumerates and deletes any remaining owned refs under
  `repos/<repo>/git/matching-refs/heads/canary/<run>/` matching
  `canary/<run>/cand-<hex>` (including branches pushed before PR creation),
  verifies zero owned open pull requests or branches remain, and propagates any
  cleanup error into a failed canary verdict.
- Credentials (`BEEKEEPER_CANARY_REPO`, `BEEKEEPER_CANARY_ALLOW_ORG`,
  `BEEKEEPER_CANARY_PROJECT`, and the sandbox GitHub App credentials) are held in
  the protected CircleCI `architect` context on tag builds (`/v.*/`) and are
  never exposed to untrusted fork pull requests.

## Publishing and rollout order

On tag builds (`/v.*/`), CircleCI runs `publish-canary-evidence`
(`.circleci/publish-canary-evidence.sh`) after `go-build`, `test-loop-workflow`,
and `upload-release-assets`:

1. `go-build` compiles and signs the cross-platform binaries, and
   `upload-release-assets` attaches `<binary>` and `<binary>.bundle` to the
   GitHub Release so `beekeeper self-update --check` can discover the candidate
   while staging is pending.
2. `publish-canary-evidence` requires `BEEKEEPER_CANARY_REPO`,
   `BEEKEEPER_CANARY_ALLOW_ORG`, and `BEEKEEPER_CANARY_PROJECT` (failing closed
   without falling back to `--canary-recorded`), copies `./beekeeper-linux-amd64`
   to a separate runner executable so the staged candidate is never the running
   binary, verifies `./beekeeper-linux-amd64.bundle` and every sibling's
   `<binary>.bundle`, and emits `<binary>.canary.json` bound to each platform
   binary's SHA-256, version, commit, OS, and architecture.
3. `architect/cosign-sign-verify` (`kind: blob`) signs and verifies every
   `<binary>.canary.json` into `<binary>.canary.json.bundle` using the CircleCI
   OIDC identity.
4. `gh release upload` publishes `*.canary.json` and `*.canary.json.bundle` to
   the GitHub Release, unlocking `self-update` and execution-host `InstallVersion`
   / `InstallRelease`.

Bootstrap this gate to existing desks through a reviewed deployment before
claiming protected rollout. Pre-gate clients still install signed releases
without staging evidence; they are an explicit rollout limitation. Keep a manual
installation/recovery path (`docs/update-recovery.md`) because releases published
before this workflow do not carry evidence.
