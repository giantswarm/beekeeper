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
`XDG_CONFIG_HOME`, `XDG_STATE_HOME`, `BEEKEEPER_CONFIG` isolated from live desk
state) and refuses if `<path>` resolves to the running executable. When
`--canary-bundle <bundle>` is provided, the candidate binary is verified against
its Sigstore bundle before execution.

The staged scenario exercises the full workflow boundary with real
`supervisor loop --once` passes:

1. Board scan (`board next --json`) and deterministic worker start
   (`agents start` in background mode).
2. Worker branch creation, commit push, pull request publication, and
   `agents park --on <owner/repo#n> "desk review"`.
3. Independent exact-head reviewer start (`agents start --review`) and exact-head
   approval review comment.
4. Coordinator merge validation (`checkReviewedMerge`), merge queue inspection,
   exact-head merge (`gh pr merge --match-head-commit <sha>`), and verification
   that the origin base branch advanced to the observed merge commit SHA.

Any failure, timeout, missing permission, unavailable sandbox, or missing merge
observation emits `status: "failed"` with a non-empty `reason` and exits
non-zero.

## Sandbox GitHub App permissions and setup

Live sandbox runs require explicit `--canary-repo <owner>/<repo>`,
`--canary-allow-org <org>`, and `--canary-project <number>`. The runner refuses
empty values, repositories outside `--canary-allow-org`, and the production
release repository (`giantswarm/beekeeper`). No personal desk defaults are ever
used.

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
- Automatic branch deletion or cleanup enabled; `CleanupSandboxRun` also sweeps
  only resources matching `canary/<run>-*` or `canary-run: <run>` on entry and
  exit so interrupted runs leave unrelated resources untouched.
- Credentials (`BEEKEEPER_CANARY_REPO`, `BEEKEEPER_CANARY_ALLOW_ORG`,
  `BEEKEEPER_CANARY_PROJECT`, and the sandbox GitHub App credentials) are held in
  the protected CircleCI `architect` context on tag builds (`/v.*/`) and are
  never exposed to untrusted fork pull requests.

## Publishing and rollout order

On tag builds (`/v.*/`), CircleCI runs `publish-canary-evidence` after
`go-build`, `test-loop-workflow`, and `upload-release-assets`:

1. `go-build` compiles and signs the cross-platform binaries, and
   `upload-release-assets` attaches `<binary>` and `<binary>.bundle` to the
   GitHub Release so `beekeeper self-update --check` can discover the candidate
   while staging is pending.
2. `publish-canary-evidence` runs `./beekeeper-linux-amd64 self-update --stage-canary`
   to execute the isolated canary workflow and emit `<binary>.canary.json` bound
   to each platform binary's SHA-256, version, commit, OS, and architecture.
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
