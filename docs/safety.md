# Folders, deletes and guards

The hook's guards around the person's folders, deletes and writes outside the allowed
organizations, and how to switch the guards off for a while.

## The person's folders

`projects.dir` (default `~/projects`) holds the person's clones and org folders
(direct children that are no checkout and hold clones). The home folder,
`projects.dir` and these org folders are protected: the PreToolUse hook refuses a
new direct child from Write or NotebookEdit, shell output redirection or `tee`,
`mkdir`, `touch`, `cp`, `mv`, `git clone`, `gh repo clone` or `git worktree add`.
Existing targets and paths deeper inside a folder pass this protection.

`projects.worktrees` defaults to `[~/projects/worktrees]`; put worktrees and scratch
checkouts there with layout `<repo>/<branch>`. For example, use
`git -C <clone> worktree add <place>/<repo>/<branch> -b <branch> origin/main`.
`agents.roots` defaults to these places when unset. `home.places` defaults to
`[~/.local/state]` for state, logs and backups; drafts use the session scratchpad.
The refusal and the worker rules supplied by `agents start` name these paths.
A path the person named for the task is created by the person or with their go-ahead.
Commands follow `cd` (bare `cd` goes home), `git -C`, `~`, `$HOME` and shell
assignments; use `cd "${dir:?}"` to stop on an empty directory variable.

`hooks.personFolders` names the person's folders (`~/` and globs: `[~/, ~/projects, ~/projects/*]`;
unset, the guard is off). The PreToolUse hook refuses a Write, Edit, MultiEdit or NotebookEdit, and
a Bash command's output redirection (`>`, `>|`, `>>`, `&>`, `n>`) or `tee` target, that creates a
file directly in one of them (an existing dotfile there is written in place), or writes a file in
the main working tree of a git checkout that sits directly in one which git neither tracks nor
ignores: a draft there overwrites the person's own untracked file of the same name.

A linked worktree, a tracked or ignored file and a file under a dot-directory (`.config/`,
`.claude/`) pass; the path counts as written, not through its links. Redirection targets are
resolved against the command's `cd`s (a `cd` after `||` may not run, so both ways count), its
assignments, `$PWD` and the environment; an unset `$NAME` is empty, as the shell takes it (`cd
"$TMPDIR"` with `TMPDIR` unset stays in the checkout), and a command substitution or an expansion
with an operator (`${dir:?}`) is unknown and passes.

The refusal names the session's scratchpad by absolute path and `~/.local/state/<program>/`. `agents
start` names the started session's scratchpad in the worker rules.

## Deletes

A delete under a variable that turns out empty removes what it never named: after a failed `cd … &&
D=$(mktemp -d)`, the cleanup `shred -u "$D"/*` runs as `shred -u /*`. The PreToolUse hook refuses an
`rm`, `shred`, `unlink`, `truncate` or `find` with `-delete` or `-exec rm` (also behind `sudo`,
`xargs` and the other wrappers, in a shell's `-c` string, a here-document a shell reads and a
command substitution) whose target is:

- a path after a variable or command substitution that may be empty: `"$D"/*`, `$D/*`, `$DIR/`,
  `"${X}/…"`, `"$1"/x`, `"$(…)"/build`, unless the variable is guarded (`${D:?}`, `${D:-/default}`)
  or `set -u` (`set -euo pipefail`, `set -o nounset`) comes before it in the command;
- the root, a top-level directory, the home directory, or every entry of one of them, as written or
  after a literal assignment in the command: `/`, `/*`, `/home`, `/usr/`, `/tmp/*`, `/tmp/"$D"`,
  `~`, `~/*`, `"$HOME"`.

The refusal names the command and the target, what the target becomes, and the safe forms: guard the
variable (`rm -rf "${D:?}"/*`), run the cleanup from a `set -euo pipefail` script with an `EXIT`
trap, or move the files aside instead of deleting them. `rm -rf "$D"` alone, `$HOME` and `$PWD`
paths, `git clean`, `git rm`, `klausctl stop|delete` and beekeeper's own commands pass.

## Writes outside the allowed organizations

An agent never writes to a repository outside the organizations it works in: what is meant for an
upstream project lands as a commit on our fork and as a row on the upstream review list
(`github.upstreamReview`), and a human decides what goes upstream and opens it. `beekeeper hook
pretooluse` refuses, before the command runs, every GitHub write aimed at a repository whose owner
is not one of `github.allowedOrgs` (default `giantswarm`; any case):

- `gh pr create|comment|review|close|edit|ready|merge|reopen|lock|unlock|update-branch`, `gh issue
  create|comment|close|edit|reopen|lock|unlock|delete|transfer|pin|unpin|develop`, `gh release
  create|edit|delete|upload|delete-asset`, `gh label create|edit|delete|clone`, `gh repo
  edit|delete|archive|unarchive|rename`;
- `gh api` with POST, PATCH, PUT or DELETE (`-X`, `--method`; a field `-f`, `-F`, `--field`,
  `--raw-field` or an `--input` without a method implies POST) on a path under
  `repos/<owner>/<repo>` or `orgs/<owner>`, `{owner}/{repo}` read as gh fills it in, and `curl`
  doing the same to `api.github.com`;
- a `gh api graphql` mutation that posts to or changes an issue, pull request, review or discussion
  (`createIssue`, `addComment`, `mergePullRequest`, …) unless every owner it names (`owner: "…"`,
  `-f owner=…`) is allowed: one that names its target by node id only is refused, naming `gh pr` and
  `gh issue` with `--repo`;
- `git push` to a GitHub remote outside the organizations: the remote's push URL (https, ssh or scp
  form) read in the worktree the command runs in (`cd`, `git -C`), a URL as given, and without a
  remote the branch's push remote (`branch.<b>.pushRemote`, `remote.pushDefault`,
  `branch.<b>.remote`, `origin`). A push to a branch of our fork (`upstream/<slug>` on `origin`)
  passes;
- a GitHub connector tool that posts (`create_pull_request`, `issue_write`, `add_issue_comment`, …)
  with an `owner` outside the organizations.

The repository is the one the command names: `--repo`/`-R` (`OWNER/REPO`, `HOST/OWNER/REPO` or a
URL), a github.com URL or `owner/repo#n` argument, the API path; without one, the repository gh
itself acts on: `GH_REPO`, the remote `gh repo set-default` chose, else the first of the `upstream`,
`github` and `origin` remotes. In a fork clone with an `upstream` remote and no default set, a plain
`gh pr create` targets the upstream project, and is refused. A command inside a shell's `-c` string
or a here-document a shell reads is checked too. Reads pass: `gh pr view`, `gh issue view`, `gh api`
GET, `gh search`, `git fetch`.

The refusal is one line: `Refused: a human decides what goes upstream: patches land on our fork and
as a row on <github.upstreamReview>. `<verb>` writes to <owner/repo>, outside github.allowedOrgs
(<orgs>).` Each refusal is a `hook.refused` event in `beekeeper log` with the session, the
repository and the command (a command a token pattern matches is withheld, naming the rule), and the
supervisor's watch says it once: `REFUSED <session>: upstream write to <owner/repo>: <command>`.
`github.allowedOrgs: []` turns the guard off.

`github.publicOnlyOwners: [example-org]` restricts the listed owners to verified public
repositories. It defaults to `[]` and is independent of the owner allowlist: an allowed owner can
still be restricted, and disabling the owner guard does not disable visibility checks. Worker
admission (local and SSH), hook and connector writes, sandbox egress, and execution write
credentials, publication and merge routes reject private or internal repositories and unreadable or
missing visibility metadata. Each boundary reads fresh GitHub metadata; an unresolved repository
target under a restricted owner is refused, including owner-only API writes and opaque GraphQL
mutations. Public repositories remain subject to the existing owner policy.

The sandbox broker also checks writes before attaching its App token. REST writes, release uploads
and Git pushes must name an allowed owner; reads and Git fetches remain available for other owners.
Unknown write targets and GraphQL mutations are refused because an opaque node ID does not establish
an allowed owner; use an owner-scoped REST endpoint for those writes. GraphQL reads are inspected
within a 1 MiB body limit and forwarded unchanged. The hook recognizes repository flags in
short-flag clusters, including an attached or separate value after `-R`.

## Temporarily disabling hook guards

Set `hooks.guards: false` to let `hook pretooluse` pass tools unchanged in
Claude, agy and Codex, including calls normally refused or rewritten for the
merge gate, secret reads, allowed organizations or guide roles. Where a build
runs is placement, not a guard: in the hooks' scope a Claude session's Bash
builds still run under `beekeeper run`, and so on `run.remote` when it is
enabled; nothing is refused. Unset or `true` keeps enforcement enabled.
Configuration errors retain recovery mode; they do not silently disable guards.

Instruction relay, result secret redaction, sessionstart and worktree placement
continue independently. While `hooks.guards: false`, managed worker environments
(shell preludes, systemd units, background worker sessions and managed execution
environments) and beekeeper-owned `devctl` child commands propagate
`DEVCTL_UNSAFE_SKIP_VERSION_CHECK=true` so `devctl`'s own version gate does not
block older binaries; enabling `hooks.guards` again removes and unsets the
variable. `beekeeper fatal` marks installed harness guard checks disabled rather
than opening outages or repairs. `beekeeper capacity` and `beekeeper guide status`
display `guards: off`.
