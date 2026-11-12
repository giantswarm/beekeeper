# Session Transcripts: Store, Search and Access Control

`beekeeper transcripts` collects, indexes, searches and gates session transcripts across every
supported harness (`claude`, `agy`, `codex`, `omp`) and execution host (the local coordinator and
SSH execution hosts).

## Store Layout and Collection

All normalized transcripts live on the coordinator under `<stateDir>/transcripts`:

- `<key>.meta.json` — session metadata (`id`, `hostSession`, `execution`, `name`, `rosterName`,
  `role`, `repo`, `ref`, `refs`, `harness`, `host`, `cwd`, `model`, `running`, `turns`, `startedAt`,
  `lastActive`, `endedAt`).
- `<key>.turns.jsonl` — normalized, secret-redacted turns (`at`, `role`, `text`, `tool`, `result`).

Metadata and turns survive the session ending or leaving the roster: when a local session or remote
SSH execution finishes, its stored record transitions to `running: false` with `endedAt` preserved
so later workers and the guide can still search and read its reasoning and decisions.

### Sources Collected

1. **Local coordinator sessions (`claude`, `agy`, `codex`, `omp`):**
   - Running sessions discovered from the process table and harness session directories.
   - Ended sessions from `omp` (`<omp.sessionsDir>`), `agy`
     (`<agy.brainDir>/<conversationId>/.system_generated/logs/transcript.jsonl`), `codex`
     (`<codex.sessionsDir>` and `archived_sessions`), and Claude Code (`<claude.projectsDir>` and
     desktop session records).
2. **SSH execution hosts (`agents.hosts`):**
   - Every managed SSH execution record (`st.Starts[i].Execution`) is indexed with its execution identity, host, harness, repository, issue/PR references and initial/follow-up prompts.
   - During host reconciliation (`syncExecutionHost`), explicit `beekeeper transcripts sync`, or
     when reading a remote execution's transcript, the coordinator pulls incremental log pages
     (`Logs`) from the execution host and parses the harness's stream (`claude` stream-json, `codex`
     `--json`, `agy` stream-json and mirrored `transcript.jsonl`) into normalized turns.

### Dialog Answers Preserved (`AskUserQuestion`)

When a session asks the operator a structured dialog question via `AskUserQuestion`, both `beekeeper
tail` and the transcript store extract the question and the person's selected answers from
`tool_result` and `toolUseResult.answers`, recording the answer as a searchable `user` turn.

### Secret Masking

Before any turn or tool result is written to `<stateDir>/transcripts`, its text is passed through
the value scanner (`guard.Index.Redact`), replacing every fingerprint match from `<stateDir>/scan`
and every credential token pattern match with `[redacted: ...]`. Raw secret values are never written
to the transcript store.

## Access Control

Transcript access is enforced both in `beekeeper transcripts` (local and coordinator RPC) and in the
pre-tool-use file guard (`guard.TranscriptAccess`, which includes `<stateDir>/transcripts` in its
guarded roots):

- **Guide:** The session holding the `guide` role (and the operator outside a managed worker session) can list, search and read **all** transcripts across every repository, harness and host.
- **Worker default (same repository):** A worker session can list, search and read:
  - Its own transcript, and
  - Transcripts of other worker sessions on the **same repository** (`meta.Repo` or the repository of `meta.Ref` / `meta.Refs`).
  - A worker cannot read the guide's transcript or transcripts from other repositories by default.
- **Requesting and granting wider access:**
  - When a worker needs context from another repository or specific session, it records a request:
    ```bash
    beekeeper transcripts request --repo example-org/other-repo --reason "check shared release contract"
    beekeeper transcripts request --session guide --reason "read prior architectural decision"
    ```
  - The guide inspects open requests and active grants with `beekeeper transcripts requests` and grants an expiring lease (default `24h`):
    ```bash
    beekeeper transcripts grant <worker> --ttl 4h
    beekeeper transcripts grant <worker> --repo example-org/other-repo --ttl 2h
    beekeeper transcripts grant <worker> --session <session-or-ref> --ttl 1h
    ```
  - Grants are stored in `state.json` (`transcriptGrants`) and expire automatically at `expires`, or immediately when revoked with `beekeeper transcripts revoke <worker>`.
  - A grant covers the stored transcript, not the files that session wrote: a file under a harness
    store needs `hooks.transcriptGrants`. The grant's output says so, and says when no stored
    transcript matches it yet.

### What a Read Cannot Return

`beekeeper transcripts read` opens with what the turns it returns do not hold, and with `--json` the
same counts are in `loss`. A worker that continues from a predecessor learns from these lines what
it does not have, and whether it can get it.

- `# lost for good:` text the harness cut before the store read it (a `<truncated N bytes>` marker in its own transcript). No read brings it back.
- `# not in the index:` tool results the store clipped to 4 KiB, each ending in `… <N bytes not
  stored>`, and tool calls of an ended session stored with no result. The source transcript may
  still hold them, which reading needs a `hooks.transcriptGrants` grant for; the line says so, or
  says the source is gone and they are lost too. A running session's calls without a result are
  counted apart: they may not have finished. A call answered with empty output is not counted.

An agy transcript indexed before tool results were stored is read again once at the next sync, while its source exists. A remote execution's transcript is read again at its next log page.

## CLI Reference

```bash
# List accessible transcripts (filters: --repo, --harness, --host)
beekeeper transcripts
beekeeper transcripts list --repo example-org/service --harness agy

# Search across accessible running and ended transcripts
beekeeper transcripts search "retention window"
beekeeper transcripts search "release promote" --repo example-org/service --tools --results --since 72h

# Read a session's transcript by roster name, session id, execution id, or issue/PR reference
beekeeper transcripts read worker-1 -n 30
beekeeper transcripts read https://github.com/example-org/service/issues/42 --tools --results

# Request, inspect, grant or revoke wider transcript access
beekeeper transcripts request --repo example-org/other-repo --reason "verify cross-repo API"
beekeeper transcripts requests
beekeeper transcripts grant worker-1 --ttl 2h
beekeeper transcripts revoke worker-1

# Explicitly sync local and remote execution transcripts into the store
beekeeper transcripts sync
```

Managed SSH workers run the same `beekeeper transcripts` commands (`list`, `search`, `read`,
`request`); their CLI forwards the call over the authenticated coordinator RPC channel where the
coordinator evaluates the worker's repository identity and active grants.

## Retention and Disk Budget

`<stateDir>/transcripts` is registered as the built-in `transcripts` disk cache policy (`mode:
files`, default `olderThan: 30d`, `maxGiB: 2`). `beekeeper disk` prunes transcript files older than
30 days or over the 2 GiB budget, and operators can tune or override the `transcripts` entry under
`disk.caches` in `config.yaml`.
