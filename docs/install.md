# Install

Download, verify and install beekeeper, keep it current, and add the role plugin.

## The supervisor service

`beekeeper install` installs the supervisor loop, the desk's supervisor,
as a persistent user service (systemd on Linux, launchd on macOS); it is
no option. It runs the installed binary with the same configuration, starts
at login and restarts on failure. It retains the installer's absolute tool
PATH entries so it can find GitHub and worker CLIs outside the service
manager's default PATH; rerun install after changing tool directories.
Later installs update its definition;
a replaced binary is re-executed at the loop's next quiet pass, after
pending timer actions finish. `beekeeper uninstall` removes the service
recorded in `install.json`. While it is down the standby watch restarts
its systemd service on Linux (on macOS launchd restarts a failed loop
itself); no session ever stands in for it.

## Download and verify

Download the binary for your OS and architecture from the latest release into any directory on
your `PATH`:

```bash
dir=~/.local/bin   # any directory on PATH
curl -fsSL -o "$dir/beekeeper" https://github.com/giantswarm/beekeeper/releases/latest/download/beekeeper-linux-amd64
chmod +x "$dir/beekeeper"
```

Release assets are signed keyless by the CircleCI pipeline, not by a GitHub workflow, and each is
published next to its Sigstore bundle (`<asset>.bundle`). Verify a download with the CircleCI
identity and issuer; a `github.com/giantswarm` identity or the GitHub Actions issuer fails with an
identity mismatch:

```bash
curl -fsSL -o "$dir/beekeeper.bundle" https://github.com/giantswarm/beekeeper/releases/latest/download/beekeeper-linux-amd64.bundle
cosign verify-blob --bundle "$dir/beekeeper.bundle" \
  --certificate-identity-regexp '^https://circleci\.com/api/v2/projects/[0-9a-f-]+/pipeline-definitions/[0-9a-f-]+$' \
  --certificate-oidc-issuer https://oidc.circleci.com \
  "$dir/beekeeper"
```

The certificate's SAN is
`https://circleci.com/api/v2/projects/<project-id>/pipeline-definitions/<definition-id>` and its
issuer `https://oidc.circleci.com`.

## Put it in place

Then put it in place for your user, after a look at what it would do:

```bash
beekeeper install --dry-run   # every file, hook and service step; changes nothing
beekeeper install
```

## Hooks and settings

`install` merges the PreToolUse, PermissionRequest, PostToolUse and SessionStart hooks into Claude
Code's user settings (`~/.claude/settings.json`, or `$CLAUDE_CONFIG_DIR`) with the binary's absolute
path, when `agy` is installed (`agy` on `PATH` or `~/.gemini/config/hooks.json` or
`~/.gemini/config/skills.json` present) writes the `"beekeeper"` `PreToolUse` (`<this binary> hook
pretooluse --format agy`) and `PreInvocation` (`<this binary> hook preinvocation`) entries into
`~/.gemini/config/hooks.json` in the shape `agy` accepts (`PreInvocation` a list of commands,
`PreToolUse` a matcher `*` with its commands; an earlier entry with a `PreInvocation` matcher, which
`agy` rejects whole, is replaced) without touching other named hooks in that file and never through
a symlinked file and extracts the embedded skills into `<stateDir>/skills` and registers `{"path":
"<stateDir>/skills"}` under `"entries"` in `~/.gemini/config/skills.json` without touching other
entries or `inherits` in that file, and when `codex` is installed (`codex` on `PATH` or
`~/.codex/hooks.json` or `~/.codex/skills` present) merges the `PreToolUse` entry (`<this binary>
hook pretooluse --format codex`, matcher `.*`) into `~/.codex/hooks.json` and
`<stateDir>/codex/home/hooks.json` without touching other hooks, extracts the embedded skills into
`<stateDir>/skills`, and symlinks them into `~/.codex/skills/<name>` and
`<stateDir>/codex/home/skills` (plus `<stateDir>/codex/home/AGENTS.md` to `~/.codex/AGENTS.md` when
present; `beekeeper doctor` checks both `agy` and `codex` hooks and skills too, and fails when `agy`
would reject any entry of `~/.gemini/config/hooks.json`, the `"beekeeper"` one or another's, since
`agy` then drops the whole file and runs unguarded; install itself fails on such an entry after its
other steps, and an `agy` worker start refuses while that file would load no `"beekeeper"` guard),
writes and starts the [standby service](notifications.md#desktop-notifications) (a systemd user
unit, a launch agent on macOS), with `teleport.proxy` set the [Teleport login's
keeper](clusters.md#the-teleport-login), and on systemd the [agent
sandbox](sandbox.md#the-agent-sandbox)'s broker (`beekeeper-sandbox.service`) and the memory guard
sized to the machine's RAM: `memcap.slice` for `beekeeper run`'s capped commands, with their CPU
budget (`memcap.cpuQuota`, `memcap.cpuWeight`), and a drop-in for the Claude Desktop scope that runs
(run install again with the app running when it does not).

Without a config it writes a starter one, the [example configuration](examples/config.yaml) with
every key commented out. The standby service runs `watch --standby`, and `watch --notify --standby`
only with `watch.notify: true` (off by default). Before restarting a broker with `secret.session`,
install prepares a vault handover through the broker's private socket. The session passes in an
anonymous memory descriptor held by systemd, with its original expiry; no session value is written
to disk or returned to the installer.

Locking or replacing the session revokes a prepared handover. If the running broker or service
manager cannot prepare it, install reports the failure, skips only the broker restart and finishes
installing the other services. This includes the first upgrade from a broker without handover
support or without a notification socket. A deliberate `systemctl --user restart
beekeeper-sandbox.service` then puts it on the new binary, drops the old session and requires a new
sign-in. Repeating install cannot add handover support to an already-running old broker. Dry runs
prepare nothing.

The user manager holds the plaintext memory descriptor briefly during a handover.
Unlike the undumpable broker, a dumpable user manager can expose that descriptor
through `/proc/<pid>/fd` to unsandboxed processes of the same user. This is an
explicit tradeoff of the descriptor-store handover. Preparation happens immediately
before restart; a failed restart revokes and wipes the snapshot. A still-running
broker also wipes an abandoned preparation after 30 seconds. The replacement
wipes the shared bytes after reading them even if descriptor-store removal fails.
A revoked or expired snapshot starts a locked broker that continues serving.

A file already as install writes it stays; one an earlier install wrote, a unit that runs this
binary and a hook entry that runs this binary's hook command (an older matcher, a self-update's
settings) are updated in place, never added twice; one that differs and is none of these it keeps
and names, and it never touches another setting. A second run changes nothing, so install is safe to
run after every update: that is how a changed hook matcher reaches the settings.

A new Claude Code, `agy` or `codex` session picks the hooks up. `beekeeper uninstall` (also with
`--dry-run`) stops the units and removes exactly what install wrote, as `install.json` in the state
directory records it, and leaves the config and the state unless `--purge`. On Linux without systemd
install writes the hooks and the config and says the service is not available.

## The hooks' scope

The hooks are user-wide, but they act only in the desk's scope: a session beekeeper started or knows
(an `agents start`, the supervisor's or the guide's holder, an agent on the roster), or one whose
working directory or project lies under one of `hooks.scope.dirs` (a linked git worktree in `cwd` or
`agy`'s `workspacePaths` resolves to its main repository checkout). A session on the person's own
projects outside these gets nothing from them: no refusal, rewrite, redaction, prelude or permission
answer (`--format agy` answers `{"decision":"ask"}` on pass because `agy` 1.4.7 denies empty stdout
or `{}`). Unset, every session is in scope.

Workers and reviewers beekeeper started (including desk role holders) cannot read another
conversation's private stores: Claude Code projects and history, Codex sessions, archived sessions
and history (including `CODEX_HOME` and managed worker homes), agy/Gemini conversations and brain
logs, and omp sessions. The configured store locations are protected alongside the defaults. A
person's own interactive session retains transcript access even inside `hooks.scope.dirs`.

A managed session keeps access to its own Claude artifact subtree
(`<projects>/<slug>/<session_id>/`, including spilled tool results) and agy brain directory
(`<brain>/<conversationId>/`), using the hook session ID and requiring both the lexical and resolved
paths to stay in that subtree. File tools and shell reads, including filters, counts, copies,
redirects and symlink aliases, share the rule. For parser development use synthetic test fixtures.

A supervisor can grant an exact file (the receiving session's own transcript) or an explicit fixture
directory through `hooks.transcriptGrants`, keyed by that session's exact ID. No grant is inferred
from `transcript_path`, a missing ID or another session's grant; wildcard grants are ignored. Shell
globs inside a granted fixture directory are conservatively refused; name the fixture files
explicitly. Interpreter scripts that obtain a home or environment directory at runtime are also
refused: the hook cannot resolve their file targets, including paths selected by globbing or by
assembling a harness name.

Use explicit file paths so ownership and fixture grants can be checked. Quoted source text in
worktree edits remains data. A link inside a granted fixture directory cannot reach outside it.
Grants should be removed when their session ends. This is a hook policy for recognized paths and
shell scripts, alongside the agent sandbox's filesystem boundary.

```yaml
hooks:
  transcriptGrants:
    receiving-session-id:
      - /path/to/synthetic-fixtures
```

```yaml
hooks:
  scope:
    dirs: [~/projects/notebook, ~/projects/work]
```

## Updates

From then on `beekeeper self-update` keeps it current: it verifies the release binary's Sigstore
signature and renames it over the old one in one step, so a running `beekeeper watch` keeps running,
and a `devctl pr merge` gate call re-executes the new binary at its next step, waiting for its turn
or following its devctl, as does a `merge-child` before it hands its outcome to the owner. A running
watch keeps the code it started with: every watch, `--once` included, says `WATCH STALE` once per
watch whose binary was replaced, naming the version it runs and the one installed, and that a re-arm
(a restart) picks the new one up; no watch re-executes itself.

`beekeeper self-update --check` exits 125 while a newer release is out. A running pre-release (a
release candidate) is kept on the newest release candidate too; a release only takes releases.
`beekeeper self-update --version v0.125.0-rc.3` selects an exact tag, including a release candidate;
add `--check` to report it without installing. The same signature verification and atomic
replacement apply. Both ask GitHub with `GH_TOKEN`, `GITHUB_TOKEN` or the gh login's token, never
anonymously (a shared IP spends the anonymous limit): without a token they say so and stop, and a
rate-limited lookup is named with its reset.

## Older binaries after an install

An install leaves the older binaries already running on their old code until they end or are
re-armed: the `agents start` of a worker started before it (its desktop reopen saves the state after
the first turn), a watch, the standby unit; `beekeeper self-update` names them (pid and command), a
gate call or `merge-child` among them marked as re-executing the installed binary at its next step,
and the other watches (`watch`, `alerts watch`) with the command that re-arms them.

The guide watch re-executes the installed binary before its next poll; if that fails it says `GUIDE
WATCH STALE` with recovery instructions. It also says a refused save (`GUIDE SAVE REFUSED`), as the
supervisor's watch says `WATCH STALE`. The state carries the version of the newest release that
saved it (`writer`), and a release older than it does not save: the save is refused with the state
unchanged, logged once per process as `state.stale-writer` naming the process, its command and both
versions, and the command ends with that message (exit 3; a gate call re-executes the binary
installed at its path first and refuses, 77, only when it cannot, the lane untouched either way; a
gate whose devctl already ran leaves the run's document and exit code under `merges/` for the watch,
which records the outcome from them at its next poll, so no lane keeps a running entry for a
finished merge).

`beekeeper doctor` (a `DOCTOR stale binary` line in the watch) lists each such process while it
runs, with the path and time of the install that replaced its binary; a watch is named by `WATCH
STALE`. The path is read as the process itself resolves it (its own root). An install proven to
precede the process start is excluded; an inode replacement inside the start timestamp's uncertain
first second still counts. A call started from a second copy on the PATH, a re-executed gate call or
one started after the install is not listed.

A build without a release version (a development or release-candidate build) neither stamps the
state nor is refused, and its saves keep what a newer release recorded: every object of `state.json`
that carries per-entry data (the state itself, roster entries and their keep markers, notes, timers,
grants, holds, lanes, session records, starts, archives, the roles) writes the members its binary
does not know back unchanged, with its entry. A process older than this mechanism (v0.71.1 and
before) keeps only the state's top-level members and drops the nested ones.

## Platforms

The session and machine views need Linux (`/proc`, cgroup v2, the journal). Leases, holds and
the budget work on any system.

Sessions can [read classic branch protection through the broker](branch-protection.md)
with `beekeeper sandbox protection <owner/repo> <branch>`.

## The role plugin

The commands are half of a desk; the roles that drive them are the other half. The repository is
also a Claude Code plugin marketplace whose plugin [`plugin/`](../plugin) carries them as skills:
`/beekeeper:guide` (the session that walks the person through their decisions),
`/beekeeper:register-agent` (an empty session joining the roster) and `/beekeeper:worker-rules` (the
core every worker follows; `agents start` puts it ahead of every brief), with the topic skills a
worker loads before the step that needs them: `beekeeper:lease` (a lab, an installation, the
browser), `beekeeper:merge` (a merge, promotion or tag), `beekeeper:github` (GitHub reads, builds,
browser steps, waits) and `beekeeper:park` (a turn that ends with the task open).

A topic no enabled plugin installed serves as the binary embeds it follows the core in the first
prompt itself, and `agents start` says so. The role skills keep an always-loaded core and read the
rest from their `references/` folder when its moment comes. Their text is generic: the desk's
installations, resources, lanes and person are the configuration's keys (below), and the desk's own
conventions — which board, which repositories, local traps — stay in the project the sessions run
in, which the skills defer to.

`beekeeper lint briefs <file or folder>...` refuses what makes a skill or a brief go stale: a dated
line, an "until X ships" clause, a note on the release that fixed something, a workaround, a role's
run number, a bare `#n` or `owner/repo#n` without its full GitHub URL, and a shell example's bare
`>` into a file, which a rerun under noclobber refuses (`>|` passes). The shipped skills pass it in
CI, and a project holding its own skills can run it as a CI step (exit 3 on a finding).

```bash
claude plugin marketplace add giantswarm/beekeeper
claude plugin install beekeeper@beekeeper   # --scope project for one project only
```

or, for a project's shared settings (`.claude/settings.json`):

```json
{
  "extraKnownMarketplaces": {
    "beekeeper": {"source": {"source": "github", "repo": "giantswarm/beekeeper"}}
  },
  "enabledPlugins": {"beekeeper@beekeeper": true}
}
```

The plugin follows the default branch; `"ref": "<release tag>"` in the source pins it to the
binary's release. With `guide.skill: beekeeper:guide` a relayed guide successor opens with the
plugin's role. The role skill carries `disable-model-invocation`, so it stays out of every worker's
skill listing and the Skill tool refuses it: the role loads only from a prompt that starts with its
bare slash command, as the person types it. A successor's first prompt and `guide handover --prompt`
start with it (`/beekeeper:guide you take over …`), and a desk's own copy of the role skill carries
the flag too. The supervisor has no skill: it is the supervisor loop, no session.

## Instructions for non-Claude workers

Non-Claude workers (`codex`, `agy` and `omp`) receive the working directory's Claude
project instructions at the head of their first message, before the worker rules and task.
The composition follows `CLAUDE.md`, its `@`imports, `.claude/rules/**/*.md`, and the
same safe mandatory-reading discovery as the repository context hook, from the repository
root down to the working directory. Outside Git, the working directory is the boundary.
Secret-named files and imports outside that boundary are excluded. `agents start --dry-run`
prints the composed instructions exactly as they enter the first message.

An existing `AGENTS.md` is included and takes precedence over conflicting `CLAUDE.md`
instructions, whether Git tracks it or not; rules nearer the working directory take
precedence over their ancestors. A repository with only `AGENTS.md` keeps its native
loading behavior. Stale local instruction files must be corrected at their source:
beekeeper does not guess their intent from Git tracking status. The composition fits
Codex's worker `project_doc_max_bytes` (262144 bytes); truncated and omitted files are
named for the worker to read. It stays in the session's message history, never in a
shared home file that another project's worker could overwrite.

For `agy` (Antigravity CLI), `beekeeper install` writes the embedded skills to `<stateDir>/skills`
(`~/.local/state/beekeeper/skills`) and registers `{"path": "<stateDir>/skills"}` under `"entries"`
in `~/.gemini/config/skills.json` without touching other entries or `inherits` (`beekeeper doctor`
checks both the `skills.json` entry and the skill files on disk, and `beekeeper uninstall` removes
them). Because `agy` walks `AGENTS.md` and `GEMINI.md` only from the working directory up to the
repository root (the folder containing `.git`) plus the global `~/.gemini/config/GEMINI.md` — and
does not load an `AGENTS.md` in a parent folder above the repositories — a desk whose shared rules
sit above the repositories puts them in `~/.gemini/config/GEMINI.md`, or includes the desk's rules
file from `~/.gemini/config/GEMINI.md` with `@[AGENTS.md](/path/to/projects/AGENTS.md)`.

For `codex` (OpenAI Codex CLI), `beekeeper install` writes the embedded skills to
`<stateDir>/skills`, symlinks each skill into `~/.codex/skills/<name>`, links
`<stateDir>/codex/home/skills` to `<stateDir>/skills` and `<stateDir>/codex/home/AGENTS.md` to
`~/.codex/AGENTS.md` when present, and merges the `PreToolUse` entry (`<this binary> hook pretooluse --format
codex`) into `~/.codex/hooks.json` and `<stateDir>/codex/home/hooks.json` (`beekeeper doctor` checks
both the user and worker `hooks.json` and `skills` directories, and `beekeeper uninstall` removes
them).

Because `codex` reads `AGENTS.md` only from the repository root (`project_root_markers = [".git"]`)
down to `cwd` plus `$CODEX_HOME/AGENTS.md` — and does not walk above `.git` to a parent
`~/projects/AGENTS.md` — a desk whose shared rules sit above the repositories puts them in
`~/.codex/AGENTS.md` (linked into `<stateDir>/codex/home/AGENTS.md` by `beekeeper install` and
`codex.EnsureHome`).
