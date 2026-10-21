# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).



## [Unreleased]

### Added

- `beekeeper cache clear go [--modcache]` clears the shared Go build cache, and the module cache, without breaking a running build: it waits for the `beekeeper run`s that hold the cache's use lock, holds back the runs that start meanwhile, runs `go clean -cache` (`-modcache`) and logs a `gocache.clear` event. The hook refuses `go clean -cache`, `go clean -modcache` and a delete of either cache's directory (`rm -rf ~/.cache/go-build`, `~/go/pkg/mod`, a directory holding them), naming the command. A clear under a running build failed it on the entries it lost ([#823](https://github.com/giantswarm/beekeeper/issues/823)).
- `sandbox.required`: a Claude Code session the agent sandbox's policy does not hold (no `BEEKEEPER_SANDBOX`: the policy not installed, or the session started before it) is refused every tool call but `SendMessage` by `beekeeper hook pretooluse`, in the hooks' scope or not, and `beekeeper hook sessionstart` gives it the refusal as context at its start. Off by default ([#138](https://github.com/giantswarm/beekeeper/issues/138)).
- `serve.organizations`: further Dex groups that make a caller a member, one per connector that spells the organization differently (a GitHub org's `<org>:<team>`, a directory's group name), beside `serve.organization`. A caller refused as no member or in no team is told, and serve's call log says, which groups the token carries, or that it carries no groups claim.
- `beekeeper app create <installation> --manifest <file> --user <login>` creates a GitHub App owned by a personal account: the manifest goes to GitHub's `settings/apps/new`, the conversion runs inside beekeeper as for an organisation's App, and the credentials land in the installation's SOPS Secret. The installation page preselects the owner and the repositories `installation.json` beside the manifest names, for `--org` too ([#1451](https://github.com/giantswarm/beekeeper/issues/1451)).

- The chart renders the Environments and merge lanes serve coordinates from `environments` and `lanes` (cluster-scoped, after its CRDs), an optional `httpRoute` that exposes `/mcp` behind a Gateway API Gateway for the musters of other installations, and `extraEnv`, `extraVolumes`, `extraVolumeMounts` and `initContainers` for a private issuer CA or a local proxy to the issuer.
- The person's Slack replies reach their running guide: the standby watch subscribes to `beekeeper://mailbox/<person>` through muster (`subscriptions/listen`) and delivers each message addressed to one of the machine's agents into its running session at its next tool round, else as a wake's turn, stamped `Message from <person> via slack`, and acks it once delivered; a message its agent does not take waits, in order, and is delivered again every `watch.interval`. `beekeeper converse "<text>"` writes to the person in the calling agent's Slack thread ([#174](https://github.com/giantswarm/beekeeper/issues/174)).
- `agents park --on hold:<target>` parks on a hold: the watch says the park resumable once the hold is lifted or its time passed, and resumes it with agents.autoResume. A park on a target nothing holds is refused. The gate's exit 77 on a hold names the park, so a worker refused by a short window (a devctl release window) resumes when it lifts instead of staying parked on its own pull request ([#1417](https://github.com/giantswarm/beekeeper/issues/1417)).
- `beekeeper browse --file` refuses a path outside the worker's directories (the working directory, the turn's `--dir`, beekeeper's state directory, the calling session's scratchpad; links resolved) before the turn starts, and the hook refuses a browse turn's `file_upload` of any path not shared with `--file`, which the report names as `refused: file_upload <path>: beekeeper's hook refused it as [Not Shared]` with a line naming the flag. `browse --help` says how a turn uploads on GitHub's comment editor, which renders no file input ([#821](https://github.com/giantswarm/beekeeper/issues/821)).
- `beekeeper app allow <name> --client-id <id>` records the consent for a GitHub App on the person's own account, which `apps.repo` does not declare: an App `app create --manifest` created, found in beekeeper's ledger, its callback hosts from its local manifest (or `--manifest <file>`). GitHub's `GET /app`, signed with the key `app create` stored, must answer the person's account (`gh api user`) as its owner, the ledger's id and that client id; its deletion is recorded the same way. An App declared nowhere is refused, naming `apps.repo` and the ledger ([#1466](https://github.com/giantswarm/beekeeper/issues/1466)).
- `beekeeper app allow <name> --gitlab <host> --client-id <uid> --callback <host> --declared <ref>` records the consent for a self-hosted GitLab's OAuth application: the hook answers that application's authorize page on that GitLab for a recorded callback host, and refuses any other application or callback there. The person's own declaration at `<ref>` (a vault field, a SOPS file's path) must carry the client id, compared inside beekeeper with no value in any output ([#1466](https://github.com/giantswarm/beekeeper/issues/1466)).
- `beekeeper app allow <name> --callback-url <url>` records one callback URL of an App the organisation declares, on a callback host its manifest names. With it, the hook answers the navigation to the App's settings page under the organisation's settings and the clicks there that name Add Callback URL, that URL or Save changes and typing exactly that URL, refusing another URL or field, a removal, a key or other input. A browse turn whose steps name the App runs with that URL's auto mode rule and reports `allowed callback URL:` ([#1455](https://github.com/giantswarm/beekeeper/issues/1455)).

### Changed

- The worker rules make the settings, installs and consents of the organisation's own Apps (`beekeeper app`) the worker's `beekeeper browse` job, never the person's; a consent that acts as the person towards another's App still needs their word. The `github` skill records an own App's grant first (`beekeeper app allow`) and reports a refusal as a barrier ([#1455](https://github.com/giantswarm/beekeeper/issues/1455)).

### Fixed

- `note add --holds` holds every `--ref` of the decision, not only the first: each is set `Blocked / Waiting`, `board next` skips each as `held: decision #<n> for <person> until it closes`, and each is set back once the decision closes; a `--ref` the hold refuses leaves none held ([#1475](https://github.com/giantswarm/beekeeper/issues/1475)).
- Builds on Go 1.27.3 (`toolchain go1.27.3`), which fixes the standard library vulnerability GO-2026-6809 that govulncheck reports for Go 1.27.2.
- The hook answers a recorded GitLab application's authorize page where the Chrome tools redacted its client id and GitLab titles the page "GitLab": it matches the application by the "Authorize <name>" heading or button the reading Chrome tools (`get_page_text`, `read_page`, `find`) read from that tab at that URL since the tab's last other call, whole and alone, for a recorded callback host. Another application's name, two names, a script's output, prose, text read at the tab's earlier URL or before a later call there, or nothing read stays refused, naming the page's application and the record's form. `app check --text` dry-runs it ([#1495](https://github.com/giantswarm/beekeeper/issues/1495)).
- The hook matches a recorded GitLab application on its authorize page by the client id alone, never by its name: where the Chrome tools redacted the URL's, by the client id the consent form's hidden `client_id` carries, as `guard.GitLabFormScript` (one fixed `javascript_tool` script, which the Chrome tools answer unredacted) read it on that tab at that URL since the tab's last call other than a page read. Chrome quotes a label unescaped, so an application whose name holds quotes could render a recorded one's "Authorize <name>" label; it is refused now, as is a click with no form read, whose refusal names the script. The recorded-callback check is unchanged; `app check --form <client-id>` replaces `--text` ([#1499](https://github.com/giantswarm/beekeeper/issues/1499)).
- `alerts capture` names the installations without a kube context on the desk in one line, as `alerts snapshot` does, and keeps `unreachable` with its cause for a context that exists and does not answer; a test holds the snapshot to the same ([#824](https://github.com/giantswarm/beekeeper/issues/824)).
- beekeeper's browse hook and `browse` treat every agentlab's loopback host as the lab's own identity provider: a nip.io name whose address is any of 127.0.0.0/8 (`*.lab2.127.0.0.2.nip.io`, dotted or dashed), a loopback address, `localhost`; a nip.io host of any other address stays refused ([#1467](https://github.com/giantswarm/beekeeper/issues/1467)).
- After `agents handover` or a relay, the predecessor never stays live under the shared name: the watch stops a CLI the desktop revives of a handed-over session, and of a relieved run once 2 minutes passed and it runs no tool command, with its reopen units (`REVIVED`, `agents.revived`), and owes its desktop session the archive. A `SendMessage` to a role, or by the successor's name, never goes to the relieved run: while the successor's CLI takes no message yet it is refused with when to send again ([#1460](https://github.com/giantswarm/beekeeper/issues/1460)).
- The hook refuses a merge in a script the call writes and runs by its path (`cat > run.sh <<'EOF'` … `./run.sh &`), however the call spells its name, and reads a script in a directory a `cd` reaches through a glob ([#1461](https://github.com/giantswarm/beekeeper/issues/1461)).
- Every lookup of a worker's start finds a worker whose CLI the desktop restarted under a new session id by its desktop host id: `agents handover` keeps the start's model, folder and brief, `agents wake` its bypass and model, a role's successor its folder, the PermissionRequest hook allows its requests (and holds them for the screen while the person took it over under its first CLI id), and the steward pick and `archive` know it as beekeeper's start ([#1441](https://github.com/giantswarm/beekeeper/issues/1441)).
- The hook knows a started worker by its desktop host id (`CLAUDE_CODE_HOST_SESSION_ID`) as well as its CLI session id. When the desktop restarts a worker's CLI under a new session id, a Claude in Chrome call in its desktop turn is still refused and pointed at `beekeeper browse`. Before, the call reached the desktop, which held the navigate for a person's site approval. The membership and multi-repository search refusals keep applying too ([#1439](https://github.com/giantswarm/beekeeper/issues/1439)).
- A session the watch opens in the running desktop app is handed over from a collected scope of its own under `app.slice` (`systemd-run --user --scope --collect`), as a new app already started: the forwarding Electron instance no longer starts in the `beekeeper-notify` unit's cgroup, where it crossed the unit's `MemoryMax`, was OOM-killed and failed the standby watch for its restart delay ([#1428](https://github.com/giantswarm/beekeeper/issues/1428)).
- `merge.cap` counts the machine's devctl merges and promotes only: a `devctl pr wait`, `release wait` or `rollout wait` polls and no longer takes a slot, so nine waits running leave a queued merge free to start under a cap of 5. The gate's line reads "N merges and promotes run machine-wide (cap M)" ([#778](https://github.com/giantswarm/beekeeper/issues/778)).
- `uninstall` resolves a symlinked unit: the person's link stays, and a target an earlier install wrote through the link is removed, its unit stopped without being disabled (systemd removes a linked unit's symlink on disable); `uninstall --dry-run` names the target it would remove and the link that stays. A target the person changed since stays as theirs ([#764](https://github.com/giantswarm/beekeeper/issues/764)).
- The secrets hook reads `ps` as the command word only: `ollama ps <model>` and a `curl` or `wget` of `/api/ps` with options after the URL (`-H …`, `-w …`) pass, where the words after them were read as BSD-style `ps` options and refused as a ps with command lines; `ps aux`, `ps -ef`, `ps -o cmd`, and `ps` after a wrapper, `watch`, `ssh <host>`, `docker exec` or `kubectl exec --` keep their refusal ([#812](https://github.com/giantswarm/beekeeper/issues/812)).

### Removed

- `note migrate` and the memo code are gone: the state keeps no memos and no pinned notes, `board next` reads priorities and holds from GitHub only and is never refused over an older beekeeper's memos, `self-update` names no migration, and the hand-overs' standing instructions are `standing.md` alone. A state document an older beekeeper left with memos keeps them as written and unread; the Note resource has no `pinned` field ([#938](https://github.com/giantswarm/beekeeper/issues/938)).

## [0.149.1-rc.3] - 2026-10-18

### Fixed

- The hook writes no gate into a variable's assigned value: `body="Proof: devctl pr merge o/r 7 ran"; gh issue comment 1 --body "$body"` passes unchanged, where the gate's path used to be inserted into the comment's text. A variable holding a blocking devctl command passes only where the call expands it as text (a quoted argument of gh, git commit, echo or printf, a here-document that is content, another such variable); one that may run (`$c`, `eval "$c"`, `bash -c "$c"`, a shell's input, an exported or prefix value, or a line that exports or reaches variables indirectly: `export`, `set -a`, `declare -x`, `-n` or `-i`, `${!…}`, `printf -v`, `read`, `set` with `a` or `allexport` in any option, an arithmetic context) is refused ([#1067](https://github.com/giantswarm/beekeeper/issues/1067)).

## [0.149.1-rc.3] - 2026-10-18

### Fixed

- A message to "the supervisor" or "the guide" during a relay reaches a run that takes it. The successor's start, run in its headless first turn, now records the relay's desktop id on its holder, so the role resolves to that desktop session instead of a bare run name nothing answers. From that start, for the role's `relayTTL`, while the successor's CLI takes no message (no CLI, or no peer socket yet), the message goes to the outgoing holder the relay relieved, while that one's CLI does; never to an older relieved run ([#634](https://github.com/giantswarm/beekeeper/issues/634)).

## [0.149.1-rc.1] - 2026-10-18

### Fixed

- A gated wait (`devctl pr wait`, `release wait`, `rollout wait`) ends with its caller: once its gate is gone without leaving the run to run on (its shell stopped by a TaskStop, killed, or gone with an ended turn), the run's merge-child stops devctl within a second and wakes nobody, where devctl used to run on to its own timeout. A wait another run of the same command follows runs on while that follower runs. A wait the call leaves on purpose (merge.toolLimit, which now applies to a foreground wait as to a merge, or the caller's `timeout`) runs on and wakes its owner as before, as does a merge's own run ([#1372](https://github.com/giantswarm/beekeeper/issues/1372)).

## [0.149.1-rc.1] - 2026-10-18

### Fixed

- The hook's shell scan reads `$'…'` to its first unescaped quote and decodes its escapes in a command's words, so `echo $'a\''; devctl pr merge o/r 7` has its merge gated and a `$'…'` argument containing `\'` stays text to its end ([#1380](https://github.com/giantswarm/beekeeper/issues/1380)).
- The build rewrite, the gate's command-position test, the script-file merge check and the guide's work check read a shell's `-c` or `eval` string as the merge gate does, its pieces joined and `$'…'` decoded: `bash -c 'cd x && '"go test ./..."` runs through `beekeeper run`, and a merge in such a string inside a script file is refused ([#1381](https://github.com/giantswarm/beekeeper/issues/1381)).

## [0.149.0] - 2026-10-18

### Fixed

- The merge gate reads a shell's `-c` or `eval` string as the shell builds it: adjacent single-quoted, double-quoted, `$'…'` and bare pieces are joined and `$'…'` escapes decoded before it looks for a blocking devctl command (eval's words joined by a space), so `bash -c 'cd x; '"devctl pr merge o/r 7"` is refused with the gated form, as a single-quoted string is. A string the parser cannot resolve (a quote left open, a command substitution) fails closed: the rest of the command line counts as its text, and one that names devctl other than behind the gate is refused. Words after a `-c` string are its arguments and stay out of it ([#1373](https://github.com/giantswarm/beekeeper/issues/1373)).

## [0.149.0-rc.12] - 2026-10-18

### Added

- `alerts.kubeconfig` lists kubeconfig files the installations' reads resolve their contexts in, passed to kubectl as `KUBECONFIG` for the context listing, every Alertmanager port-forward and the upgrade reading, behind the machine kubeconfig unless the list names it. An installation whose context lives only in a kubeconfig of the watch's own (`KUBECONFIG=<file> tsh kube login <cluster>`) is read by `beekeeper alerts snapshot` and the watch without a `KUBECONFIG` in the environment ([#705](https://github.com/giantswarm/beekeeper/issues/705)).

## [0.149.0-rc.11] - 2026-10-18

### Fixed

- The merge gate's rewrite puts the gate only before a blocking devctl command the shell runs: on the command line, in a shell's `-c` or `eval` string, or in a here-document a shell reads. A command named after a `;`, `&` or `(` inside a quoted argument of a command that reads it as text (a `gh … --body` text, a `git` commit message, a `grep` or `jq` pattern, an `echo`) stays as written, unless it sits in a subshell or a command or process substitution or its pipeline feeds a command that may run it; a quoted argument of any other command (`flock -c`, `watch`, `su -c`, a shell with options before its `-c`, `git rebase -x`) keeps its gate. ([#1364](https://github.com/giantswarm/beekeeper/issues/1364)).

## [0.149.0-rc.8] - 2026-10-18

### Fixed

- A finished worker's leftover background commands no longer keep its session from being archived. `agents idle --done` stops every tool shell of the calling session's CLI but the one it runs under, with the processes below them (a wait loop, a port-forward), and says which it stopped. The doctor stops the tool shells of a done agent whose CLI runs nothing else but `sleep`s (no headless turn, no gated merge, no transcript written within 30s), so it takes the agent off the roster and archives its session; `--dry-run` says "would stop". Such a sleeping wait used to count as a running turn for hours ([#1360](https://github.com/giantswarm/beekeeper/issues/1360)).

## [0.149.0-rc.7] - 2026-10-18

### Fixed

- The hook that refuses a blocking devctl command run outside the gate keys on the commands a call executes, not on prose that names them. An interpreter's program (a `python3` here-document, a script file, a `-c` or `-e` string) runs a command from a string literal that starts with it or names it after a shell operator; a comment, or document text in a literal (a README, a CHANGELOG entry), runs nothing. A shell's `-c` or `eval` named inside a quoted argument or a content here-document (a PR body draft) is text too. The same commands run by the program or the shell are refused or gated as before. ([#1033](https://github.com/giantswarm/beekeeper/issues/1033)).

## [0.149.0-rc.3] - 2026-10-17

### Fixed

- A gated merge that GitHub declines because the base's classic branch protection requires a review ends with one gate line naming the cause and the fix: `align: true` on the repository's entry in its giantswarm/github team file (the entry and team file devctl names), and `devctl repo status <owner/repo>` to check it. The fix used to be cut off at the end of devctl's long reason, which the gate truncates. No token the gate runs with may change `enforce_admins` (HTTP 403). ([#1340](https://github.com/giantswarm/beekeeper/issues/1340)).

## [0.149.0-rc.2] - 2026-10-17

### Fixed

- A `beekeeper run` command's go-git SSH transport reads a known_hosts it parses: when the caller sets no `SSH_KNOWN_HOSTS` and `~/.ssh/known_hosts` or `/etc/ssh/ssh_known_hosts` holds a line Go's `knownhosts` cannot parse (a host key type it does not know, `ssh-mldsa44-ed25519` for one), the command gets `SSH_KNOWN_HOSTS` naming `<stateDir>/ssh/known_hosts`, a copy of both without those lines; the machine's files stay untouched. Every go-git SSH test (five in gitsemver) failed with `knownhosts: … unknown key algorithm` on a machine whose known_hosts held such a key ([#1351](https://github.com/giantswarm/beekeeper/issues/1351)).

## [0.149.0-rc.1] - 2026-10-17

### Added

- `beekeeper secret copy`, `fingerprint` and `compare` take one value of a plain file on the host as a source, `file://<absolute path>#<path>`: a dotted key of the file's JSON or YAML mapping or a line of its dotenv lines, read in beekeeper's process. A credential a CLI writes as a plain JSON file (the Codex CLI's `auth.json` after `codex login --device-auth`, for one) reaches a lab's Secret without a session reading the file. Only files under `secret.fileRoots`, their symlinks resolved, are read, never written, moved or deleted; a path outside the roots (the refusal names them), a missing file or a missing key is refused in one line naming it, never a value, and a `file://` destination is refused ([#1349](https://github.com/giantswarm/beekeeper/issues/1349)).

## [0.148.1-rc.2] - 2026-10-17

### Fixed

- No trim of the shared Go build cache removes an entry while a `beekeeper run` compiles: every run holds the cache's use lock (`beekeeper-use.lock` at the top of the cache) shared for the length of its command, a trim takes it exclusive and otherwise waits, the trim past the cap by a quarter included, which ran under any build, and a run that begins during a trim waits for its end within its `--wait`. A `go build` under `beekeeper run` failed with `could not import net/http … no such file or directory` when a trim removed an entry the compile read ([#1335](https://github.com/giantswarm/beekeeper/issues/1335)).

## [0.148.1-rc.1] - 2026-10-17

### Fixed

- `sessions serve` and every command naming a session resolve a registered agent by its task title, the roster name with the suffix `agents start` sets (`FIX beekeeper 1323 (opus, builds)`), and a name that matches nothing is refused with the three closest roster names ([#1333](https://github.com/giantswarm/beekeeper/issues/1333)).

## [0.148.0-rc.29] - 2026-10-17

### Fixed

- The watch reads the parked agents' and the notes' issues and pull requests in GraphQL requests of at most 20 references, each batch with its own try timeout and a minute for its retries, so a parked set of any size is read without one request over the whole set. A batch that still fails is named in the line (`refs batch 2/3 of giantswarm (20: giantswarm/a#1 … giantswarm/b#9): …`), and the other batches' agents and notes settle as usual ([#1324](https://github.com/giantswarm/beekeeper/issues/1324)).

## [0.148.0-rc.27] - 2026-10-17

### Fixed

- A devctl merge's tool-release window lifts once the merge's release candidate is published with this platform's binary, while the merge's devctl still waits for the rest of the release, instead of holding every other merge until that devctl ends; `hold list` and `hold check` name the candidate as the window's end ([#1326](https://github.com/giantswarm/beekeeper/issues/1326)).

## [0.148.0-rc.25] - 2026-10-17

### Fixed

- A package script that runs a lab e2e proof (`yarn test:e2e:agentlab`, `npm run test:e2e:agentlab`: its script name names `agentlab`) no longer waits for or holds the build slot, like the `agentlab … backstage-test` form: the Bash hook no longer routes it through `beekeeper run`. Other package scripts (`yarn test:e2e`, `yarn tsc`) keep the slot ([#1322](https://github.com/giantswarm/beekeeper/issues/1322)).

## [0.148.0-rc.24] - 2026-10-17

### Fixed

- `agents wake` on a session whose desktop import is pending no longer resumes it headless beside the CLI the import warms: it is refused, saying what the import waits for and until when. A start records its import and first desktop turn as pending, so `agents` shows such a session `importing, until <t>` instead of `not running` ([#1318](https://github.com/giantswarm/beekeeper/issues/1318)).

## [0.148.0-rc.23] - 2026-10-17

### Fixed

- A repository hold whose `--lift-when` probe waits on that repository's release or tag (it names the repository and `release` or `tag`: `gh release view v1.2.3 --repo owner/repo`) lets the repository's own promote through the gate, the step that cuts that release: the gate no longer refuses it (exit 77) or keeps it waiting for a lift that cannot come, and merges into the repository stay held. `hold list`, `hold check` and the gate's lines name such a hold `owner/repo except its promote` ([#1317](https://github.com/giantswarm/beekeeper/issues/1317)).

## [0.148.0-rc.18] - 2026-10-17

### Fixed

- A Google OAuth client on record names the account its sign-in chooses: `beekeeper app allow <name> --google --callback <host> --account <address>`. The hook answers a click on Google's account chooser of that client's flow only when the call's words name that account, refusing another account and "Use another account", and a browse turn's prompt and auto mode rule name it. Before, a browse turn with several signed-in Google accounts could pick another one and stop at its sign-in challenge ([#1260](https://github.com/giantswarm/beekeeper/issues/1260)).

## [0.148.0-rc.16] - 2026-10-17

### Fixed

- `beekeeper install` never writes through a unit file that is a symlink (`beekeeper-teleport.timer` linked into the person's dotfiles): it keeps the link and its target byte-identical, says why (`keep <unit>: a symlink to <target>: install never writes through a link; …`), neither starts nor restarts that unit, and drops it from its manifest, so `uninstall` neither stops nor removes it. A regular unit file is written and started as before ([#1305](https://github.com/giantswarm/beekeeper/issues/1305)).

## [0.148.0-rc.15] - 2026-10-17

### Fixed

- `beekeeper browse --file` never uploads into a hidden file input: before each `file_upload` the turn runs beekeeper's upload-target check, which marks the one visible, enabled file input of the intended form, and the hook refuses a `file_upload` to any other ref (a hidden review-reply box took two uploads and left an orphaned attachment). After one screenshot of a session timed out on a frozen renderer, the hook refuses every further screenshot and zoom, so the turn reads the page within one timeout instead of waiting out one per screenshot ([#1302](https://github.com/giantswarm/beekeeper/issues/1302)).

## [0.148.0-rc.14] - 2026-10-17

### Fixed

- The supervisor's `yours <resource>` to a session queued behind earlier grants moves its grant to the head of the resource's queue, so its next `lease claim` is held instead of queued; the grants it passes keep their order behind it. `beekeeper lease grant --first` does the same without the word, refused to anyone but the supervisor while one is recorded (`--as` included); a plain `lease grant` still queues behind the earlier grants ([#1304](https://github.com/giantswarm/beekeeper/issues/1304)).

## [0.148.0-rc.12] - 2026-10-17

### Changed

- The shipped `merge` skill's gated merge line is `devctl pr merge <owner/repo> <n> --update-branch` (`--rebase --update-branch` on a rebase line): from devctl 8.140.0 a head behind its base is exit 3 without it, strict protection or not. An exit 3 that still says behind on a squash line (GitHub refused the update) gets the base merged into the branch and pushed, then the same merge once more; a rebase line keeps its fresh PR from the base. A test holds every shipped skill's gated merge line to `--update-branch` ([#1306](https://github.com/giantswarm/beekeeper/issues/1306)).

## [0.148.0-rc.9] - 2026-10-17

### Fixed

- The Teleport login's keeper renews within seconds of resume after a suspend that outlasted the login: `beekeeper-teleport.timer` is a calendar event on the wall clock (`OnCalendar=`, `Persistent=true`), whose tick missed during the suspend fires at resume, where its monotonic `OnUnitActiveSec=` stopped with the machine and fired up to `teleport.every` after. The keeper records its last run (`checkedAt`): the run after a missed tick tries a login whose renewal failed before the suspend again, and a failure of that run is said by the watch (`TELEPORT RENEWAL FAILED`, `teleport.fail`) and tried once more at the next run before the sign-in note. A rerun of `beekeeper install` rewrites the timer ([#1289](https://github.com/giantswarm/beekeeper/issues/1289)).

## [0.148.0-rc.6] - 2026-10-16

### Fixed

- The worker rules and the Secret guard's refusals give one answer about comparing a secret: `beekeeper secret compare|fingerprint`, which the broker answers without the value reaching the session, is the one allowed comparison, and any other comparison of a value (a hash, a diff, `grep -c|-q`) stays refused. The worker rules forbade every comparison while the refusal offered `secret compare` ([#1279](https://github.com/giantswarm/beekeeper/issues/1279)).

## [0.148.0-rc.5] - 2026-10-16

### Fixed

- A devctl release window no longer holds every other repository's merge and promotion while the devctl pull request waits for its CI: the installed devctl is the latest release until the release is coming, so the window holds the other merges only once GitHub reports its pull request merged (a promotion's from its start), until the release is installed. giantswarm/devctl's own next merge still waits for the release ([#1278](https://github.com/giantswarm/beekeeper/issues/1278)).

## [0.146.3] - 2026-10-16

### Fixed

- `agents start --builds` waits its turn for the build slot instead of being refused while it is busy: a start refused by the busy slot alone takes a ticket in a build slot queue (`<slotDir>/queue/`) that `beekeeper run`'s waiters now share, and the slot goes to the waiters first in first out, up to `MEMCAP_WAIT`, else 8 minutes, 60 in a background Bash call. A slot held by run after run no longer refuses every build start: 13 build starts had waited 1.5 h while two sessions alternated test runs ([#1249](https://github.com/giantswarm/beekeeper/issues/1249)).

## [0.146.2-rc.1] - 2026-10-16

### Fixed

- `beekeeper board next` offers no item while the state keeps memos and pinned notes an older beekeeper filed, which held the items they name: it is refused (exit 3), naming their count and `note migrate --dry-run`, then `--apply`. `self-update` says the migration the state still needs once the new binary is installed. A release that stopped notes holding board items took effect before the desk ran `note migrate`, and `board next` offered 33 items they had held ([#1248](https://github.com/giantswarm/beekeeper/issues/1248)).

## [0.146.1] - 2026-10-16

### Fixed

- `beekeeper browse` closes every tab it opens, the blank ones included. Replacing a frozen tab by a fresh one closed the tab the turn's group was made with first, which dissolves the group and strands the fresh tab outside it, out of every call's reach: the hook now refuses that close, naming the order (the group's first tab last), and a turn that would end with tabs of its own open goes on once to close them (`beekeeper hook browsestop`, the turn's Stop hook). Below the report, `tabs closed:` counts them, or `tabs left open:` names those left and the command fails ([#1176](https://github.com/giantswarm/beekeeper/issues/1176)).
- `beekeeper browse` recovers a screenshot that times out on a tab that says visible (its window occluded or not drawn): the turn reloads the same tab once and screenshots again, and on a second timeout reads the page's text instead and names the page not rendered, rather than retrying until the turn's timeout ([#1201](https://github.com/giantswarm/beekeeper/issues/1201), [#1207](https://github.com/giantswarm/beekeeper/issues/1207)).

## [0.146.0-rc.5] - 2026-10-16

### Added

- `beekeeper secret copy <ref> -- <consumer…>` admits `valkey-cli` and `redis-cli` with `--askpass`, which reads the password from stdin, in a pod through `kubectl exec -i` against the pod's own server: `--scan [--pattern <glob>]` lists key names, and `DEL`, `UNLINK`, `EXISTS`, `TYPE` and `TTL` act on named keys. A lab proof drops a component's server-side session in its Valkey, `k8s://kind-<lab>/<namespace>/<secret>/<key>` as the password, without the password reaching the session. A host or port (`-h`, `-p`), a password on the argv (`-a`, `-u`), any other option and every other command (`GET`, `CONFIG`, `ACL`, `KEYS`, …) stay refused ([#1241](https://github.com/giantswarm/beekeeper/issues/1241)).

## [0.146.0-rc.3] - 2026-10-16

### Fixed

- The plugin's skill budget test names what is over its budget: the file, its size in bytes and the budget (`plugin/skills/worker-rules/SKILL.md (the worker rules): 2,731 bytes, budget 2,700 (1,000 tokens at 2.7 bytes per token)`), for the worker rules, each topic skill and its description, never a bare red build ([#1231](https://github.com/giantswarm/beekeeper/issues/1231)).

## [0.145.4-rc.1] - 2026-10-15

### Fixed

- `beekeeper run` no longer takes a build slot whose lock is free while its slice still runs another run's scope (a run that joined the slot outlives the run that held it): capping that slice at a smaller `--max` killed the neighbour or the fresh `systemd-run` at 0 MiB at the slice's cap. The run takes another free slot or waits, naming the slice's runs and memory. A kill of `systemd-run` before the command started is reported as the slot's other runs filling its cap, and a kill at a joined slot's shared cap as the slot's runs together, never as the command exceeding its `--max` ([#1230](https://github.com/giantswarm/beekeeper/issues/1230)).

## [0.145.3] - 2026-10-15

### Fixed

- `beekeeper agents note` waits 3 s at most for the state lock instead of 2 min: a lock held longer (a desktop import of the session pending) fails the note unsaved within seconds, naming the lock's holder and the pending import, never a bare `context deadline exceeded` ([#780](https://github.com/giantswarm/beekeeper/issues/780)).

## [0.145.2] - 2026-10-15

### Fixed

- A stuck holder of the state lock no longer starves every other command. The desktop import freezes a session's headless turns only while no process holds the lock, so a turn's SessionStart hook is never frozen holding it; the hook waits for the lock two seconds at most and otherwise records nothing. Every other wait for the lock ends after two minutes with the holder's PID, command and how long it has held the lock (`state.lock.holder`). A hook frozen with the lock for the import had held it for 25 minutes, and `status`, `note add` and `agents handover` hung behind it ([#781](https://github.com/giantswarm/beekeeper/issues/781)).

## [0.145.2-rc.1] - 2026-10-15

### Fixed

- `beekeeper agents handover` of an agent whose roster row holds no task (none open, none it reported idle on) is refused (exit 3) before it asks for anything, instead of starting a follow-up busy with `""`; `--task "<task>"` gives the task, which the follow-up's prompt and roster row carry. Before the old session ends, the hand-over makes sure the roster lists the follow-up under the name busy with its task, restoring a lost row or task; a follow-up that cannot be listed fails the hand-over and the old session runs on. `watch` no longer says `HANDOVER DUE` for an agent with no task at all ([#803](https://github.com/giantswarm/beekeeper/issues/803)).

## [0.145.1] - 2026-10-15

### Fixed

- A merge queued under the GitHub budget floor runs at the first reset after it was queued, before the waits. For `merge.resetFirst` (5m) from the reset the merges queued for it go first, the oldest queued first: a gated `devctl pr wait`, `release wait` or `rollout wait` starts after them, the watch's GitHub timer checks wait, `beekeeper budget` names them and `budget --gate` refuses; only the running merges count against `merge.cap` for them, not the waits' devctl processes. The gate no longer gates on a budget reading whose reset has passed, and the waits run with what the merges leave. Queued merges had waited through several resets while the waits drew the budget under the floor again within minutes ([#777](https://github.com/giantswarm/beekeeper/issues/777)).

## [0.145.0] - 2026-10-15

### Added

- `beekeeper alerts own` takes one instance of an alertname, `<installation>/<alertname>#<fingerprint>` or its fingerprint, and leaves the alertname's other instances unowned; the bare `<installation>/<alertname>` and alertname forms still take every instance, and each output line names the instance it took. `beekeeper alerts disown` with the same forms releases the owned alerts it names (`alert.disowned`): the watch counts them unowned from then and says `PAGE UNOWNED` once they have had no owner for `alerts.ownerGrace`; naming no owned alert is refused ([#605](https://github.com/giantswarm/beekeeper/issues/605)).

## [0.144.0] - 2026-10-15

### Added

- `beekeeper sessions serve --clear` removes the calling session's own record, and `agents idle --done` clears the finished agent's record. A worker's `sessions serve` naming another session is refused (exit 3): a worker's records never land on another session's name, and a record it wrote is its own to clear ([#772](https://github.com/giantswarm/beekeeper/issues/772)).

## [0.142.0] - 2026-10-15

### Added

- `beekeeper answer <owner/repo#n> [--note <id>] [--choice <n>] [--via cli|slack] "<words>"` records a person's words verbatim on the GitHub thread they concern, however they came: an agent-written comment naming who answered, how it came and the note it answers, `answer.recorded` in `beekeeper log` with the thread, the comment's URL and the words, the named note closed and the asking session told (the note's filer, else the sessions serving the thread). A public thread gets nothing: the comment goes to the first private plan pull request naming it or an epic above it, then to the nearest epic above it in a private repository; with neither the answer is refused, naming what to add. `note answer` is this verb on the note's first `--ref`; `note.answered` is no longer written, and the hand-over's answers and `note add`'s re-ask warning read `answer.recorded` by thread ([#942](https://github.com/giantswarm/beekeeper/issues/942)).

### Changed

- A decision (`note add --for` the person) is refused without `--ref`: every decision names the thread its answer is recorded on ([#942](https://github.com/giantswarm/beekeeper/issues/942)).

## [0.141.5-rc.1] - 2026-10-15

### Fixed

- The shipped `merge` skill names the squash subject for a personal account's repository, merged with the person's own `gh` outside devctl: `gh pr merge <n> --repo <owner/repo> --squash --subject "<PR title> (#<n>)"`. Without `--subject`, `gh` took a one-commit PR's commit message as the subject, which a force push cannot shorten and auto-release turns into the tag text ([#1203](https://github.com/giantswarm/beekeeper/issues/1203)).

## [0.141.4] - 2026-10-15

### Fixed

- `beekeeper board move` moves an issue of a repository outside the board's owner on that owner's own project the issue is on (a personal account's user project), by that project's Status values, without a Team or the epic gate; the output names the board. An issue on no such project is refused with the `gh project item-add` recipe, one on several with the `gh project item-edit` one, and the board-read hook's message says where `board move` reaches. Such an issue could only be moved by item id ([#1198](https://github.com/giantswarm/beekeeper/issues/1198)).

## [0.141.4-rc.1] - 2026-10-15

### Fixed

- The shipped `github` skill says a code scan (govulncheck, trivy) runs in a fresh worktree of `origin/main`, never a shared clone, and that a finding names the commit and module versions it was raised on; the supervisor dispatches a scan finding only once that commit is the default branch's head or a re-scan there still raises it. A scan of a weeks-old shared clone had reported vulnerabilities main had fixed ([#1199](https://github.com/giantswarm/beekeeper/issues/1199)).

## [0.141.3-rc.1] - 2026-10-15

### Fixed

- The Go build cache stays near `doctor.goCacheMaxGiB` on a desk where a go build almost always runs: past the cap by a quarter the doctor trims the least recently used entries even while builds run (a build takes a removed entry for a miss and builds it again), and the trim's line names the builds it ran beside. A cache that stays over the cap across two `doctor.goCacheEvery` intervals is one `GO CACHE` line with its size, the cap and how long it has waited. The trim kept being skipped and the cache grew past its cap without bound ([#591](https://github.com/giantswarm/beekeeper/issues/591)).

## [0.141.2] - 2026-10-15

### Fixed

- The upgrades reader reads every installation the alerts reader reaches. A shared upgrades reading without a kube context (`upgrades.json`, written by a watch, snapshot or ui whose kubeconfig lacks the context) is never fresh: each process resolves the installation's context through its own kubeconfig again, and reads it at once when the context is there. A watch run with `KUBECONFIG=~/.kube/config:<teleport kubeconfig>` named the Teleport installations as without a kube context for up to `upgrades.every` after another process without that list had read them ([#779](https://github.com/giantswarm/beekeeper/issues/779)).

## [0.141.1] - 2026-10-15

### Fixed

- A gh read GitHub answers transiently is asked again: an HTTP 5xx, an empty body and a cut-off one (`unexpected end of JSON input`), up to three tries 2s and 4s apart. It covers every read through the gh CLI: `board next` and the board's other GraphQL queries, issue and pull request reads, the watch's reads of notes and reviews, the gate's pull request state. A read still failing names its call, what went wrong and the tries (`gh api graphql (repositoryOwner): HTTP 502 after 3 tries: …`); a write (a mutation, a REST call with a body) is asked once, as GitHub may have applied it. `board next` failed on a single empty GraphQL answer and on a single 504 ([#753](https://github.com/giantswarm/beekeeper/issues/753), [#544](https://github.com/giantswarm/beekeeper/issues/544)).

## [0.141.0] - 2026-10-15

### Added

- The doctor keeps Docker's images and build cache under `doctor.dockerImagesMaxGiB` (40) and `doctor.dockerBuildCacheMaxGiB` (20): every `doctor.dockerEvery` (1h) a watch removes the unused images created longest ago and prunes the build cache down to three quarters of each cap. An image a container or a kind lab's node uses is never removed, and nothing is pruned while an image import or build runs or a lab lease is under an hour old. Each prune is a `docker.prune` event with what it reclaimed; a prune held back an hour, or a failed one, is a `DOCKER` line. A lab machine had 76 GiB of images and 39.5 GiB of build cache nothing trimmed ([#1178](https://github.com/giantswarm/beekeeper/issues/1178)).

## [0.140.2-rc.1] - 2026-10-15

### Fixed

- A bare `#n` names an issue of the repository before it only when it continues a list of references (`beekeeper#173, #176`, `o/r: #1 (a), #2`); after prose it names nothing. A cross-repository memo, "gpm#282 is live; then tick #37850", listed `giantswarm/gpm#37850` among its `names:` in `note list --full`, and `board next` skipped by it. A beekeeper item (`notes #3 and #4`) ends a list too ([#1184](https://github.com/giantswarm/beekeeper/issues/1184)).

## [0.140.1] - 2026-10-15

### Fixed

- A gated merge's outcome reaches its worker after a desktop restart. merge-child owes the outcome in the state (`owed`) before it hands it over, and the watch hands over what is left: an outcome whose merge-child was killed on the way, a wake the service manager refused (`Transaction … is destructive` while the user manager stops) and a merge stopped before devctl reported its end, whose outcome is what GitHub says of the pull request: merged at its time (logged `merged`, with `devctl release wait <repo> --pr <n>` for its release), open (run it again) or closed. A try a minute; GitHub's answer is awaited 30 minutes, after which the worker hears the stop without it, and an outcome nobody could take is given up after a day, logged as `devctl.unheard`. The merge whose user manager stopped with a desktop restart had merged on GitHub, but its worker was never woken (its wake was refused) and had to query the pull request itself ([#1182](https://github.com/giantswarm/beekeeper/issues/1182)).

## [0.140.0] - 2026-10-14

### Added

- `beekeeper note list --full` prints each note's whole text, every line of it, indented under its id and tags, with its default and the issues and pull requests it names (`names: owner/repo#n, …`, from its text and its `--ref` links, read as `board next` reads them); `--json --full` adds them as each note's `names`. A scan sees what a memo names in one call, without reading each note. The default output is unchanged ([#920](https://github.com/giantswarm/beekeeper/issues/920)).

## [0.140.0-rc.2] - 2026-10-14

### Added

- `beekeeper board next --limit <n>` lists at most n items of each list, the skipped lines above the pick and the free items behind it (and `--json`'s `skipped` and `after_pick`), each followed by how many it left out (`… 12 more skipped above the pick (--limit 3)`, `--json` `skipped_more` and `after_pick_more`); the pick, the counts by kind and the epics stay whole. `--fields ref,title,url` (implies `--json`) keeps only the named fields of each item in `pick`, `skipped` and `after_pick`, an unknown name refused with the names it knows. Without either the output is unchanged. The whole walk printed about 42 KB for the roadmap board, which a scan had to dump to a file and read back ([#930](https://github.com/giantswarm/beekeeper/issues/930)).

## [0.140.0-rc.1] - 2026-10-14

### Fixed

- `beekeeper spend`, `report` and `log` take a bare date (`2026-10-08`) for `--since` and `--until`: that day's 00:00 in the command's zone, for both bounds, so `--since 2026-10-08 --until 2026-10-09` is the whole of October 8; it was refused as `neither a time nor a duration` ([#769](https://github.com/giantswarm/beekeeper/issues/769)). `beekeeper log` gets `--until` beside `--since` (events before it), combinable with `--verb` and `--json`; `--since` is read back from `--until` when both are given ([#810](https://github.com/giantswarm/beekeeper/issues/810)).

## [0.139.2] - 2026-10-14

### Fixed

- The shipped worker rules (`beekeeper:worker-rules`) name `KUBECONFIG="$HOME/.kube/config:$HOME/.local/state/beekeeper/kube/teleport.yaml" beekeeper alerts snapshot` as the read path for a post-roll or proof alert check, never Alertmanager or Mimir directly; a promoter worker had tried Mimir's Alertmanager API and fallen back to a weaker check ([#1167](https://github.com/giantswarm/beekeeper/issues/1167)).

## [0.139.2-rc.1] - 2026-10-14

### Fixed

- `beekeeper note answer` on a note a supervisor or guide run filed reaches the session holding that role now, not only the run that filed it: an answer to a note of a run a relay ended failed to be told with `no running session or registered agent matches`. The output names the holder told when it is another run than the filer. While no session holds the role, or its holder cannot be told, the command says so and the answer waits in the state for the role's next `supervisor start` or `guide start`, which prints it as `waiting for the <role>: NOTE ANSWERED …`; a decision closed with its default is routed the same way ([#1162](https://github.com/giantswarm/beekeeper/issues/1162)).

## [0.139.1-rc.1] - 2026-10-14

### Fixed

- `beekeeper board move`, `board rank` and `priority set` never add an issue of a repository outside the board's owner (a personal account's, another organisation's) to the board: one whose `projectItems` name no item of the board is refused (exit 3) before anything is written, naming its owner's own project as its place. A move of a personal account's private issue had added it to the organisation's board ([#1157](https://github.com/giantswarm/beekeeper/issues/1157)).

## [0.139.0-rc.1] - 2026-10-14

### Added

- `beekeeper secret import <src> --for <sops-file|directory>` takes the age recipient whose identity it imports from the `.sops.yaml` the repository holds, instead of repeating it as `--recipient <age1…>`: a SOPS file's recipient (its metadata's when encrypted, else its creation rule's), or for a directory that of the creation rule sops applies to the files below it, else the one recipient all its creation rules share. No rule, a rule without an age recipient, or several recipients is refused in one line naming the path; `--recipient` keeps working and wins when both are given ([#728](https://github.com/giantswarm/beekeeper/issues/728)).

## [0.138.10] - 2026-10-14

### Fixed

- The gate's devctl release window lifts within minutes of a devctl release's binary assets, not after the release workflow devctl's own merge waits for (about 25 minutes): once GitHub reports the window's pull request merged (a promotion's from its start), the watch and the waiting gates run `devctl version update` every minute while the merge's devctl still runs, and the window lifts as soon as `devctl version` reports the new release; the merges held behind it start on it without a manual step. An update that fails, a release whose binary never verifies against its signature bundle, keeps the window, is logged as `hold.update` and is named on the hold: `beekeeper hold` and a held merge's refusal say `its last update failed, the window stays: <reason>` ([#748](https://github.com/giantswarm/beekeeper/issues/748)).

## [0.138.10-rc.3] - 2026-10-14

### Fixed

- `beekeeper watch` and `beekeeper snapshot` no longer print one line per installation whose kube context is not in the kubeconfig kubectl reads: the upgrade reader said `UPGRADES <installation> unreadable: Error in configuration: context was not found …` and the alerts reader `<installation> unreachable: error: context "…" does not exist` for each, on every start. Both readers share the cause, an explicit `alerts.installations[].context` the kubeconfig lacks, which context resolution now treats as no context (as it does an unresolved templated one); kubectl's own missing-context error counts the same when the contexts are unknown. Such an installation is not read, keeps its upgrade holds and its alert baseline, and is named with the others in one line per reader, once per watch (`UPGRADES: <n> installations without a kube context, not read: …`, `ALERTS: …`), and in one entry of the snapshot's upgrades and one line of its alerts. An installation whose context resolves and whose read fails keeps its own `UPGRADES … unreadable` line ([#743](https://github.com/giantswarm/beekeeper/issues/743)).

## [0.138.10-rc.1] - 2026-10-14

### Fixed

- `beekeeper doctor` warms no desktop CLI beside a running headless turn of the same session (`claude -p --resume <id>`, a wake's): its archive's steward warm counts that turn as the session's running CLI and leaves the archive for a later run, and a reopen (the doctor's of a rowless worker, a stop-post's) looks for any headless turn of the session right before its show, not only a wake unit, and is missed instead. A session with no CLI at all is still warmed. Sessions woken headless after a reboot got a second, desktop CLI from the doctor, and the watch said `TWIN CLI` for them ([#725](https://github.com/giantswarm/beekeeper/issues/725)).

## [0.138.9] - 2026-10-14

### Fixed

- A beekeeper binary run from elsewhere than PATH, such as a downloaded release candidate under a read-only proof, no longer syncs the user-scope `beekeeper@beekeeper` plugin on its first command, nor records the sync: only the installed beekeeper (the one on PATH, which a self-update replaces) brings the plugin up to date once per version, and `self-update --sync-plugins` run from elsewhere says it leaves the plugin as it is. A candidate's first command had updated the plugin, so its skills reached new sessions before the promote ([#1131](https://github.com/giantswarm/beekeeper/issues/1131)).

## [0.138.7] - 2026-10-14

### Fixed

- A `--rebase` merge queued in a lane behind another merge of its repository no longer fails its turn with devctl's exit 3 "behind" on a base that requires branches to be up to date: at its turn the gate reads the pull request again and, when it is only behind its base, has GitHub rebase it onto the base (Update with rebase, never a base merge into the branch), awaits the new head and runs devctl on it, which waits for the new head's checks and merges; the `merge.rebased` event and a gate line say so. One that conflicts by then, or that GitHub does not rebase, is refused (77) with nothing merged, its place out of the lane and the replacement PR named. devctl stays the only merger; its `--update-branch` merges the base in, which a rebase line forbids ([#1127](https://github.com/giantswarm/beekeeper/issues/1127)).

## [0.138.5] - 2026-10-13

### Fixed

- The beekeeper plugin follows the binary whatever installed it: a newly installed release brings every installed beekeeper plugin up to date on the first command it runs (once per binary, recorded as `plugin-sync` in the state directory; a Claude Code that does not list its plugins leaves it to the next command), and `self-update` hands the sync to the binary it installed (`self-update --sync-plugins`) instead of running its own. A self-update by a release without the sync left the plugin stale until a manual `claude plugin update` ([#1112](https://github.com/giantswarm/beekeeper/issues/1112)).

## [0.138.5-rc.3] - 2026-10-13

### Fixed

- `beekeeper guide watch` says `GUIDE WATCH STALE` once `self-update` replaced its binary (the version it runs, the one installed, the command that re-arms it), and a save a newer release refuses as `GUIDE SAVE REFUSED` with the same command and the lines that save would have recorded, again only when they change. `beekeeper self-update` names each watch it leaves on the old binary (`watch`, `alerts watch`, `guide watch`) with the command that re-arms it, as does `WATCH STALE`. The guide's watch stayed on the old binary after an install and its refused saves reached nobody as such, while only the supervisor's watch was re-armed ([#890](https://github.com/giantswarm/beekeeper/issues/890)).

## [0.138.5-rc.1] - 2026-10-13

### Fixed

- A build slot's holder record and a run's `run.start`/`run.end` events name the build that holds the slot: a command too long for the line (140 characters in the record, 100 in an event) leads with its first heavy command (`… go test -race -count=150 …`) instead of cutting it off behind what runs before it. A 12-minute `go test` after an assignment, a `git checkout` and a `git worktree remove` showed as `zsh -c S=<scratchpad>/amp; git -C "$S" checkout -q -- .`, which read as a checkout holding the slot. The record names the shell's `-c` script, no longer the shell. The hook's routing was right and stays as it was: a `git checkout`, `git switch` or `git worktree add` takes no slot, and a build after one does ([#1111](https://github.com/giantswarm/beekeeper/issues/1111)).

## [0.138.4] - 2026-10-13

### Fixed

- A giantswarm/devctl merge that releases only a candidate (`vX.Y.Z-rc.N`) lifts the devctl release window as soon as the gate records it: the window waited for the local devctl to report the candidate, which `devctl version update` never installs, so it held every other repository's merges until lifted by hand. A promotion of giantswarm/devctl opens the window instead and holds the merges until the local devctl reports the stable release; a promotion that ends without its document lifts it, the next devctl run updating devctl first ([#1113](https://github.com/giantswarm/beekeeper/issues/1113)).

## [0.138.4-rc.3] - 2026-10-13

### Fixed

- Every `agents.*` event reaches `events.jsonl`: an agent start's delivery line, a wake, a handover, a note and a broadcast waited at most a second for the state lock and were dropped silently when it was busy; they now wait for it as an update does. `beekeeper log --since <duration>` (`1h`, `30d`, a time or RFC 3339) shows every event of a window, past the 50 that `-n` shows by default, and `beekeeper log --help` states the retention: the log keeps every event since the machine's first, never trimmed or rotated ([#884](https://github.com/giantswarm/beekeeper/issues/884)).

## [0.138.4-rc.2] - 2026-10-13

### Fixed

- A call through muster (the watch's PagerDuty read, the feedback watch, every central verb) connects afresh each time instead of reusing a connection of the shared HTTP/2 pool, so a connection whose far end went away stalls one attempt, and the retry of the PagerDuty listing reads over a new one. A call that runs out of its deadline names the stage it timed out in and the deadline (`connect timed out (deadline 30s)`, `auth …`, `tool call …`) in place of `context deadline exceeded`. All calls had been multiplexed onto one HTTP/2 connection, and once it stalled every reading timed out until the kernel gave the connection up: PAGERDUTY unreachable for 16 minutes while muster answered in 80 ms ([#899](https://github.com/giantswarm/beekeeper/issues/899)).

## [0.138.0] - 2026-10-13

### Added

- `beekeeper app installation <installation> --app <id|slug>` reads and changes what GitHub's sudo mode keeps a browse turn from, as the App itself with the private key of the installation's Secret, no person and no value in any output: the installation's repository selection, suspension and reached repositories, the App's device flow (probed with its client id) and its user token expiry (no API carries it: named unread). `--suspend`/`--unsuspend` set the suspension as the App; `--repos` with `--token` (an owner's fine-grained token with `installation_repositories=write`, which no App can declare) makes a selected installation reach exactly those repositories and reads it back; `--repos --reinstall` deletes the installation as the App and answers the installation page with the repositories preselected, whose Install click meets sudo mode. An installation on all repositories, a suspended one and a missing token are refused in one line naming the setting. The installation's scope and the App's optional features could only be read on settings pages behind sudo mode ([#1094](https://github.com/giantswarm/beekeeper/issues/1094)).

## [0.137.2-rc.3] - 2026-10-13

### Fixed

- `beekeeper capacity` and the watch's SWAP line count zswap's writeback apart from disk swap's growth: disk swap growth is the swap in use less zswap's share (`SwapTotal` − `SwapFree` − `Zswapped` of `/proc/meminfo`) less the pages zswap moved from its pool to the swap device (`zswpwb` of `/proc/vmstat`), so the start guard reacts to new swap-outs only; the writeback is its own figure (`zswap Z MiB writing back W MiB/h` in the headroom line, `zswap writeback W MiB/h` in SWAP) and the watch's `swap.json` reading carries it as `writebackPerHourMiB`. The pool writing back with swap in use falling read as disk swap growing +949 MiB/h and warned against every start ([#894](https://github.com/giantswarm/beekeeper/issues/894)).

## [0.137.2-rc.2] - 2026-10-13

### Fixed

- `beekeeper secret copy --help` and the README said a brokered call runs in the broker's directory and that `git -C` takes an absolute path; the broker starts every brokered call, and the call its consumer, in the requester's working directory, so `git -C .` acts on the caller's checkout. A sandboxed session's brokered secret call in a directory its sandbox does not let it read is now refused, the directory named ([#873](https://github.com/giantswarm/beekeeper/issues/873)).

## [0.137.1] - 2026-10-13

### Fixed

- `beekeeper note add` folds a note into an open one for the same person only when it asks the same question (the same text, case, spacing and closing punctuation aside) on the same issue or PR, keyed on its `--ref`, else the first URL of its `--status-quo`. A shared first word and URLs cited as context no longer fold, so a grilling round's distinct questions on one plan PR stay separate notes with their own `--default` and `--recommend`. The same question with a different default or recommended option is refused, naming the open note; `--fold-into <n>` folds into open note `n` explicitly, whose default and recommendation stand ([#647](https://github.com/giantswarm/beekeeper/issues/647)).

## [0.137.1-rc.6] - 2026-10-13

### Fixed

- A test holds the gate's stdout to devctl's document alone on every route a blocking wait takes (owned, unowned, owned without its run files): the gate's own lines and devctl's progress (`waiting for image …`) go to stderr. `beekeeper gate --help` says so, and that a `2>&1` before `| jq` mixes the progress into the document, which is what broke a `devctl release wait … --timeout 9m 2>&1 | jq` at line 1, column 8 ([#875](https://github.com/giantswarm/beekeeper/issues/875)).

## [0.137.1-rc.4] - 2026-10-13

### Fixed

- A `beekeeper run` in a background Bash call waits the documented 60 minutes for a slot however the call reaches it: directly, after a `cd`/`export` chain or through a script. The PreToolUse hook starts every background call with `export MEMCAP_BACKGROUND=1;`, which `run` takes as the 60m default (`--wait` and `MEMCAP_WAIT` still win); before, only a heavy command the hook wrapped itself got it, and a `beekeeper run` written out, chained or inside a wrapper script gave up after 8 minutes. The wait lines and the exit-75 message state the wait applied and where it came from (`gave up after 60m (the background default)`), and a background run no longer advises running in the background ([#893](https://github.com/giantswarm/beekeeper/issues/893)).

## [0.137.1-rc.3] - 2026-10-13

### Fixed

- `beekeeper hook pretooluse` runs an invocation that only prints its help or version at once, whatever the command word and wherever it stands in the command line: its arguments are subcommand words followed by `-h`, `--help`, `help`, `-version` or `--version` (redirections aside), or it is `go help`, `go version` or `go env`. Such an invocation is never wrapped in a build slot and never refused by the lab or model-server guards for what the command would do without the flag; `govulncheck ./...` is still wrapped. `govulncheck -h` waited 8 minutes for a slot, `agentlab platform --help` 355 s, and `agentlab models-test --help` was refused for a model load ([#885](https://github.com/giantswarm/beekeeper/issues/885)).

## [0.137.1-rc.1] - 2026-10-13

### Fixed

- A merge whose release failed (devctl exit 6, `release.verdict` `ci_failed`) settles its installation lane at once: nothing rolls, so the next merge starts without the `merge.settle` wait; the merge records it (`releaseFailed` in the state and the MergeLane CRD), the `merged` event and the gate say `release failed, nothing rolls`, and `beekeeper lanes` says `release failed` instead of "until an unknown release rolls". The lane line names the release a settling merge waits for, calls a release candidate a pre-release (at once where the range admits none), and says `its release unknown, until <time>` for an unknown one ([#877](https://github.com/giantswarm/beekeeper/issues/877)).

## [0.137.0] - 2026-10-13

### Added

- `beekeeper hook pretooluse` refuses, before it runs, every GitHub write aimed at a repository outside `github.allowedOrgs` (default `giantswarm`): `gh pr` and `gh issue` create, comment, review, close, edit, ready, merge and their kin, `gh release`, `gh label` and `gh repo` writes, `gh api` and `curl` with a write method (given, or implied by a field or an input) on a `repos/<owner>/<repo>` or `orgs/<owner>` path, a `gh api graphql` mutation that posts without naming an allowed owner, a `git push` to a GitHub remote of another owner (the remote read in the worktree the command runs in), and a GitHub connector post. The repository comes from `--repo`/`-R`, a URL or `owner/repo#n` argument, the API path, or the repository gh itself acts on (`GH_REPO`, the `gh repo set-default` remote, else `upstream` before `origin`), so a plain `gh pr create` in a fork clone with an `upstream` remote is refused. The refusal names the rule in one line (a human decides what goes upstream: patches land on our fork and as a row on `github.upstreamReview`), is logged as a `hook.refused` event with the session, the repository and the command, and the supervisor's watch says it as a `REFUSED` line. Reads pass. Nothing but instructions kept an agent from opening pull requests and issues and commenting on upstream projects ([#1061](https://github.com/giantswarm/beekeeper/issues/1061)).

## [0.136.1-rc.3] - 2026-10-13

### Fixed

- A send to a running CLI (`agents wake`, `agents broadcast`, a steward's archive, retitle or reopen ask) whose relay turn fails on its own (the API refuses it, the login expired and cannot be refreshed, the `claude` command fails) names that error after one turn, `the relay turn failed: <error>`: it read `the sender called no SendMessage in 3 turns` after three turns that could not succeed, so an expired login looked like a refusing relay model. A clean turn without a SendMessage call is still tried three times ([#1063](https://github.com/giantswarm/beekeeper/issues/1063)).

## [0.136.1-rc.2] - 2026-10-13

### Fixed

- A gated run's waiter (its merge-child, a queued merge's or a wait's) stopped by a signal before its command settled (SIGTERM from a cleanup or a stop of its unit, SIGHUP, SIGINT) no longer wakes its owner as if the merge had settled: the wake names the stop, that nothing waits for the command any more and to run it again if it still needs to; a command that reported a merge before the stop still hands that merge over. A waiter whose owner left the roster wakes nobody, stopped or not ([#898](https://github.com/giantswarm/beekeeper/issues/898)).

## [0.136.1-rc.1] - 2026-10-13

### Fixed

- A gated devctl wait (`pr wait`, `release wait`, `rollout wait`) or merge wrapped in `timeout` runs on when the timeout fires: the gate took the timeout's SIGTERM, its caller still there, for a stop aimed at itself, stopped devctl (exit 143) and woke nobody. A SIGTERM from a `timeout` between the gate and the Claude Code CLI now cuts the connection, not the run: the call ends at once with exit 76 and says so, devctl goes on outside it and its verdict wakes the owner (`devctl.unheard`); a merge leaves as at its tool limit. The worker rules say a gated wait is never wrapped in `timeout`. `beekeeper gate --help` no longer names a local binary path ([#879](https://github.com/giantswarm/beekeeper/issues/879)).

## [0.136.0] - 2026-10-13

### Fixed

- A hold that lets one pull request through (`hold set <owner/repo> --except <owner/repo>#<n>`) keeps the lane's other merges out until that one has gone: a merge queued before the hold keeps its place and waits instead of being refused (exit 77) and losing it, the held place no longer holds up the exception behind it, and a merge whose devctl was still waiting for CI when the hold was set is ended before it merges and goes back to its place (`merge.preempted`); after the hold, the queued merge runs from its place without queueing anew. A new merge during a hold is still refused naming it ([#1054](https://github.com/giantswarm/beekeeper/issues/1054), [#835](https://github.com/giantswarm/beekeeper/issues/835)).

## [0.136.0-rc.1] - 2026-10-13

### Added

- The watch frees what dead sessions and dead go commands leave in the tmpfs `/tmp` by itself: every `doctor.freeEvery` (default 1h; negative: off) one watch runs `beekeeper free --apply --only session-dirs,go-build` under a lock, the watches sharing when it ran last, logs each run that freed something (`free.apply`) and says it in one `FREED` line with the count and size of each class; a failed run is `FREE FAILED` until one succeeds, and the `TMPFS` line says when it last ran and what it freed. `beekeeper doctor` frees the same (`--dry-run` says what it would). Nothing ran `free --apply` on its own, so 4 GiB of dead sessions' scratchpads (1087 dirs) and 3.8 GiB of `/tmp/go-build*` had piled up in RAM ([#860](https://github.com/giantswarm/beekeeper/issues/860)).

### Changed

- `beekeeper free` has a `go-build` section of its own: a go-build dir is orphaned once it is unchanged for an hour, no go command of the user runs that began before its last change, and no process holds anything inside; it was a throwaway temp dir untouched for `--stale-hours`. A session's scratch (`session-dirs`) is dead once its CLI is gone, also a newer CLI of its desktop session, no roster agent with its task and no role holder is the session, and the git worktree its transcript names is gone; a dead session's dir under 1 MiB goes too, counted without a line of its own. A process's open files and executable keep a dir as its working directory did, and each section of dirs ends with how many it frees and their size ([#860](https://github.com/giantswarm/beekeeper/issues/860)).

## [0.135.4-rc.1] - 2026-10-13

### Changed

- `beekeeper agents register --help` names the refusal of a nested claude, and `beekeeper sessions serve --help` and the README name the roster name the serve record stores ([#1047](https://github.com/giantswarm/beekeeper/issues/1047)).

## [0.135.3] - 2026-10-13

### Fixed

- `beekeeper watch` and `alerts watch` no longer resolve alerts on a stretched, cut-short or partial installation reading: a reading that misses alerts of the baseline keeps them with their first start and says `ALERTS <installation> <n> of <m> alerts missing from this reading, kept until the next reading confirms them resolved` (quiet when every one of them is), and they resolve only when the next reading misses them too; a failed reading keeps them as before. An alert keeps its first start across such readings, and one that fires again within `alerts.flap.window` after it resolved is one `FLAPPING` line with its first start instead of a NEW line, its later changes damped. Under CPU pressure whole page sets resolved together and fired again minutes later with new starts, a RESOLVED and a NEW line per alert each time ([#1045](https://github.com/giantswarm/beekeeper/issues/1045)).

## [0.135.2] - 2026-10-13

### Fixed

- `beekeeper sessions serve <session>` takes a registered agent's roster name: it resolved its argument by the session's own name only, so a worker whose desktop title differs from its roster name, or one the desktop has not titled yet, was refused with `no running session matches`. `sessions serve` and `lease grant` (and the supervisor's `yours <resource>`) now resolve a session by one rule: a session id, host id or PID, else a name matched whole, ignoring case, before a unique part of one, where a running session's names are its own and, for a registered agent, its roster name. A registered agent whose CLI does not run is recorded and the line says so; a name that matches nothing is refused with `no running session or registered agent matches "<name>": name one by its session name, roster name or session id`, and one that matches several lists them with their session ids ([#789](https://github.com/giantswarm/beekeeper/issues/789)).

## [0.135.1] - 2026-10-13

### Fixed

- The hand-over and its `--prompt` keep the session record of an agent the roster keeps (parked, kept, or woken by a timer) past the hour after its session ended: the prompt prints it as parked with what it serves and what it waits on (`"<name>" parked, no CLI, parked on #12: <waits>; it serves <item>, waiting on <waits>`) instead of "has ended; re-query", and its Agents line says a parked agent's park instead of "its CLI is not running (stopped by a reboot or a crash)". Once the park resolves, the agent reports done or leaves the roster, the record is dropped after the hour as an ended one; a successor no longer misses a parked worker's item and dispatches it again ([#1025](https://github.com/giantswarm/beekeeper/issues/1025)).

## [0.135.1-rc.2] - 2026-10-13

### Fixed

- `beekeeper sessions serve` and `board next --claim` store a registered agent's roster name in the serve record: they stored the session's own name, which for a session the desktop has not titled is its process name (`klaus-lab-34`), so the record, `beekeeper sessions`, the hand-over and every row that prints a record named the process, not the agent working the item. A session that is not on the roster keeps its own name ([#1027](https://github.com/giantswarm/beekeeper/issues/1027)).

## [0.135.1-rc.1] - 2026-10-13

### Fixed

- `beekeeper agents register` from a claude another session's tool started (a `claude -p` in a worker's Bash) no longer renames the worker's roster entry: such a process inherits the worker's `CLAUDE_CODE_*` ids, so beekeeper took it for the worker and replaced the worker's entry under the nested process's `--name`. The register now reads the parent chain from the process table, and a caller whose chain passes two session CLIs is refused with `this runs in a claude started inside session "<worker>" (<id>), whose ids it inherits: its entry stays as it is; a nested claude registers nothing`; the worker's entry is unchanged. Every other command resolves the caller as before, so a guard still treats the nested process as an agent ([#1026](https://github.com/giantswarm/beekeeper/issues/1026)).

## [0.135.0] - 2026-10-13

### Fixed

- The sandbox broker (`beekeeper sandbox broker`) opens the state store, so its log of a brokered secret call that logged itself not (refused, ended on the broker's deadline or by a signal, waited on the locked vault in vain) and the vault keeper's sign-in lines reach the event log: the broker loaded only its configuration, and the first such call dereferenced the nil store and ended the whole broker until its unit restarted it. A log write on an app without a store now fails the call with `the call is not logged: no state store is open; the entry: …` instead of panicking, and a handler's panic answers its own request with `the broker failed on this request: <panic>` (the stack in the broker's journal) while the broker serves the next one ([#1038](https://github.com/giantswarm/beekeeper/issues/1038)).

## [0.135.0-rc.3] - 2026-10-13

### Added

- `units.quiet` (default false) counts every end of a unit beekeeper starts (a merge-child, a wake's turn, a reopen) as success, any exit status and a kill by SIGKILL, SIGTERM or SIGABRT included (`SuccessExitStatus` lists 1–255 and the three signals; systemd takes no range): an isolated harness state sets it, so its stub tools' exits and its deliberate kills no longer fail units in the person's user manager and reach the desktop's failed-unit alerts, the real exit still in the run's `.rc` and `.result` files. Unset, a unit fails on a real fault as before ([#1037](https://github.com/giantswarm/beekeeper/issues/1037)).

## [0.135.0-rc.2] - 2026-10-13

### Added

- `beekeeper lease claim <resource> --park` waits on a held resource without a process: a queued claim says its first line unchanged, parks the calling agent on the lease (`agents park`, on `lease <resource>`) and exits 3; the claim waits in its queue, the grant already stored in the state. Once the resource is free and the agent's grant is first in its queue (or no supervisor is recorded), the watch marks the park resumable and, with `agents.autoResume`, wakes the agent with the words to claim it; a grant to another session first keeps it parked, and a grant to the agent that is gone wakes it to ask again. A refused claim parks nothing. `lease claim --wait` is a foreground wait of at most 9 minutes; a longer one is refused, naming `--park`. A worker that ran `lease claim --wait 4h` in the background lost its claim when the desktop's background limit ended the process. The `beekeeper:lease` skill names `--park` as the form of a longer wait; running workers keep the skills they started with ([#1029](https://github.com/giantswarm/beekeeper/issues/1029)).

## [0.135.0-rc.1] - 2026-10-12

### Added

- The plugin's topic skills `beekeeper:lease` (need, yours, free, the claim's outcomes), `beekeeper:merge` (the gate, exits 76 and 77, hold and release, rebase-merge lines, merging, merged, tagging), `beekeeper:github` (the budget gate, `beekeeper person`, per-repository scans, headless turns, timers, `beekeeper run`, `beekeeper browse`, subagents) and `beekeeper:park` (parking on a person, a merge, a release or a probe timer): a worker loads each before the step that needs it ([#881](https://github.com/giantswarm/beekeeper/issues/881)).
- `beekeeper agents start --dry-run` prints the first prompt and its size after the first turn's command line ([#881](https://github.com/giantswarm/beekeeper/issues/881)).

### Changed

- A started worker's first message carries only the core rules every worker needs (the goal and the report through `agents idle --done`, addressing the supervisor, problems found, never asking the person directly, secrets, git, no polling, nothing left running, the relay at `agents.relayAt`) and names the four topic skills: 2,695 bytes, 975 tokens by the model's own count, from 10,687 bytes and 3,685 tokens (3,599 on v0.120.0). The public-repository and GitHub-tooling paragraphs, desk policy rather than beekeeper process, leave the shipped rules for a desk's own project rules. `agents start` and `agents handover` put a topic skill into the first message themselves when no enabled beekeeper plugin installed serves it as the binary embeds it (none installed, or an out-of-date plugin), and say so with the command that updates the plugin, so no worker loses a rule to a stale plugin. The role skills `supervise`, `guide` and `register-agent` keep an always-loaded core and move the rest, unchanged, into `references/` files they name with the moment to read each ([#881](https://github.com/giantswarm/beekeeper/issues/881)).

## [0.134.0] - 2026-10-12

### Added

- `beekeeper browse --file <path>` (repeatable) shares a file the steps upload through a page's file input with the browse turn: Claude in Chrome's `file_upload` refused every path in the turn, which has no file tool. The command refuses a path that does not exist, is not a regular file, or files over `file_upload`'s 10 MiB before the turn starts, copies each file into a stage root of the turn's own that it names to the CLI as the folder of the files shared with the session (removed after the turn), and the prompt names each file with the copy's path `file_upload` takes; every other path stays refused, the turn's `--dir` included, and the turn keeps no Read tool. A line below the report says `shared for upload: <path>` ([#1004](https://github.com/giantswarm/beekeeper/issues/1004)).

## [0.133.1-rc.3] - 2026-10-12

### Fixed

- `beekeeper sessions` names a parked worker among the session records: a worker that runs no CLI while it stays on the roster parked (`agents park`) or kept (`agents keep`, a timer that wakes it) still serves its issue or pull request, and its record now reads `"<worker>" parked, no CLI, parked on <ref>: <reason>; it serves <owner/repo#n>` after the running sessions' records instead of `ended at …; re-query`. `--json` carries these rows in `parked` (session, issue, waits, parked, by, at), the same ones `board next` skips as served, through one helper both read. The served-check of an issue is the records of `beekeeper sessions`; a worker whose record was released, or that reported its task done, is not listed ([#1001](https://github.com/giantswarm/beekeeper/issues/1001)).

## [0.133.1-rc.2] - 2026-10-12

### Fixed

- A sandboxed session's state directory stays writable when beekeeper's config directory holds it (a scratch config beside its `stateDir`, a `sandbox.allowWrite` under a `sandbox.allowRead`): the sandbox mounts a readable path read-only after the writable ones, so the readable config directory left the state, the spool and the scanner's mount point read-only, and every command failed at bwrap's start with `Can't create file …/scan: Read-only file system`. The policy now lists such a readable path by its entries around the writable path, which it leaves to the write list, and `beekeeper sandbox install` no longer refuses the layout. A sandboxed session starts from a working directory outside any git checkout; a test pins that the policy does not depend on one ([#427](https://github.com/giantswarm/beekeeper/issues/427)).

## [0.130.0] - 2026-10-12

### Changed

- `beekeeper secret github-app <installation> --code <code>` lands a GitHub App created on GitHub's manifest page in the installation's SOPS Secret without a person or a session reading a credential: beekeeper converts the one-time code (or the redirect URL carrying it) as the person's `gh` login, writes `private-key` and `client-secret` (and `webhook-secret` when the App has one) under `stringData` of the Secret `apps.secrets` names for the installation in one sops write, and prints the App's id and client id alone. Every check runs before the code is spent; a spent or expired code is refused in one line ([#979](https://github.com/giantswarm/beekeeper/issues/979)).

## [0.129.0] - 2026-10-12

### Fixed

- The PreToolUse hook reads a file as a script only where the call runs it. A shell's or an interpreter's program is read off its command line in the line's order: the argument of a code option (`python3 -c`, `node -e`, `perl -ne`), a module (`python3 -m`, after which nothing on the line is a script), or the first operand, a script file or `-` for the program on the input; everything after the program is its data, as is the argument of a read-only command (`grep`, `sed`) and a file read into a quoted argument of a command that only carries text (`--report "$(<report.txt)"`, `"$(cat report.txt)"`). A read-only `python3 -I - "$f" <<'EOF'` on a persisted tool result that logged a `devctl pr wait`, a `python3 -I tool.py <file>` (python's `-I` is a flag, which the hook took for an option with a value, so the file after the script was read as the script) and a `beekeeper agents idle --done --report "$(<report.txt)"` whose report named a merge, a `bash merge.sh` and a here-document (the redirection in the substitution read as a command run by its path) were each refused as a merge outside the gate, the file's prose judged as a script: the class of a quoted text taken for a command ([#841](https://github.com/giantswarm/beekeeper/issues/841)), one level down. What the call does run is read as before (`bash merge.sh log.txt`, `python3 -I merge.py`, `cat log.txt | bash`), and a refusal a file causes names the file, the matched token and its line, so a data file the call does run as its program is seen as such ([#964](https://github.com/giantswarm/beekeeper/issues/964)).

## [0.129.0-rc.3] - 2026-10-12

### Fixed

- The PreToolUse hook no longer routes commands that build nothing through `beekeeper run` and the build slot. `agentlab`'s commands are off its heavy list (the lab-count guard and the lab lease govern a lab, which mostly waits on its cluster; no agentlab command builds an image), as are `graphify update` (AST only), `label` and `cluster-only`; `graphify extract` stays. A heavy word counts only at a command position of the shell, read with quotes masked, so a read-only pipeline such as `gh pr checks 1 --repo o/r | grep -E 'lint|make'` or a `python3` here-document that mentions `go test` passes unwrapped, while a build in a `sh -c` or `eval` string or in a here-document a shell reads is still wrapped ([#968](https://github.com/giantswarm/beekeeper/issues/968)).

## [0.129.0-rc.1] - 2026-10-12

### Added

- A `devctl pr merge`, or a `devctl release promote` of one repository, running with no beekeeper gate among its ancestors (the gate runs its command in a merge-child; a merge a script ran past the hook has neither) is reported: `beekeeper watch` says one `UNGATED MERGE` line naming the command, its pid, its session and its parent while it runs and one `ENDED` line when it ends, `beekeeper doctor` lists it as `ungated merge`, and `beekeeper snapshot` marks the wait `ungated`. Such a merge runs outside the lanes, the holds and the budget floor, with devctl's own bypass, and nothing on the machine said so ([#874](https://github.com/giantswarm/beekeeper/issues/874)).

## [0.128.0] - 2026-10-11

### Added

- `beekeeper board block <owner/repo#n> --by <owner/repo#n>` and `board unblock` record and remove a hold on a board item: a GitHub blocked-by dependency on another issue (`addBlockedBy`, `removeBlockedBy`), which `board next` already reads, so the item is skipped while a blocker is open ("1 of 1 blockers open") and offered again at the next `board next` once its blockers closed, with no session acting. Only the two issues are read, each on its own repository, never the board's items; the counts printed are the item's blocker list, which GitHub answers at once where its dependency summary follows seconds later. A blocker that is a pull request or no issue GitHub resolves, an item that is no issue and an item held by itself are refused with exit 3 naming why; a hold on a person's review or answer is the item's Status Blocked / Waiting plus a decision for that person ([#941](https://github.com/giantswarm/beekeeper/issues/941)).

## [0.127.1] - 2026-10-11

### Fixed

- `beekeeper browse` no longer ends a turn silently when the Claude in Chrome connection drops mid-turn (the extension's service worker gone: a call answers "Claude in Chrome is not connected", `list_connected_browsers` lists no browser). The turn checks the connection with `list_connected_browsers` before each step and, when it is gone, reconnects once in the turn (`select_browser` on the browser the list names) and retries the step; a connection the turn did not get back is reconnected once by `browse` itself: a fresh CLI resuming the turn's session, a fresh Chrome connection, under the same timeout, goes on with the steps not done, its report printed after the first. Below the report, `connection lost:` counts the calls that found the connection gone, names the first and says what came of the reconnect; a reconnect that finds no browser ends the command with a non-zero exit, `browser connection lost at <call>`, naming the steps the report lists as not done, to run again once the extension is connected ([#948](https://github.com/giantswarm/beekeeper/issues/948)).

## [0.125.1] - 2026-10-11

### Fixed

- `beekeeper agents park` ends with the complete, copy-pasteable `beekeeper agents resume "<name>"` naming the parked session, where the line said only `agents resume` and the command refused it without its session argument ([#862](https://github.com/giantswarm/beekeeper/issues/862)).

## [0.125.1-rc.2] - 2026-10-11

### Fixed

- While a devctl release window is open, the machine's gates and the watch share one read of the window's state: every waiting gate step (one per queued merge, every 5 s) was asking GitHub for the window's pull request and the local devctl for its version, n reads per step with n queued merges, neither shared nor bounded. A step now claims the read in the state (the hold's `toolChecked`) under its lock and makes it only when the last one is 30 s old, so the pull request is read at most once per 30 s over all gates and the watch; the gate whose own run of the window's merge ended still checks at once, and a window still lifts within the interval of devctl reporting its release ([#859](https://github.com/giantswarm/beekeeper/issues/859)).

## [0.125.1-rc.1] - 2026-10-11

### Changed

- The shipped `worker-rules` skill says never to `git stash` in a worktree: the stash list is the clone's, shared by every worktree of it, so a pop applies another session's entry or drops it; work that has to wait is committed to the worker's own branch or a throwaway branch, or left in place ([#761](https://github.com/giantswarm/beekeeper/issues/761)).

### Fixed

- The gate's plain squash merge, the route of a repository devctl does not serve, refuses a pull request with a reviewer's unanswered feedback as `devctl pr merge` does: a `COMMENTED` or `CHANGES_REQUESTED` review, a review comment or a conversation comment newer than the head by a person other than the author and the gh login, not answered since by them, a new commit or that reviewer's approval, read before the wait and again right before the merge call, is exit 5 with devctl's `unanswered review: …` reason, which the gate's line names. Its document carries `unansweredReviews` on every outcome, an empty list when nothing held the merge, where the route reported none and merged past such a review ([#394](https://github.com/giantswarm/beekeeper/issues/394)).

## [0.125.0] - 2026-10-11

### Fixed

- `beekeeper board move <owner/repo#n> <status>` no longer refuses an issue that has no item on the board or one of a repository outside the board's organisation. An issue whose `projectItems` name no item of the board is added with `addProjectV2ItemById`, the issue's own mutation, which answers the issue's existing item where it has one (GitHub lists an item of another organisation's board under no `projectItems`) or a new one, given `board.team` as its Team; then the Status is set by the item's id, the board's items never read. Every GitHub call of a move acts on the issue's repository (`GH_REPO`), so devctl's agent `gh` link, which picks its login by the owner, reaches a personal account's repository with the person's own login where the App's token resolved neither the repository nor the issue's node. Only an issue GitHub does not resolve is refused, as no issue with GitHub's reason, and `board next --claim` asks whether a served issue of such a repository is open the same way. Workers were adding the item with `gh project item-add` and setting Team and Status with `gh project item-edit` by hand ([#550](https://github.com/giantswarm/beekeeper/issues/550), [#592](https://github.com/giantswarm/beekeeper/issues/592)).

## [0.125.0-rc.3] - 2026-10-11

### Added

- `beekeeper app allow <name> --permission "<scope>: <level>" --word "<the person's words>"` records one change of a declared App's permissions on the person's word (`"Members: Read-only"`), for an App without callback URLs too, one used through the device flow, which has no consent page (`--client-id` on such an App is refused naming the missing callback URLs, no longer "no App name"). With it, `beekeeper hook pretooluse`, in a browse turn too, answers the grant the classifier refused as `[Permission Grant]` itself: the navigation to that App's permissions page under the organisation's settings, the calls there whose own words name the permission and no other level and Save changes, and the installation's Review request and Accept new permissions naming the App are allowed with the record in the reason; the permissions page of any other App and any other call on the recorded one are refused naming the record's form; a batch is allowed only when each of its calls is. A browse turn whose steps name the App runs with that change's auto mode rule and the prompt's words to set exactly that permission, and its report says `allowed permission: <name> (<change>, declared in <manifest>)`; a `[Permission Grant]` refusal names both record forms. `app check <url> --words <words>` and `--navigate` print the decision for a call in its words and for a navigation ([#917](https://github.com/giantswarm/beekeeper/issues/917)).

## [0.125.0-rc.1] - 2026-10-11

### Added

- `beekeeper app create <name>` creates a GitHub App from the manifest the organisation declares (`apps.repo` at `apps.manifest`, read from the default branch) through GitHub's manifest flow, modelled on dex-operator's: the manifest, with this run's redirect URL and a `state` nonce, goes to GitHub from a page on a loopback listener in the person's browser, the person clicks Create, GitHub redirects with a one-time code, beekeeper checks the `state`, converts the code in its own process and writes the answer into the shared vault item `<name>` in one `op` call (`app-id`, `client-id` and `slug` as configuration, `client-secret`, `private-key` and `webhook-secret` concealed), then sends the browser to the App's installation page with the organisation and the repositories of `installation.json` next to the manifest preselected (`{"repositories": "all" | ["<name>", …]}`; absent: all), waits for the person's Install click and records the installation's id as `installation-id`. It names each click and waits for it, in an agent session too (the browser lease held), and answers and logs (`app.create`) ids, the slug, lengths and keyed fingerprints only; an App no manifest declares under its name, one GitHub knows already, a redirect whose `state` is not this run's and a Create click that does not come in time are refused in one line with nothing written. The vault keeper's socket takes a `store-item` request, several fields into one item in one call, plain or concealed ([#914](https://github.com/giantswarm/beekeeper/issues/914)).

## [0.124.0] - 2026-10-10

### Fixed

- A `devctl pr merge` (or `release promote`, `pr wait`, `release wait`, `rollout wait`) the call runs from a script file no longer bypasses the merge gate: the PreToolUse hook reads the script a shell runs (`bash merge.sh`), one run by its path (`./merge.sh`), a sourced one and one an interpreter runs (`python3 merge.py`), relative to the call's working directory and into the scripts it runs in turn, and refuses the call naming the script and the line, with the line's gated form where the gate has a place; a script the call writes before it runs it is judged by the call's own text. The same refusal meets a merge in an interpreter's code (`python3 -c`, `node -e`, `perl -e`, …), in the program a shell or an interpreter reads from its input (`echo '…' | bash`, `bash <<< '…'`, `sh < merge.sh`, `python3 - <<'EOF'`) and in an `eval` string, as it did one in a `-c` string. A merge in a here-document a shell reads (`bash <<'EOF'`, `cat <<'EOF' | sh`) and one on an indented line are gated in place, and a here-document that is content (a file written, an issue body sent) is left as written, the gate no longer put into it ([#874](https://github.com/giantswarm/beekeeper/issues/874)).

## [0.124.0-rc.1] - 2026-10-10

### Changed

- `beekeeper app allow` records only an App the organisation declares: it reads the App's manifest from the repository the config's `apps.repo` names, at `apps.manifest` (`{name}` for the App's name), on its default branch, refuses an App no manifest declares under its name, and takes the record's callback hosts from the manifest's `callback_urls` (`--callback` is gone); the record, `app list`, the hook's allow and the browse report name the manifest and the commit it was read at ([#655](https://github.com/giantswarm/beekeeper/issues/655)).

## [0.123.0-rc.1] - 2026-10-10

### Added

- Worker profiles: `agents.profile` (and `supervisor.profile`, `guide.profile`) names a tool profile of `agents.profiles`, or the built-in `minimal`, that the sessions beekeeper starts get: the built-in tools kept (`--tools`), the plugins kept (every other installed plugin, of any marketplace, disabled), the MCP servers kept (`allowedMcpServers`, claude.ai connectors included) and further deny rules, in a flag settings file beekeeper passes with `--settings`, never editing the person's settings files. A profiled worker's task runs as a headless turn under the profile with no seed turn, its desktop import beside it; every headless wake of one of beekeeper's starts runs under its role's profile. `start` and the log say `its task runs as a headless turn under profile <name>`, and `agents start --dry-run` prints the profile applied and the first turn's command line. Measured on a start: the first task turn's input drops from 86,471 to 30,560 tokens, the cached shared prefix from 40,839 to 7,708, and the seed turn's ~16,000-token cache write is gone ([#882](https://github.com/giantswarm/beekeeper/issues/882)).

## [0.122.0] - 2026-10-10

### Changed

- `beekeeper capacity`'s swap guard weighs disk swap's growth with memory pressure and MemAvailable instead of refusing on the growth while MemAvailable falls: a growth over `capacity.swapGrowthMaxMiB` blocks a start under pressure (memory PSI, full avg60, over the new `capacity.swapPSIMax`, default 0, or MemAvailable under `capacity.availMinMiB`; either unknown counts as pressure) and is a warning in parentheses after the verdict without, the line naming the three figures (`room for 7 starts (disk swap growing +391 MiB/h over 256, without pressure: memory PSI 0% (max 0%), MemAvailable 43.0 GiB (floor 20 GiB))`); the headroom line and `--json` carry the pressure. The rate is measured over the new `watch.swapWindow` (default 10m, at least 5m) instead of a fixed hour, for the watch's SWAP and OOMD IMMINENT checks too, so a single zswap writeback burst, which moves pages to disk without new swapping, leaves the figure within minutes rather than holding it for an hour ([#858](https://github.com/giantswarm/beekeeper/issues/858)).

## [0.122.0-rc.1] - 2026-10-10

### Changed

- The build slot bounds only a start that builds. `beekeeper capacity --builds` and `beekeeper agents start --builds` mark a task that builds, tests or lints (its commands take the slot under `beekeeper run`), and such a start is refused while no slot is free (`no start: no free build slot`). A start without the mark does not build (a filer, a dispatcher, a reviewer, a promoter proving a release read-only) and is judged on MemAvailable, disk swap's growth, the 1-minute load against the watch's HIGH LOAD limit (new in the headroom) and the kind labs alone: a busy slot is a note in parentheses after its verdict, never a `no start`, so the machine's one slot held by a worker's compile refuses no start that only reads; the watch's `CAPACITY LOW` weighs such a start. `agents start` judges its start the same way before anything is recorded and refuses one whose verdict has no room (exit 3, the guards named, no session created); `--force` starts it anyway and records the verdict overridden in the event log ([#878](https://github.com/giantswarm/beekeeper/issues/878), [#844](https://github.com/giantswarm/beekeeper/issues/844)).

## [0.120.0] - 2026-10-10

### Added

- `beekeeper app allow <name> --client-id <id> --callback <host> --word "<the person's words>"` records an App the person owns and asked a consent for, on their word (`app list`, `app revoke`, `app.allow` and `app.revoke` in the log). With it, `beekeeper hook pretooluse` answers a Chrome call that acts on GitHub's consent page itself: on the recorded App's page with the recorded callback host (the client id, or the App's name in GitHub's title where the Chrome tools redact the id) the click is allowed, the record in its reason; on GitHub's consent page of any other App or callback host it is refused, naming the page and the record's form; a fixture user's sign-in or consent on the lab's own identity provider on a loopback host (`*.127.0.0.1.nip.io`, `localhost`) is allowed without a record. The page is the tab's last known URL and title in the session's transcript; a call whose page it does not show gets no decision, as before. `beekeeper browse` records its turn as a start of its own (mode `dontAsk`, gone when the turn ends), so the hook acts in it wherever it runs; steps that name an App on record run with that App's auto mode rule after the shipped rules and the prompt's words to click its page's Authorize once, steps that name a loopback host with the lab's rule and the prompt's exception for the fixture credential, the report saying `allowed consent:` and `allowed lab sign-in:` first, a `[Permission Grant]` refusal naming the record. `beekeeper app check <url> [--title <title>]` prints the hook's decision for a page, dry; `beekeeper log --verb browse` lists the turns ([#864](https://github.com/giantswarm/beekeeper/issues/864)).

## [0.119.0] - 2026-10-10

### Added

- `beekeeper secret copy|set … -- <consumer>` names two more consumers by what they are: `gitlab-rails runner [-e <env>] '<code>'` whose Ruby code reads `STDIN`, on the host or in a pod through `kubectl exec -i` (a GitLab instance's token or OAuth application secret, set without a pretend `--password-stdin`), and `beekeeper secret copy <ref> --git-askpass -- git [-C <dir>] clone|fetch|push|ls-remote …`, which runs git over HTTPS with `GIT_ASKPASS` naming a helper beekeeper writes for the one call: the helper asks beekeeper on a socket only git's own processes reach and answers `--git-user` (default `oauth2`) and the value, which is on no argv, in no file, in no environment and not in the redacted answer. git runs without the global and system configuration and without hooks; a repository naming a credential helper and the options that set configuration or name a program are refused ([#857](https://github.com/giantswarm/beekeeper/issues/857)).

## [0.119.0-rc.1] - 2026-10-10

### Added

- `beekeeper secret capture <file#path> [--jq <filter>] [--encode …] [--vault op://…] -- <producer…>` stores a credential its issuer generates and prints once (a registry token's password, an API token, a cloud access key): the producer runs in beekeeper's process with the caller's environment minus the vault's credentials, `SOPS_AGE_KEY` and the variables the new `secret.env` globs name, its stderr passed through; the value is its stdout, or the one string a jq filter picks from its JSON, and goes into the SOPS path under the nearest `.sops.yaml`'s creation rules (and the shared vault's field first with `--vault`). Every check runs before the producer does; a non-zero exit, empty stdout or a filter that picks no single string is refused in one line, nothing stored. `--dockerconfigjson <registry> --username <user>` writes a `kubernetes.io/dockerconfigjson` Secret in one step. It answers the length and the keyed fingerprint, never the producer's output, and `beekeeper log` records the producer's command and the references ([#856](https://github.com/giantswarm/beekeeper/issues/856)).

## [0.118.1-rc.1] - 2026-10-10

### Added

- `merge.sessionCap` (default 2): one session runs at most that many gated merges and promotions at once. Each running devctl polls GitHub, so ten merges one session queued (a dependency sweep across repositories, each in a lane of its own) poll as two would: the others wait in their lanes without reading GitHub (no pull request read, no budget probe) and start in the order they queued, an earlier merge that cannot start (behind another session's merge, its lane settling) not holding a free slot. The gate's line names the session's running merges; `beekeeper budget` prints each session's gated merges, those polling GitHub and those queued without reading it (`gated` in `--json`). `-1`: no cap ([#850](https://github.com/giantswarm/beekeeper/issues/850)).

## [0.118.0] - 2026-10-10

### Added

- `beekeeper secret store`, the person's command: a value a web page showed once, or a file it downloaded, goes into a field of the shared vault, from the Wayland clipboard (`wl-paste`, cleared afterwards) or from `--from-file <path>` (shredded afterwards unless `--keep-file`), the item created when absent. It prints the length and the keyed fingerprint, which `beekeeper log` records with the reference; the value travels in beekeeper's memory alone, with `secret.session` to the broker over the keeper's socket the way the session itself does, and reaches no command line, spool or state file. `--github-app <item>` stores a GitHub App's client secret (the clipboard) and private key (the newest `*.private-key.pem` in `~/Downloads`, or `--from-file`) into `client-secret` and `private-key` in one call, naming the two clicks and waiting for Enter while a value is still missing. It stops with exit 78 while the broker holds no session, before any click, refuses in an agent session and without a terminal, and the hook refuses it in agent sessions, naming the way ([#852](https://github.com/giantswarm/beekeeper/issues/852)).

## [0.117.0] - 2026-10-10

### Added

- `beekeeper browse --allow-deploy "<what, where>"` declares the one deploy or create click a browser proof's task covers (a portal's Deploy button on a review step, which the headless turn's classifier refuses as a production deploy): the turn runs with an auto mode allow rule for exactly that action after the shipped rules (`--settings`, this run only; the desktop turns' rules are untouched), its prompt says to click that control once and nothing else that deploys, and the first line below the turn's report says `allowed deploy: <what, where>`. Without the flag the refusal stays: the `refused:` line names the click with the classifier's reason, and a line names the flag. The hook's refusal of the Chrome tools in a desktop turn names the flag too ([#842](https://github.com/giantswarm/beekeeper/issues/842)).

## [0.116.2] - 2026-10-10

### Fixed

- `secret copy|compare|fingerprint k8s://…#<path>` reads key-value lines beside a bare line: a `head: value` or `head = value` line resolves by its head whatever the other lines hold, and a bare key-like line right before key-value lines is a section for them, addressed as `#<section>.<head>` (to the next bare line, or while they stay indented under it). The refusal names a section's lines as `<section>.<head>`; a bare line that names no section stays counted by its length ([#827](https://github.com/giantswarm/beekeeper/issues/827)).

## [0.116.1] - 2026-10-10

### Fixed

- `secret copy|compare|fingerprint k8s://…#<path>` reads a key an operator wrote by hand that is neither a YAML document nor dotenv: tab-indented YAML, `key = value` and `key: value` lines with any spacing around the separator, CRLF line ends, quoted values, or a mix. A refusal for text no reader takes names each line's head, the text before its first `:` or `=` (trimmed, `export` stripped), never anything after it; a line without either, or with a head no key carries, is counted by its length ([#827](https://github.com/giantswarm/beekeeper/issues/827)).

## [0.116.0] - 2026-10-10

### Fixed

- A message to `the supervisor` or `the guide` reaches the holder beekeeper records although another live session carries the holder's title: the holder's CLI is found by its session id and addressed by its own socket while its title is shared. `beekeeper doctor` and the watch (`SAME TITLE`) name every title two live sessions carry, and a relay or a role's `start` refuses a run name another live session carries, naming that session ([#758](https://github.com/giantswarm/beekeeper/issues/758)).

## [0.116.0-rc.1] - 2026-10-10

### Changed

- `#<path>` on a `k8s://` source reaches into every shape a Secret's key commonly holds: a dotted key of its YAML or JSON mapping as before, a line of its dotenv lines (`KEY=value`, an `export` in front and matching quotes stripped, `#` comments and blank lines ignored), and, one level down, a value of the YAML mapping or dotenv lines a block scalar holds (`#config.yaml.oidc.clientID`; a key holding dots takes as many path segments as it needs). A path that reaches nothing fails in one line with the shape of what the key, or the node on the way, holds and nothing of its value: the format (a YAML or JSON mapping with its top-level keys, dotenv lines with their keys, a list of n items, a scalar, text, binary, nothing), its line count and byte length; a key named like a generated value is counted, not named. The caller picks the right path from the refusal instead of reading the value ([#799](https://github.com/giantswarm/beekeeper/issues/799)).

## [0.115.0-rc.3] - 2026-10-09

### Added

- A key of a Kubernetes Secret as a source of `beekeeper secret`: `k8s://<context>/<namespace>/<name>/<key>[#<path>]` reads one key, or one value of the YAML or JSON document the key holds, in beekeeper's process and serves it to every `copy` destination (a SOPS path, `<ref>=<path>…`, a consumer, `--to-secret`), to `compare` and to `fingerprint`, so a credential its owner placed in a cluster Secret reaches a SOPS path without a person and without its value reaching a session. A lab's context (`kind-<cluster>`) reads through kind under the caller's lab lease, any other through the new `kube.kubeconfig` list (default `KUBECONFIG`'s files, else `~/.kube/config`), one Secret GET and nothing written; a Secret, key or path absent fails in one line naming it, no content quoted, and the answer is the value's length. The hook's refusal of a `kubectl get` of a Secret names the form ([#773](https://github.com/giantswarm/beekeeper/issues/773)).

## [0.115.0-rc.1] - 2026-10-09

### Changed

- `secret reveal` answers a client id however random it looks: a leaf named `id`, `clientID` or `client_id` and an item of a `trustedPeers` or `peers` list are exempt from the entropy rule, since every authorize URL carries them; a key named like a secret on the path, the value scanner and a URL password still refuse, and a client id equal to a value under a secret-named key of the same file is refused ([#774](https://github.com/giantswarm/beekeeper/issues/774)).

## [0.114.0-rc.1] - 2026-10-09

### Added

- `secret.ageVaults`: the vaults besides `secret.vault` that age identities are read from. A SOPS file under `installations/<name>/` that no `secret.ageIdentities` entry names finds its identity in the item `<name>.agekey` of `secret.vault` or one of them, a field or the document holding the identity file, and `secret recipients` names that item; an `op://` entry of `secret.ageIdentities` may name a field of an age vault, and one naming any other vault fails the configuration's load instead of being listed as found ([#766](https://github.com/giantswarm/beekeeper/issues/766)).

## [0.113.1-rc.3] - 2026-10-09

### Fixed

- `beekeeper install`'s standby unit names in its header comment the same watch as its `ExecStart`, `watch --standby` while `watch.notify` is false. A unit path that is a symlink (into the person's dotfiles) stays the link: install writes its target, a dangling one too, and says so (`update <unit>: through the link to <target>`), and restarts an updated unit with `systemctl --user restart` instead of `disable --now` and `enable --now`, since systemd's disable removes a unit file linked from outside the unit directory ([#744](https://github.com/giantswarm/beekeeper/issues/744)).

## [0.113.1-rc.2] - 2026-10-09

### Changed

- The merge gate names the command whose turn came: `promoting <owner/repo>` for `devctl release promote`, `merging <owner/repo>#<n>` for `devctl pr merge` (unchanged). A promotion devctl refuses (`not_built`, `nothing_to_promote`) logs no `merging` and its outcome line, printed after devctl returns, says `nothing promoted (<state>)`; a dispatched one says `promoted <owner/repo>: release <version> dispatched` ([#593](https://github.com/giantswarm/beekeeper/issues/593)).

## [0.113.0] - 2026-10-09

### Changed

- A rollout is read from a HelmRelease's `status.history`: `timer add --when "helmrelease-ready <context>/<namespace>/<name> <version>"` holds once the release is Ready and runs that chart version (the newest release of the history, or without one the attempted revision while Ready), and the merge gate's roll check takes the attempted revision of a Ready HelmRelease without a history. Current Flux leaves the applied revision empty, so a timer probing it never fired although the release had rolled. A conditional timer whose condition has not held for 24 h is one `TIMER STUCK` watch line (event `timer.stuck`), and `timer list -v` says since when ([#737](https://github.com/giantswarm/beekeeper/issues/737)).

## [0.113.0-rc.1] - 2026-10-09

### Added

- `beekeeper spend --since <time> [--until <time>]` prices every API request of the window from the whole transcripts of every session, running, paused or archived, and their subagents, each request once by its message id, with a line per model and the unpriced models named. `sessions` prices only the last 512 KiB of the live transcripts, a twentieth of a day's spend ([#713](https://github.com/giantswarm/beekeeper/issues/713)).
- `metrics.models` prices `claude-sonnet-5-5` by default ($2 in, $10 out, $0.20 cache read): its requests were cost unknown.

## [0.112.2] - 2026-10-09

### Changed

- `beekeeper install` writes the standby unit as `beekeeper watch --standby`, and with `--notify` only when the new config key `watch.notify` is true (default false): a desk without desktop notifications runs install as it is. Re-running install is idempotent and safe after every update: besides what an earlier install recorded, it updates in place the hook entries that run this binary's hook command (an older matcher, settings a self-update left) and a unit that runs this binary, never adds an entry twice and touches no other setting, so a changed hook matcher (`Read|Grep`, `TaskStop`) reaches `~/.claude/settings.json`; a second run changes nothing. `install --dry-run` prints every step ([#622](https://github.com/giantswarm/beekeeper/issues/622)).

## [0.112.2-rc.1] - 2026-10-09

### Fixed

- The watch no longer says PagerDuty unreachable on a transient: a failed `list_incidents` is tried once more after a short pause, and a PagerDuty that read and then does not answer is said unseen only once it has been 3 minutes unseen. muster answered `auth_required` for a reading or two and read again within two minutes with no sign-in between; a persistent failure is still said, the same line ([#729](https://github.com/giantswarm/beekeeper/issues/729)).

## [0.112.1] - 2026-10-09

### Fixed

- A gate run that ends abnormally leaves nothing behind. A merge call GitHub answers with a 5xx (devctl's exit 7 naming `…/pulls/<n>/merge: 502`) is sent again while GitHub reports the pull request open, up to three times 10 s, 30 s and 1 m apart, the caller reading one document. A run with nothing merged leaves its lane instead of keeping a "retrying" place, and a waiting place an older release kept goes at the next gate step. The watch records a run whose gate is gone once its devctl wrote its exit code, even while merge-child still hands the outcome over, so a killed gate frees its lane within one tick. A `TaskStop` of a gate's background task ends its devctl and merge-child (or drops its waiting place) through the PreToolUse hook, whose matcher gains `TaskStop`; `beekeeper install` updates it. A 502 on the merge PUT had held a lane 15 minutes, a killed gate 19, and a stopped gate left devctl merging on ([#712](https://github.com/giantswarm/beekeeper/issues/712)).

## [0.112.1-rc.2] - 2026-10-09

### Changed

- The watch keeps one Alertmanager port-forward per installation from tick to tick and asks kubectl for the kubeconfig's contexts only once it changed: a tick is HTTP requests, no `kubectl` or `tsh kube credentials` run (kubectl keeps the Teleport credential until it expires). A forward that dropped is replaced by one new one at the next attempt, counted as today. An installation whose context is not in the kubeconfig is said at once (`context "<ctx>" is not in the kubeconfig`), never retried: 23 such installations ran 670 kubectl a tick. A PROCESS STORM line names the watch's own processes as `"beekeeper watch"`, and the snapshot lists the kept forwards as no session's wait. One tick over 28 installations was the machine's fork storm ([#710](https://github.com/giantswarm/beekeeper/issues/710)).

## [0.112.1-rc.1] - 2026-10-09

### Fixed

- `beekeeper secret import` (and `set --vault`) creates a new vault item op's ItemValidator accepts: a Password item whose `password` field is the built-in one with its purpose, and a built-in destination field (`notesPlain`, `username`) keeps its own purpose and type instead of becoming a bare concealed field. A failed `op`, `sops` or `kind` call prints the tool's whole message on one line, patterns redacted and the value it was given replaced, instead of its first 160 characters, which had cut op's validator reason off ([#727](https://github.com/giantswarm/beekeeper/issues/727)).

## [0.111.0] - 2026-10-09

### Added

- `beekeeper secret copy` and `secret set` take `--encode base64` or `--encode basic:<user>`: the value is written in that form, made in beekeeper's process before the write (`base64("<user>:<value>")` for basic, the credential a gateway injects verbatim after `Basic `), never printed, logged or returned. `secret fingerprint <ref> --encode …` answers the encoded form's fingerprint and `secret fingerprint --secret <context>/<namespace>/<name>/<key>` a held lab Secret's key, so a delivery is checked without a value read ([#720](https://github.com/giantswarm/beekeeper/issues/720)).

## [0.111.0-rc.1] - 2026-10-08

### Added

- PagerDuty is the watch's second alert source: with `alerts.pagerduty.context` (a muster context) and `alerts.pagerduty.services` (the team's service ids) set, `beekeeper watch` reads the services' open incidents every `alerts.pagerduty.every` (1m) through muster's read-only PagerDuty tools as the person, and prints `PAGERDUTY NEW <installation> <TEAM> #<number> <title> since <start>`, `ACKNOWLEDGED` and `RESOLVED` lines, the installation from the incident's alert. A PagerDuty that does not answer is one unreachable line, said again every 15 minutes, its open incidents kept; `alerts watch` reads it once, `snapshot` lists the open incidents. beekeeper holds no PagerDuty token. A page for the team on an installation the watch did not read reached PagerDuty and the person, never the watch ([#697](https://github.com/giantswarm/beekeeper/issues/697)).

## [0.110.1-rc.2] - 2026-10-08

### Fixed

- An install (`beekeeper self-update`) while a gated merge runs no longer loses its outcome or leaves its lane running: a gate call whose binary was replaced re-executes the installed one at its next step in every phase, following its devctl on from where the stderr was copied (`continuing under beekeeper <version>: following <merge>'s devctl (pid <n>) on`, the same pid, arguments and stdio) so the installed release records the outcome; a step whose save the newer release refused re-executes instead of refusing (77) while the binary is replaced; and a `merge-child` whose command ended on a replaced binary re-executes the installed one carrying the outcome before it wakes the owner. A gate whose save is refused all the same (the re-exec did not start) leaves the run's document and exit code for the watch, which records the real outcome from them at its next poll instead of asking GitHub, so no running lane entry stays for a finished merge. `self-update` and `doctor` mark a gate call or `merge-child` among the processes left on the old binary as re-executing by itself ([#706](https://github.com/giantswarm/beekeeper/issues/706)).

## [0.110.1-rc.1] - 2026-10-08

### Fixed

- `beekeeper lease claim` says the outcome in its first line: `held by you since <time>: …` (exit 0), `queued: number <n> behind <holder or grantee> …` while a grant to the caller waits behind a holder or an earlier grant (exit 3), or `refused: <reason>` (exit 3). Only a held claim of a kind lab prints its kubeconfig's export line, after the held line; a queued or refused claim prints one line, naming how to wait (`beekeeper lease status <resource>` in a probe timer, or `--wait`), and nothing to stderr. `lease claim --wait <duration>` claims again every 10 seconds until the claim is held or the duration has passed, then says the last outcome with how long it waited. A worker whose grant was second in the queue read the export line as its lease and worked a lab another session held ([#699](https://github.com/giantswarm/beekeeper/issues/699)).
- The worker-rules skill says to read the claim's first line, gate the work on its exit code (`beekeeper lease claim … && <work>`) and wait for a queued claim with `--wait` or a probe timer.

## [0.110.0] - 2026-10-08

### Fixed

- A gated merge queued for the GitHub budget's reset exits 76, like one queued in its lane, with the same `queued, … its outcome wakes …` line; 77 is left to a refusal, which queues nothing and says why. A merge queued under `github.floor` exited 77 while its line said it was queued, which a worker reads as a refusal to act on, and the later merge came as a surprise ([#679](https://github.com/giantswarm/beekeeper/issues/679)).

## [0.110.0-rc.1] - 2026-10-08

### Added

- `beekeeper lanes urgent <owner/repo> <n> --reason <why>` marks a merge urgent, the supervisor's go for a privacy or security fix whose delay keeps something exposed: its gate runs it under the GitHub budget floor instead of queueing it for the reset, while the budget keeps the new `github.urgentBound` (200) for it. The gate's log (`merge.urgent`, and `merge.urgent.spent` with what its run drew against that bound) and `beekeeper budget` name the merge and who asked. One urgent merge runs under the floor per reset window: a second mark, or a mark whose gate finds the window's run taken, is refused with the reason and that merge waits for the reset ([#678](https://github.com/giantswarm/beekeeper/issues/678)).

### Fixed

- The shipped `worker-rules` and `register-agent` skills say to ask the supervisor for a resource (`need <env>`) and claim it only after its `yours <env>`, and that a command needing the lease runs only after `lease claim` exited 0 (`lease claim … && <command>`, never `;`): a refused claim is no lease ([#676](https://github.com/giantswarm/beekeeper/issues/676)).

## [0.109.0] - 2026-10-08

### Fixed

- `watch --once` is silent only when nothing changed: a read that fails (the state, the process table, the vault's state, the GitHub budget, the kernel journal, an installation's upgrades, the notes' or parked agents' issues and pull requests) is a line and makes it exit 1. The notes' read is said on the NOTE path: `NOTE REFS UNREADABLE` when GitHub fails it, and under `--once` `NOTE REFS NOT READ` while the budget is under its floor and `NOTE REFS UNANSWERED` for an issue or pull request GitHub did not answer, which keeps its notes open. A failed notes read used to be a line a NOTE filter dropped, and a skipped one no line at all ([#675](https://github.com/giantswarm/beekeeper/issues/675)).

## [0.109.0-rc.10] - 2026-10-08

### Fixed

- The watch holds the hand-over of a worker in its task's last step instead of saying `HANDOVER DUE` the moment its context crosses `agents.relayAt`: a worker whose `sessions serve --waits` names the report or says the merge landed (`report`, `merged`, `release`, `rolled out`, `proof`), or whose merge the gate saw land (settling, its release and rollout pending), is `HANDOVER HELD "<agent>" at <n>k: last step, report expected (<the evidence>); due at <HH:MM> or at <ceiling>k`, once, for `agents.lastStepGrace` (30m) from that evidence or until its context reaches `agents.lastStepCeiling` (default a quarter above `relayAt`); past either, `HANDOVER DUE` follows and says how long it was held, and a worker that reported done (`agents idle --done`) is never handed over. A worker handed over at its report started a fresh session that found nothing to do while the original's `WORKER REPORT DONE` arrived seconds later ([#671](https://github.com/giantswarm/beekeeper/issues/671)).

## [0.109.0-rc.9] - 2026-10-08

### Fixed

- The merge gate gives `devctl pr merge` the new `merge.ciTimeout` (1h) as its `--timeout` unless the command names its own. devctl's default of 30m ended merges with exit 2 and nothing merged while the CI that `--update-branch` restarted from zero still ran (about 30 minutes on a fork line); the requeued merge then passed ([#672](https://github.com/giantswarm/beekeeper/issues/672)).

## [0.109.0-rc.8] - 2026-10-08

### Added

- `beekeeper secret` decrypts a SOPS file for a recipient no `secret.ageIdentities` entry names with the shared vault's item per recipient: the item titled `sops age key <recipient>`, whose `password` field holds the `AGE-SECRET-KEY-1…` identity, which a person creates once for an installation's recipient. The broker resolves the recipients from the file's metadata and, for a file still to be written, from the creation rule of the gitops repository's `.sops.yaml` for its path (`path_regex`), finds the item in the vault's item listing (metadata, no value) and reads it for the one sops call, so `copy` and `set` write a test installation's Secret without a person's sops run. A recipient with neither an entry nor an item fails before sops in one line naming the installation (`alerts.installations`, by the directory of the file's path), the recipient and the item the vault lacks, never a note asking a person to decrypt. `secret recipients [<directory> | <sops-file>]` lists each creation rule's (or the file's) recipients with where its identity is, reading no value, exit 1 when one has none; with `secret.session` it runs in the broker like a call on the vault. A test installation's recipient had no identity anywhere, so no agent commit on it could carry a secret ([#664](https://github.com/giantswarm/beekeeper/issues/664)).

## [0.109.0-rc.7] - 2026-10-08

### Added

- `note list --for <person>` lists the open notes for one person or team ([#668](https://github.com/giantswarm/beekeeper/issues/668)).

### Fixed

- The note overtake no longer closes a person's open decision when a pull request's closing keyword closes the issue the note asks about (`Closes #n` at the merge) or when any close comes while the worker that filed the note still runs its task: the watch reads the issue's latest close event in the same GraphQL request as its state, keeps the note open and says once per reason `NOTE KEPT: #<id>, <ref> closed by <owner/repo#pr>'s closing keyword, not by its worker: <text>` or `…, its worker "<name>" still runs: …` (`note.kept`, `watch --once` names it and writes nothing). `agents idle` stamps the worker's open notes with its task's end (`taskEnded`); from then on their settled issues overtake them, a keyword close included, as a close by hand of a note no worker runs for does. Two decisions vanished from the guide's list at a worker's merge while its task still ran ([#668](https://github.com/giantswarm/beekeeper/issues/668)).

## [0.109.0-rc.3] - 2026-10-08

### Added

- `agents start --harness omp` hands an agent its provider's key from the vault: `omp.providers.<provider>.apiKey` names the key's `op://` reference (a value there is refused when the configuration loads), omp's models file (`omp.modelsFile`, `~/.omp/agent/models.yml`) names only the variable omp reads the key from (`apiKey: SPARK_API_KEY`, which omp resolves as a variable's name first), and the start reads the reference through `beekeeper secret`'s own handling where the vault session is (the host's broker with `secret.session`, as a sandboxed start runs) and writes `NAME=value` into the agent's inbox before omp runs: the unit's shell exports it and execs omp, so no file, command line or unit property carries the value and it lives in omp's environment alone. omp hands that environment to the agent's tool shells, so the unit names beekeeper's own bash as `SHELL` (`<stateDir>/omp/bin/bash`), which drops the variables `BEEKEEPER_OMP_CREDENTIALS` lists first. A start on a provider whose key the file carries while a reference names it, whose variable no reference fills, or whose reference does not answer is refused before anything is recorded; the record and the output say which variable came from which reference, never the value. The machine's omp configuration held a provider's key as a plain value, which any session could read ([#607](https://github.com/giantswarm/beekeeper/issues/607)).

## [0.109.0-rc.2] - 2026-10-08

### Fixed

- `timer add --when "controlplane-ready <context>/<namespace>/<name>"` fires once the KubeadmControlPlane has rolled out under Cluster API v1beta2 too: the check reads the object and judges it in beekeeper, its `spec.replicas` against `status.replicas`, `readyReplicas` and `upToDateReplicas` (v1beta2) or `updatedReplicas` (v1beta1), or the v1beta1 `Ready` condition, once `status.observedGeneration` has caught up with the spec. v1beta2 has no `updatedReplicas`, so the timer never fired. `helmrelease-ready` waits for the observed generation too, and every typed condition tells a reference it cannot read (an unknown context, no access, no such kind or object) from one that does not hold yet: the watch says it once as `TIMER UNREADABLE` (event `timer.unreadable`), again when the reason changes, and the timer keeps waiting. A timer keeps what its last check found; `timer list -v` shows it, the new `timer check <id>` runs the check now, and a timed-out or expired timer's line names it ([#594](https://github.com/giantswarm/beekeeper/issues/594)).

## [0.109.0-rc.1] - 2026-10-08

### Added

- The PreToolUse hook refuses a `kubectl`, `helm` or `flux` command that reaches a cluster through the machine kubeconfig without an explicit context, or with one that may expand to nothing (`--context ""`, an unset `"$CTX"`, a command substitution), and names `--context <name>` and `"${CTX:?no context}"` instead. A literal context, `${CTX:?}`, a variable the command sets to a non-empty word, and a kubeconfig of the command's own pass; commands that reach no cluster (`kubectl config`, `version --client`, `--dry-run=client`, `helm template`, `flux build`, …) pass. An empty `--context` fell back to the kubeconfig's current context without an error ([#598](https://github.com/giantswarm/beekeeper/issues/598)).

## [0.108.3] - 2026-10-07

### Fixed

- `secret copy --to-secret` (and `set --to-secret`) writes its key as a merge patch of that one key, creating the Secret when absent, so a second copy into another key of the same Secret keeps the first and every other key, label and annotation. The write was a server-side apply under the one field manager `beekeeper-secret` declaring only the new key, so the second apply removed the key the first had written and the Secret's consumer failed on the missing key ([#648](https://github.com/giantswarm/beekeeper/issues/648)).

## [0.108.2] - 2026-10-07

### Fixed

- The gate tests' store saves as a dev build's, whichever version the test binary is linked with. `make test` on a stable tag links the release's version, so the tests' first save stamped the state as that release's and `TestAnOlderReleasesGateIsRefusedBeforeTheLane`'s own, older stores were refused: every stable tag's CircleCI `go-build` failed and v0.108.0 and v0.108.1 were published without assets, image or chart ([#635](https://github.com/giantswarm/beekeeper/issues/635)).

## [0.108.1] - 2026-10-07

### Fixed

- A tool the `secret` operations run (`kind` for `copy --to-secret` and `lease kubeconfig`, `op`, `sops`, `age`) is found on PATH or in the Go tool directories: beside the running beekeeper binary, `$GOBIN`, every `$GOPATH/bin`, `~/go/bin` and `~/.go/bin`; one found nowhere is refused naming them. The sandbox broker (`beekeeper-sandbox.service`) runs its calls with the service manager's PATH, the distribution's default without the directory `go install` writes to, so a sandboxed `secret copy --to-secret` into a lab answered `kind: executable file not found in $PATH` with the vault unlocked, and no credential reached a lab's Secret ([#629](https://github.com/giantswarm/beekeeper/issues/629)).

## [0.108.1-rc.1] - 2026-10-07

### Fixed

- `beekeeper board next` without `--claim` is the preview of the board's work: it lists every free item behind the pick in its order and counts the skipped items by kind (`served`, `note`, `assigned`, `blocked`, `stale`, `lease`, `order`; `--json` carries the counts as `skipped_by`), so a scan sees at once what is free and whether the roster, the notes or the order's rules hold the rest. The text output named the pick and a count, and a scan had to read `--json`'s `after_pick` for the free list. The issues the served pull requests close are read in requests of 50 refs, about the refs that are no board item only: one request over every ref the notes name outgrew an argument's size limit ([#597](https://github.com/giantswarm/beekeeper/issues/597)).
- `board next` reads the issues a task or note names the way people write them: `repo#n` of `board.owner`, a repository before a list (`beekeeper: #524, #525`) and a bare `#n` after the last repository named (`beekeeper#173, #176`), beside `owner/repo#n` and the URL; a bare `#n` before any repository names nothing, `note #n`, `timer #n`, `memo #n` and `decision #n` are beekeeper's own items, and a note's `--ref` links count too. Items a dispatch memo named in those forms were offered as free ([#565](https://github.com/giantswarm/beekeeper/issues/565)).

## [0.108.0] - 2026-10-07

### Fixed

- The watch's look at the Go build cache no longer races with the trim it started: the look takes the `goCaching` flag before it reads when the next look is due, and gives it back at once when none is, so the trim goroutine's write of that time is ordered before the read. `go test -race ./cmd/` failed `TestWatchSlowsInstallationReadsUnderCPUPressure` with a data race between `goCache` and its own goroutine whenever the timing lined up, reddening any pull request's go-build by chance ([#613](https://github.com/giantswarm/beekeeper/issues/613)).

## [0.108.0-rc.2] - 2026-10-07

### Added

- The PreToolUse hook refuses reading files known to hold secret values whole: omp's provider configuration (`~/.omp/agent/models.yml`, `config.yml`, `agent.db`) and a built-in list of credential files, the vault's `secret.tokenFile`, and what the new `secret.files` adds (globs and `~/` allowed). A `Read`, a content `Grep`, and a command that names the file (`cat`, `less`, `head`, `tail`, `cp`, `jq .`, `yq .`, `grep`, …) are refused with the read that passes, built from the file's key names: a `jq` or `yq` filter that deletes every credential key it holds (`del(.. | .apiKey?)`), a keys-only filter, `grep -c` or `-l`. The hook's matcher becomes `Bash|Read|Grep|Edit|Write|NotebookEdit|AskUserQuestion|SendMessage|mcp__.*`; `beekeeper install` updates it. A plain `cat` had put a value into a transcript ([#608](https://github.com/giantswarm/beekeeper/issues/608)).

## [0.107.2-rc.3] - 2026-10-07

### Fixed

- The vault keeper tries a failed sign-in again instead of taking one failure for a rejected password: after 30 s, 1 m, 2 m and then every 5 m within `secret.unlockWait` of the ask, each try bounded by two minutes. Before each try it asks the session bus for the person's credential store (the Secret Service: KeePassXC, GNOME Keyring, KWallet) and waits, looking every 15 s, while the store is locked and, within ten minutes of the boot, while nothing serves it yet. The journal names each failure's cause (the store locked or absent, the network, the command's bound, a rejection) with the command's last line, the watch says `VAULT SIGN-IN RETRYING: …` until the sign-in unlocks or gives up, and `VAULT SIGN-IN FAILED: <cause> after <n> tries: …` once it gave up, with a `vault.signin` line in `beekeeper log`. Only a password 1Password rejected while the store answered unlocked reaches the person, as one sign-in note that closes by itself once `beekeeper secret status` finds the broker holding a session (the command exits 78 while it holds none); a store that was locked or absent is the cause whatever the command said. The first sign-in after a boot ran seconds after it, before the person's credential store was up, and its one failure read as a rotated password: the vault stayed locked, every secret step waited, and a note asked the person to update an entry that was fine ([#601](https://github.com/giantswarm/beekeeper/issues/601)).

## [0.107.2-rc.2] - 2026-10-07

### Fixed

- The reopen after a start's or wake's headless turn runs in a transient unit of its own: the unit's `ExecStopPost` is `agents reopen --detach`, which starts `beekeeper-reopen-<id8>-<reopen>` (as the doctor's reopen of a rowless worker already ran, bounded to 35 minutes by `RuntimeMaxSec` instead of a stop timeout) in the turn's directory, naming the turn's unit for the leftover processes it reads, and returns at once; a start's or wake's unit itself stops within 60 s (`TimeoutStopSec`, under the user manager's default of 90 s), so a reboot or logout no longer waits for a reopen, and a hand-over stops the old session's reopen units too. The reopen waited for the person to leave the desktop's window in the turn unit's stop path, under a `TimeoutStopSec` of 35 minutes, where only a timeout could end it: every shutdown within half an hour of a finished headless turn held the user manager (`basic.target: stopping held back, waiting for: beekeeper-wake-…`) until PID 1 killed the whole user slice, and a shorter cap only killed the reopen ([#600](https://github.com/giantswarm/beekeeper/issues/600), [#587](https://github.com/giantswarm/beekeeper/issues/587)).

## [0.107.2-rc.1] - 2026-10-07

### Fixed

- A release older than the one that wrote the state no longer saves it: the save is refused with the state unchanged, logged once per process as `state.stale-writer` (the process, its command, both versions), and the command ends with that message (exit 3); a gate call waiting in a lane re-executes the binary installed at its path before it saves and refuses (77) only when it cannot, the lane untouched either way. `beekeeper self-update` names the beekeeper processes it leaves on the old binary (pid and command), and `beekeeper doctor` (`DOCTOR stale binary` in the watch) lists each while it runs, the watches apart (`WATCH STALE`). A watch or an `agents reopen` of the old release kept saving by its old rules after an install, undoing what the new one had recorded ([#595](https://github.com/giantswarm/beekeeper/issues/595)).

## [0.107.1] - 2026-10-07

### Fixed

- A queued `devctl release promote` promotes only the release candidate it was queued for: the gate records the newest candidate (`devctl release promote --dry-run`) when the call arrives and asks again right before devctl runs; when another candidate is newest by then, the promotion is refused (exit 77, `… was queued for candidate A, the newest is candidate B now: nothing promoted`), its place leaves the lane and its owner is woken to decide again. `beekeeper lanes drop <owner/repo> promote` takes a waiting promotion out of its lane, and a gate whose place was dropped, a queued run of its own included, refuses instead of joining again. A promotion queued by one worker fired after the merge ahead of it and promoted whichever candidate was newest then, another worker's included, and no command could take its place out ([#570](https://github.com/giantswarm/beekeeper/issues/570)).

## [0.107.1-rc.1] - 2026-10-07

### Fixed

- The merge gate knows which base branches auto-release: before devctl's turn it reads the pull request's base branch and the Auto-release workflow on it (`.github/workflows/zz_generated.auto_release.yaml`, its `on.push` branch filter), once per merge. A merge into a branch no Auto-release run tags (a maintenance branch whose tags are cut by hand, a fork line's backport branch) runs `devctl pr merge --no-release-wait`, frees its lane the moment devctl reports it merged, awaits no release and says so in the gate line, the `merged` event and the wake text; a branch with auto-release still holds its lane until the release rolled. GitHub not answering for the base branch refuses the merge (77). Such a merge waited for a tag that never came, holding its lane until a person stopped devctl ([#573](https://github.com/giantswarm/beekeeper/issues/573)).
- A SIGTERM aimed at a gated merge or wait stops its devctl too: when the gate's caller is still there two seconds after the SIGTERM, the gate passes it on to devctl and records the run as one ended by a signal. A caller going away (SIGHUP, the gate's parent gone with the SIGTERM) still leaves devctl running on ([#573](https://github.com/giantswarm/beekeeper/issues/573)).
- `hook pretooluse` leaves a command alone only when it invokes `memcap` or `beekeeper run` at a command position; a wrapper path it merely mentions (`ls …/memcap`, `M=…/memcap`, `grep -v memcap`) no longer lets its build run without a slot.

[Unreleased]: https://github.com/giantswarm/beekeeper/tree/main

## [0.107.0-rc.2] - 2026-10-07

### Fixed

- A waiting `agents reopen` stays under one CPU-second per minute on a busy machine: it reads the roster every 15 seconds instead of 5, each read parsing the whole state. A waiter used about 1.2 CPU-seconds per minute live; an `agents desktop` ask now reaches a waiting reopen within 15 seconds ([#498](https://github.com/giantswarm/beekeeper/issues/498)).

## [0.107.0-rc.1] - 2026-10-07

### Added

- `beekeeper run`'s commands share a CPU budget: `memcap.slice`'s `CPUQuota` (`memcap.cpuQuota`, default half the cores, `1200%` on 24) bounds the cores every capped run uses together, its `CPUWeight` (`memcap.cpuWeight`, default 50 against the desktop's slices' 100) their share while the desktop wants the cores too; `beekeeper install` writes both into the slice unit and every `run` sets them again (`systemctl set-property --runtime`) at the slot it takes, so the configuration rules without a reinstall, the slots share the budget by equal weight, and the command starts at nice 10 (`systemd-run --nice`; the sandbox's scope process nices itself before it execs the command). `snapshot` and `free` say the slice's budget and use (`build CPU: 1180% of 1200% in use …`, from its `cpu.max` and `cpu.stat`), and that it has no quota until the next run sets one. A slot's memory was capped and its CPU was not: one `golangci-lint` ran at 1358% and a `go build` took every core of the machine ([#581](https://github.com/giantswarm/beekeeper/issues/581)).
- The shipped `worker-rules` skill states five standing rules of the person: a barrier is filed and worked around, never a reason to park; a release candidate is proven and promoted as routine; no question about the person's local lab setup; a personal-account repository is merged with the person's own `gh` login; GitHub access is `gh` and the pro tools only.

## [0.106.1-rc.1] - 2026-10-07

### Fixed

- A gated command's `beekeeper-merge-*` unit fails only where a person must act: devctl's authentication (8), a kill, or the merge child itself. devctl's verdicts (0–7, 9, a mistyped flag's usage 7 included), the gate's own 76 and 77 (a queued merge's gate refused by a hold) and a stop with SIGTERM end it successfully; the outcome still reaches the calling session, `beekeeper lanes` and the event log as before. A started or woken session's `ExecStopPost` (`agents reopen`) no longer fails its unit when it is killed: it records its own outcome. `beekeeper doctor` and the watch's doctor pass run `systemctl --user reset-failed` for beekeeper's failed `beekeeper-*` units and name them in a DOCTOR line (`--dry-run`: "would reset"). Expected outcomes showed on the desktop as dozens of failed units a day ([#579](https://github.com/giantswarm/beekeeper/issues/579)).

## [0.106.0-rc.1] - 2026-10-07

### Added

- The PreToolUse hook refuses, in a session `agents start` started, a `gh api` read of an org's members, a login's membership, an org's teams or a team's members, or a login's orgs, by REST path (`orgs/<org>/members[/<login>]`, `orgs/<org>/memberships/<login>`, `orgs/<org>/teams/…`, `teams/…/members…`, `users/<login>/orgs`) or as the same query in a `gh api graphql` body (`membersWithRole`, an organization's `teams` or `members`, a user's `organizations`, inline, from a here-document or from the file `-f query=@…` or `--input` names), and names `beekeeper person <login> --org <org>` instead. The App's token an agent's `gh` carries sees public memberships only, so a worker probing the endpoints read a colleague as an outsider; a person's own session and every other `gh api` call pass as before ([#566](https://github.com/giantswarm/beekeeper/issues/566)).

## [0.105.0] - 2026-10-07

### Added

- `beekeeper person <login>` answers whether a GitHub login is a member of the org (`--org`, default `giantswarm`), private memberships included: `member`, `not a member`, or `permission missing` (exit 3) when the token may not read the whole membership. The roster is read with the person's own `gh` login, never the App's token an agent's `gh` carries, once its own membership proved active, and kept for a day (`--refresh`); in the sandbox the broker answers. The App's token answered `orgs/<org>/members/<login>` with 404 for a private member, which read as "not a member" ([#524](https://github.com/giantswarm/beekeeper/issues/524)).

## [0.104.1-rc.1] - 2026-10-07

### Fixed

- A message by the supervisor's or guide's run name ("Supervisor run 82") reaches the role's holder after its desktop titled the session differently: the SendMessage hook reads a name the holder carries or carried on the roster, or its session id, as the role and sends it to the holder's running CLI, where it was refused with "no CLI of it runs". The relay broadcast names both forms, "the supervisor" and the run name ([#530](https://github.com/giantswarm/beekeeper/issues/530)).

## [0.104.0] - 2026-10-07

### Fixed

- Every brokered `beekeeper secret` call is logged: a call's `secret.<operation>` entry waits for the state lock as an update does instead of giving up after a second, a call whose entry cannot be written fails with the reason and the entry it lost rather than ending unlogged, a call the broker refused, left waiting on the locked vault past `secret.unlockWait` or ended on its deadline is logged by the broker as the requester's, and every entry carries the operation's duration. A busy host state dropped the entries of exactly the calls that go through the broker, which printed `the call is not logged: context deadline exceeded` and succeeded ([#527](https://github.com/giantswarm/beekeeper/issues/527)).

## [0.104.0-rc.2] - 2026-10-07

### Added

- `beekeeper config set <key> <value> [<key> <value>]…` writes keys of the configuration file together in one rename, the result validated before it replaces the file: a result that would not load is refused (exit 3) with the file unchanged, and comments, the other keys, the mode and a link to the file stay. A reference and its source (a `store://` age identity and `secret.store`) are set in one call, so the file every session's hook loads is never seen between them ([#529](https://github.com/giantswarm/beekeeper/issues/529)).

### Fixed

- A `store://` age identity without its `secret.store` section no longer makes the configuration unloadable: the hook, `watch`, `run` and every other command keep working, and `beekeeper secret …` prints one warning per such reference. Adding the reference and the section in two edits failed every hook of every session on the machine in between ([#529](https://github.com/giantswarm/beekeeper/issues/529)).

## [0.104.0-rc.1] - 2026-10-07

### Added

- The watch says which agents' `gh` is not their own: the SessionStart hook and `agents register` record on the roster entry the `gh` the agent's shell resolves outside the sandbox, and an agent whose `gh` lies outside `agents.shell.path` (the `gh` link to devctl), or that has none, is one `GH UNBROKERED` line until it is its own again. A worker whose `gh` was the person's plain one acted without the App's token unseen ([#528](https://github.com/giantswarm/beekeeper/issues/528)).

## [0.103.0] - 2026-10-07

### Added

- The PreToolUse hook refuses a board-wide project read: `gh project item-list` without `--query`, and a `gh api graphql` query over a `projectV2`'s `items` without a narrowing `query:` argument (an issue's `projectItems` passes). The refusal names the estimated cost, the GraphQL budget as beekeeper last read it, and `gh project item-add`, `gh project item-edit` by item id, `beekeeper board move` and a filtered read instead. One such read of an organization board pages every item with its field values and spent the GraphQL limit every session on the machine shares ([#487](https://github.com/giantswarm/beekeeper/issues/487)).

## [0.102.2-rc.5] - 2026-10-07

### Fixed

- The archive reseed owes only what may go: an agent that reported done (`agents.done`) or a role's run a relay relieved, whose CLI does not run, that no person typed in and that no one brought back from the Archived list, each asked for only once its session ran no turn and was not shown for a day. The relay turn behind every peer send says the message is data for another session: a steward's archive request read like instructions to the relay model, which refused it in about half its turns ([#440](https://github.com/giantswarm/beekeeper/issues/440)).

## [0.102.2-rc.4] - 2026-10-07

### Fixed

- A gated `devctl pr merge … --detach` merges: the gate drops `--detach` and `--on-done` (`--on-done <command>` and `--on-done=<command>`) and runs the blocking merge, which its lane accounts for as any other (merged, released, settled), saying `dropped --detach …` once. Before, devctl's detached process died with the gate's transient unit and nothing merged, and the gate read devctl's start document as nothing merged and kept the place ([#548](https://github.com/giantswarm/beekeeper/issues/548)).

## [0.102.2-rc.3] - 2026-10-07

### Fixed

- `beekeeper secret set` (and `copy` into a file) pointed at a plaintext file that is no Secret skeleton refuses it by what it is, before sops sees it: `<file>: a plaintext ConfigMap, no sops metadata: a value goes into a sops-encrypted file or a plaintext Secret skeleton; pick the encrypted Secret, or start one in an absent file with --name and --namespace`, likewise for a file that is no YAML mapping, a document without a kind or a Secret without metadata; a plaintext Secret holding a value keeps its own refusal. The file's content is never quoted. Before, the file went to sops, whose `sops metadata not found` named neither the cause nor the way on, so a wrong path read like a broken vault ([#490](https://github.com/giantswarm/beekeeper/issues/490)).

## [0.102.2-rc.2] - 2026-10-07

### Fixed

- Finished workers' and relieved role runs' desktop sessions are archived without an idle steward: when no other desktop CLI of beekeeper's is idle to ask, the doctor (and `agents remove`, `agents handover`) shows the session in the desktop for a moment once the person's typing pauses, which warms its own CLI, and asks that CLI to archive itself. A run that could ask no steward (the desktop does not run, the person keeps typing) counts none of the five tries, and the doctor owes once more the archives it gave up, relieved role runs included. Before, every run without an idle steward counted a try, and after five "no idle desktop CLI … runs to ask" the session stayed in the sidebar for good ([#440](https://github.com/giantswarm/beekeeper/issues/440)).

## [0.102.1-rc.1] - 2026-10-07

### Added

- A session no longer runs a headless turn beside its desktop CLI: a headless resume (`agents wake`, a task turn the desktop does not run, a hand-over's note turn) sends its message to the session's desktop CLI when one started meanwhile, and a desktop send that starts the desktop's CLI (a wake, the standby's revive) waits up to two minutes for the session's headless turn to end and is refused while it still runs. The watch says `TWIN CLI: "<name>" runs <n> CLIs on session <id> (PID <pid> in <dir>, …)` for a session that runs two CLIs anyway (counted in the process table, once the youngest has run 30 s), and its ENDED line once one is left. Two CLIs on one transcript both worked the task, each unaware of the other, and a message by name reached only one ([#499](https://github.com/giantswarm/beekeeper/issues/499)).

## [0.102.0] - 2026-10-07

### Fixed

- `agents reopen` waiters no longer pile up and burn CPU. One reopen waits per session: a later turn's reopen leaves the showing to the waiting one and ends. A waiting reopen ends, showing nothing, once its agent reported its work done (`agents idle --done`), left the roster (`agents remove`) or is a relieved role run, and keeps a desktop CLI of the session that started meanwhile instead of showing it again. Its wait polls cheaply: the window's focus every 2 seconds instead of 500 ms, the roster every 5 seconds without the state lock, the screen lock every 15 seconds, and the input watch sleeps 250 ms after the events it drained. Each waiter read the 1.3 MB state and the whole process table twice a second, about 10 CPU-seconds per minute, and one machine ran six of them, two for a session already off the roster ([#498](https://github.com/giantswarm/beekeeper/issues/498)).

## [0.102.0-rc.3] - 2026-10-07

### Fixed

- A worker whose session the desktop never imported (its start ran at the desktop's cap of CLIs, so its task ran headless and the reopen after it missed) is no longer invisible: `agents` marks it `no desktop row` in REACHABLE (`--json`: `noDesktopRow`) and counts such agents under its table, the watch says `NO DESKTOP ROW` once per worker with what shows it, and the doctor reopens a worker whose CLI does not run once the desktop runs fewer CLIs than its cap, `agents reopen` in a transient unit `beekeeper-reopen-<id>-<n>` that gives the session its row and warms its CLI, one `DOCTOR` line and an `agent.reopen` event each, never within seven minutes of the start, beside a unit of the session or a reopen of its own, and counting the reopens under way against the cap; `doctor --dry-run` names the workers it would reopen and those the cap leaves without a row. Before, the event log was the only sign of such a worker ([#480](https://github.com/giantswarm/beekeeper/issues/480)).

## [0.102.0-rc.2] - 2026-10-06

### Fixed

- `board next` skips an issue a served pull request closes: a serve record, a busy agent's task or a note naming a pull request covers the issues its closing references name (`Closes`/`Fixes`, across repositories), read from GitHub in one request, the skip saying `served by "<session>" through owner/repo#<pr>`. A worker that recorded the PR it opened left its issue free, and the picker handed it to a second worker ([#520](https://github.com/giantswarm/beekeeper/issues/520)).

## [0.101.3-rc.2] - 2026-10-06

### Fixed

- The supervisor's `yours <resource>` is the grant it says it is: a `SendMessage` or `beekeeper agents wake` from the session holding the supervisor role whose message says `yours <resource>`, `<resource> yours` or `<resource> is yours` for a leasable resource records the grant to the message's target as `beekeeper lease grant` does, and the supervisor reads what was recorded (or why nothing was, with the command that records it) in the hook's answer or the wake's output. A claim refused for a missing grant names the supervisor, the resource, the caller and the exact `beekeeper lease grant <resource> "<session>"` that records one. Before, the word and the record drifted apart: the worker's claim was refused as "a free lease is not a grant" without saying that a grant was what was missing ([#481](https://github.com/giantswarm/beekeeper/issues/481)).

## [0.101.2] - 2026-10-06

### Fixed

- The devctl release window lifts without a manual step when merges queue behind a devctl release. A merge queued behind the release started on the old devctl seconds after the release, devctl refused it (exit 7, outdated), and its "merged nothing" lifted the window before anything installed the release, so every devctl run stayed outdated until somebody updated it by hand. Every merge or promotion that runs devctl now starts with `devctl version update` (installs the latest release when the local devctl is behind, a failure logged as `merge.update`); the next giantswarm/devctl merge waits while the previous window waits for its release; a merged window lifts only once `devctl version` reports its release, not an intermediate version; and the gate installs the release after a devctl merge also when its window was lifted by hand ([#503](https://github.com/giantswarm/beekeeper/issues/503)).

## [0.101.1] - 2026-10-06

### Fixed

- A sandboxed session starts on a fresh state directory: `beekeeper sandbox install` and the broker at its start create every denied path's mount point (`scan/` in the state directory). The sandbox cannot create one in the read-only remount of its writable parent, so every session failed at start with `Can't create file …/scan: Read-only file system` ([#461](https://github.com/giantswarm/beekeeper/issues/461)).
- `beekeeper sandbox install` refuses a writable path inside a readable one (a `sandbox.allowWrite` under a `sandbox.allowRead`, or beekeeper's state under its config directory) and names both: the sandbox mounts the readable parent read-only over the writable child, so the spool went unwritable and sessions found no broker ([#462](https://github.com/giantswarm/beekeeper/issues/462)).

## [0.101.0] - 2026-10-06

### Added

- The Go build cache every session's builds share stays bounded: every watch reads its size every `doctor.goCacheEvery` (1h) and, over `doctor.goCacheMaxGiB` (20 GiB), removes the least recently used entries down to three quarters of the cap, never while one of the user's `go` commands runs, one process at a time, each trim one `gocache.trim` event with the size before and after; a trim that waited an hour for the builds, or an unreadable cache, is one `GO CACHE` line, and `beekeeper doctor` trims or (`--dry-run`) sizes it too. Go drops only entries unused for five days, so the cache reached 72 GB on a busy machine ([#495](https://github.com/giantswarm/beekeeper/issues/495)).

## [0.100.0] - 2026-10-06

### Fixed

- A relayed supervisor or guide successor gets its desktop CLI while the desktop runs its cap of CLIs: `makeRoom` ends the idle CLI of the role run the relay relieved (its work ended with the relay) and, for a role's holder or relay successor, a worker's idle on its task, which a message by name starts again. The standby watch's revive of a holder with no row in the desktop imports the row first, then sends, instead of "the next gap tries again". Before, the successor's reopen missed at the cap, the role lapsed until a peer woke it, and the relieved run kept its CLI ([#484](https://github.com/giantswarm/beekeeper/issues/484)).

## [0.100.0-rc.1] - 2026-10-06

### Added

- The watch warns before `/` runs full: `DISK FILLING: / loses <n> GiB/min, <n> GiB free, full in ~<n> min` while it would run full within `watch.diskFillWithin` (2h) at the rate its free space fell over the last ten minutes, naming the commands and sessions whose processes wrote most and how much of the loss no readable process accounts for (a root daemon such as dockerd, or ended processes), and `DISK NEARLY FULL` under `watch.diskCriticalMiB` (1% of `/`). `LOW DISK` alone was said once at its threshold and never again while free space fell to zero ([#494](https://github.com/giantswarm/beekeeper/issues/494)).

## [0.99.4] - 2026-10-06

### Added

- A sandboxed `beekeeper agents start`, `wake` or `resume` keeps a scratch state: `--config <scratch>` (or `$BEEKEEPER_STATE_FROM`) goes to the host's broker, which takes only that file's `stateDir` and `leaseDir`, held to the session's lists, and runs the call under the host's configuration with them; the started agent keeps the scratch state through `$BEEKEEPER_STATE_FROM`. Before, the broker refused `--config`, dropped `$BEEKEEPER_CONFIG`, and a scratch state had no broker to ask, so a sandboxed proof could only start on the live roster ([README](README.md#the-agent-sandbox)).

### Changed

- The README's browser section says why a headless turn of a start keeps its own Claude in Chrome tools (`--chrome` in bypass) rather than going through `beekeeper browse`: the bypass is that turn's scope, so narrowing its Chrome tools would narrow nothing; a desktop turn browses through `browse`, a headless turn with its own tools ([README](README.md#agents-started-without-a-click)).

## [0.99.3-rc.1] - 2026-10-06

### Fixed

- `budget --json` reports `used` as what the limit lost (`limit - remaining`) for a GraphQL reading reused from the state and for a core answer without `X-RateLimit-Used`, where it printed 0, and reads the GraphQL limit again once the stored reading's reset has passed instead of printing a reset in the past.

## [0.99.2] - 2026-10-06

### Fixed

- `secret set` and `secret copy <ref> <file#path>` never write a value outside the SOPS file's encrypted fields: on a Kubernetes Secret a path that names neither `data` nor `stringData` goes under `stringData` (`set <skeleton> default --generate` used to write a top-level `default`, which a `^(data|stringData)$` `encrypted_regex` left in plaintext), and a path the file's `.sops.yaml` creation rule would leave in plaintext is refused before any value is drawn or written, the error naming the rule.

## [0.99.0] - 2026-10-06

### Added

- `beekeeper browse "<steps>"` runs browser steps in a headless `claude -p --chrome` turn that has the CLI's own Claude in Chrome tools and nothing else (no shell, no file tools, no other MCP server, dontAsk with a Chrome-only allow rule), on a connection that never waits on a site approval, and prints the turn's report, its transcript and every screenshot it took as an image file under `<stateDir>/browse/<id>/`. `beekeeper hook pretooluse` refuses the Claude in Chrome tools in the desktop turns of the sessions `agents start` starts and names `browse`: Claude Desktop holds such a session's navigate to a new site for a person's site approval, which no hook answers. Headless turns of a start (`agents wake`, a task turn the desktop does not run) get `--chrome`. A person's own sessions are untouched.

## [0.98.1-rc.1] - 2026-10-06

### Fixed

- `agents start` no longer loses a worker's task when the desktop does not import the seeded session (at its cap of CLIs with none of beekeeper's to end, or the person still typing): the start logs why and runs the task headless, whose reopen imports the session later. A start that still delivers no turn of the task exits non-zero, logs "task not delivered" with the reason, and `agents` (REACHABLE) and the watch's AGENTS STOPPED line say "task not delivered" until `agents wake` delivers a turn. Such a start used to leave the worker idle after its seed turn, busy on the roster, with nothing in the log.

## [0.98.0] - 2026-10-06

### Fixed

- A session that carries the sandbox policy's environment while no sandbox holds its commands (Claude Code applied the policy's `env`, not its `sandbox` block) acts on GitHub as on the host again: the agent shell's prelude and beekeeper at start drop the egress variables and `BEEKEEPER_SANDBOX` unless `SANDBOX_RUNTIME` is set. Before, gh sent the egress login's fixed word straight to GitHub (401), `budget --gate` and the merge gate failed on it, git had no credential helper for GitHub, and the prelude took devctl's `gh` link off `PATH` ([README](README.md#the-agent-sandbox)).

## [0.98.0-rc.1] - 2026-10-06

### Added

- `secret.ageIdentities` takes an entry of the person's own credential store, `ref: store://<entry>`, read by the broker through the person's own commands in `secret.store` (`read`, and `search` for `store://` with no entry, which finds the entry by the SOPS file's recipients through the store's own search). The store shows whatever unlock prompt it shows; only the identity of the file's recipient reaches the one sops call, and nothing is printed, copied or fingerprinted ([README](README.md#age-identities)).

## [0.97.2] - 2026-10-06

### Fixed

- The broker runs a brokered `beekeeper secret` call on a SOPS file whose age identity is an `op://` field of `secret.ageIdentities` with the vault session, as the client decides: both use one predicate, the file named relative to the requester's directory. Such a call ran without the session and failed.

## [0.97.2-rc.1] - 2026-10-06

### Added

- A sandboxed git signs its commits and tags with the person's key: the policy sets git's `gpg.program` to a script the broker writes into the egress directory, which runs the hidden `beekeeper sandbox gpg`; it hands the payload to the broker, and the broker runs the person's gpg on the host, where gpg reaches its agent, and answers the signature and gpg's status lines. Only git's signing call (`--status-fd=2 -bsau <key>`) passes; the key and the agent stay outside the sandbox ([README](README.md#trust-model)).

### Fixed

- In the agent sandbox the agent shell's prelude takes `agents.shell.path` off an inherited `PATH` as well, so `gh` is gh, through the egress proxy, and not the gh link to devctl, which reads a keychain the sandbox closes.

## [0.97.1] - 2026-10-06

### Fixed

- `beekeeper budget`, the snapshot, the `ui` screen and the watch read the GitHub GraphQL limit beside the REST one, from a real `rateLimit` query: a GraphQL refusal shows while REST has headroom, named as the hourly limit spent or a secondary limit, with its reset or Retry-After. The watch says it once with the `gh` and `devctl` callers and their sessions, and its end; `budget --gate` exits 3 while it holds ([README](README.md#what-it-does)).
- The watch no longer sends the "GitHub budget under the floor" notification while the budget is above the floor.

## [0.97.0] - 2026-10-06

### Changed

- The agent sandbox's GitHub token never enters the sandbox. `beekeeper sandbox broker` is the sandbox's only egress proxy (`sandbox.proxyPort`, 3190, on `127.0.0.1` for its own user only): the policy points Claude Code's `httpProxyPort` and `socksProxyPort` at it, it holds every connection to GitHub and `sandbox.domains`, refuses the rest and never dials a loopback or link-local address by name, and terminates TLS for `github.com`, `api.github.com` and `uploads.github.com` with a name-constrained CA made in its memory to set the `Authorization` header itself, request bodies untouched. A GitHub endpoint that echoes its input no longer returns the token. The masked token files, their masks, `tlsTerminate`, `sandbox.mask` and `beekeeper sandbox git-credential` are gone; the broker removes the old files under `$XDG_RUNTIME_DIR/beekeeper/github` at start ([README](README.md#the-agent-sandbox)).
- The agent sandbox denies the runtime directory (`$XDG_RUNTIME_DIR`) for reading, apart from the egress proxy's CA and gh's login.

## [0.96.1-rc.1] - 2026-10-06

### Fixed

- The broker's vault session no longer dies unseen after 30 idle minutes: its keepalive touched the session with `op whoami`, which op does not count as activity; it now lists the vaults. A session op no longer takes is dropped and signed in again at once, by the keepalive or by the call op refused, which is retried once with the new session; `copy <ref> -- <consumer>` exits 78 for a vault it cannot read again (it exited 77, so the broker never retried it). The watch says `VAULT SESSION DROPPED` until the new sign-in.

## [0.96.0] - 2026-10-06

### Added

- `beekeeper secret set` fills a plaintext Kubernetes Secret skeleton (apiVersion, kind, metadata, an empty `stringData`) and writes it back as a SOPS file encrypted to the `.sops.yaml` recipients, and `--name`/`--namespace` start an absent file as such a Secret, so a generated value lands in a file Flux applies ([README](README.md#secret-operations)).
- A consumer can be a command in a pod: `-- kubectl exec -i --context <context> <pod> -- <consumer…>`, whose command reads the value on stdin (no TTY, no `-v`, never a production context), with `garage json-api <endpoint> -` a consumer of its own. `--stdin-json '<object>' --stdin-field <key>` hands the consumer a JSON request with the value at `<key>`, which imports a generated key into Garage (`ImportKey`) without the value on any argv ([README](README.md#secret-operations)).

## [0.95.0] - 2026-10-06

### Added

- `beekeeper secret set <file> <path> --generate` takes no `--vault` when the value belongs in the SOPS file alone: beekeeper draws it in its own process and writes it only into the SOPS path. `--to-secret <context>/<namespace>/<name>/<key>` (a lab the caller holds) or `-- <consumer…>` (the consumers `copy` allows) receives the same value in the same call, with or without a vault ([README](README.md#secret-operations)).

## [0.95.0-rc.1] - 2026-10-06

### Changed

- `beekeeper agents start` runs a worker in Claude Desktop from its first turn: a seed turn without tools creates the session, the desktop imports it under its name at once, and the task runs as a desktop turn of the session's desktop CLI, or of one a steward's `send_message` starts, after `makeRoom` kept the desktop under its cap. `agents wake` of a session with a desktop row and no CLI takes the same route. A turn runs headless only where the desktop cannot run it, and the command says why; a relay successor keeps its headless first turn ([README](README.md#agents-started-without-a-click)).

## [0.94.0] - 2026-10-06

### Added

- `secret.ageIdentities` takes an age identity file, `ref: file:///<absolute path>`, besides a vault field: for an identity that stays out of every vault. The broker reads it in its own process, without the vault session, and gives only the identity of the SOPS file's recipient to the one sops call; an identity file's comment lines and several identities are understood, for vault fields too ([README](README.md#age-identities)).

## [0.93.1-rc.1] - 2026-10-06

### Fixed

- `beekeeper secret set --generate` creates and edits the vault item again: op refuses piped input next to `--template` ("cannot create an item from template and stdin at the same time"), so the item's JSON now goes to `op item create -` and `op item edit` on stdin alone; `secret rotate --generate`, which edits the item, is fixed with it.

## [0.92.1-rc.3] - 2026-10-06

### Fixed

- The broker's vault session ends at the exact end of `secret.sessionLifetime`, not up to a minute later on the keeper's next look, so the watch's `VAULT UNLOCKED … until <t>` holds to the minute.

## [0.92.1-rc.1] - 2026-10-05

### Changed

- With `secret.session` the broker signs in to the vault by itself through the new `secret.signinCommand`, at its start and whenever a call waits, one sign-in for every waiting call, and holds the session for `secret.sessionLifetime` (12h), touching it every 10 minutes so that op does not let it idle out. The watch says `VAULT UNLOCKED … until <t>` for that lifetime and `VAULT SIGN-IN FAILED: <reason>`; no message sends the person to a terminal ([README](README.md#the-vault-session)).

### Fixed

- `beekeeper secret unlock` reached no broker: the broker is undumpable, so its `/proc/<pid>/exe` is root's and the peer check failed before anything was sent or logged. The check now identifies the broker as the main process of `beekeeper-sandbox.service` running this binary, from systemd, and a refused unlock is logged.
- The watch no longer ends a waiting vault call with a bare ENDED line when it gave up: `VAULT LOCKED: <who>'s call on <ref> timed out …, still locked`.

## [0.92.0] - 2026-10-05

### Added

- A sandboxed session runs the roles' host commands through the broker, as itself and in its working directory: `beekeeper agents start`, `wake` and `resume` (the user bus starts the agent's unit; a start's brief and `--dir` are held to the session's lists), `beekeeper watch` (the person's kubeconfig and Teleport login stay outside the sandbox; the watch runs in a scope of its own and its lines stream back to the `Monitor`, which stops it by stopping), and the new `beekeeper lease up|down <lab>`, which creates or tears down the kind cluster of a lab whose lease the caller holds (`agentlab up` asking nothing, or `agentlab down`, capped as `beekeeper run` caps it). The broker streams a call's output while it runs and ends it once its sandboxed command ended. In the sandbox the hook refuses `agentlab up|down` and `kind create|delete cluster` and names `lease up|down` instead ([README](README.md#the-agent-sandbox)).

## [0.92.0-rc.1] - 2026-10-05

### Added

- `beekeeper secret` names a missing age identity instead of passing on sops' `exit 128 (Failed to get the data key …)`: before sops decrypts a file encrypted to age recipients, beekeeper checks `SOPS_AGE_KEY`, `SOPS_AGE_KEY_FILE` and sops' `keys.txt` for an identity of one of them, and without one fails naming the recipients, each source checked and the configuration that supplies one. The new `secret.ageIdentities` maps a recipient or a `pathRegex` to the `op://` field of the shared vault holding its identity: beekeeper reads it in its own process, checks it belongs to one of the file's recipients and gives it to that one sops call's environment alone, so `copy`, `compare`, `fingerprint` and `rotate` work on files only the vault has an identity for. With `secret.session`, such a call runs in the broker ([README](README.md#age-identities)).

## [0.91.2-rc.1] - 2026-10-05

### Fixed

- The watch's doctor says an unchanged chore line once per watch instead of on every pass, and the finished workers waiting on an unset `agents.archiveAgreement` as one summary line (the count and the key to set), ended once none waits, instead of one line and event per agent every 30 s.

## [0.91.1] - 2026-10-05

### Changed

- The desktop sessions of finished workers are archived only under the person's agreement: `agents.archiveAgreement` says where they agreed (never for a session they started), the steward's request quotes it, and the steward runs the new `beekeeper agents archivable <local_id>…` first, which confirms each session is a finished worker beekeeper started and exits 3 for any other. Without it no steward is asked. The doctor reads each asked steward's turn: the line names the steward whose `archive_session` call archived a session, reports a decline with the first line of the reply, and asks a declining steward for no archive for 24 hours instead of again on every run ([README](README.md#agents-started-without-a-click)).
- The doctor owes, once, the archive of every finished worker beekeeper started whose desktop record stayed unarchived, its CLI running or not, and asks for at most 20 owed archives per run.
- A reopen never warms a desktop CLI beside a headless turn: it looks for the session's wake right before it shows the session, after its wait for the person, and the standby watch does not resume a role's holder whose reopen runs while it asked for a desktop turn (`agents desktop`), since that reopen shows it at once.

## [0.91.1-rc.2] - 2026-10-05

### Fixed

- A relayed role holder's import no longer misses on a keystroke: a desktop turn's import (a role's successor, `agents start --desktop`) gives the person's typing its bound again when the link checks the desktop once more, as its first wait did, instead of failing at once ([#395](https://github.com/giantswarm/beekeeper/issues/395)).

## [0.91.1-rc.1] - 2026-10-05

### Fixed

- `RELAY DUE` and `GUIDE RELAY DUE` are said again after `relayAt` changed: the state records the `relayAt` a relay due was said at, and a term told at the old value is told at the new one. Every long-running `watch` and `guide watch` says a standing relay due once more when it did not say it yet (a restarted watch), keeping when it was first reported. The standby watch relays a supervisor or guide still over its `relayAt` `<role>.relayGrace` (new, 30m) after its relay due, with no relay open and, for the supervisor, at a quiet moment: it starts the successor as the holder's own `relay` does (`RELAYED`, `GUIDE RELAYED`), and the successor's start takes the role.

## [0.91.0] - 2026-10-05

### Changed

- With `secret.session`, the vault session lives in the broker's memory alone: every `beekeeper secret` call on an `op://` reference runs in `beekeeper sandbox broker`, from an agent session, a sandboxed one or the person's terminal, and the broker gives the session to its own `op` calls only. The person unlocks it with the new `beekeeper secret unlock` in their own terminal (`op signin` there, the session handed over a 0600 socket in the runtime directory to a listener checked to be this binary); `secret lock` forgets it, `secret status` says whether one is held. While it is locked a call prints `vault locked: waiting for the person's approval` and waits up to `secret.unlockWait` (8m), the watch says `VAULT LOCKED: <who> waits on <ref>` and `beekeeper status` names the waiting sessions; nothing asks the person ([README](README.md#the-vault-session)).
- The README has a threat model: the parties, the secrets and where they live, why agents read and unlock none, and the residual risks ([README](README.md#threat-model)).

## [0.91.0-rc.2] - 2026-10-05

### Changed

- No agent session signs in to or unlocks a vault: the Secret guard refuses `op signin`, `op account add`, `op unlock`, the person's unlock helpers (`secret.unlockCommands`), `beekeeper secret unlock` and keyring reads (`secret-tool lookup|search`, `security find-*-password`), and `op run` like every other `op` command. It scans every argument a shell runs (`zsh -ic <helper>`, `bash -c <word>`, `"$SHELL" -c`), `eval`'s words, shell functions and groups on the line, and the scripts a command line runs (`bash x.sh`, `./x.sh`, `source x.sh`). The agent shell prelude drops `OP_SESSION_*`, `OP_SERVICE_ACCOUNT_TOKEN` and `OP_CONNECT_TOKEN` from every agent command's environment ([README](README.md#secret-reads)).

## [0.91.0-rc.1] - 2026-10-05

### Added

- `timer add --repeat daily|weekdays|weekly` re-arms a `--wake` or `--run` timer at the same local time of day once it fired, a weekday timer skipping Saturday and Sunday, instead of closing it: a daily brief or a Friday summary is one timer that stays open until `timer done`, its `TIMER` line naming the next time. A repeat on a condition or on a plain timer is refused ([README](README.md#what-it-does)).

## [0.90.0] - 2026-10-05

### Added

- A sandboxed session's `gh`, `git push` and `devctl` act with devctl's App user token, which commands see only as a placeholder. `beekeeper sandbox broker` reads the token every five minutes with `devctl auth exec` (`sandbox.devctl`) into gh's login and git's Basic credential under `$XDG_RUNTIME_DIR/beekeeper/github`, rewritten in place. The policy masks both files, so the sandbox proxy puts the real token into requests to GitHub's hosts only, and a renewed token reaches a running session. It also points `GH_CONFIG_DIR` there, rewrites `git@github.com:` to HTTPS and makes `beekeeper sandbox git-credential` git's helper (an `authtype=Basic` credential, git 2.46 or newer). A sandboxed `beekeeper gate` (devctl `pr merge`, `pr wait`, `release promote`, `release wait`, `rollout wait`) runs on the host through the broker, as the session. In the sandbox, `agents.shell.path` stays off `PATH`. The proxy substitutes the placeholder in request bodies as well, so an endpoint that echoes its input still returns the real token ([README](README.md#the-agent-sandbox)).

## [0.89.1] - 2026-10-05

### Fixed

- A relayed supervisor or guide is imported into the desktop's sidebar under its run title without a click: its import goes ahead past the desktop window's focus as a desktop turn's does, and a reopen that meets the standby's headless resume of a session the desktop never imported imports it beside that turn instead of leaving it to the end of a turn that keeps the role's watch. An import beside a headless turn (first turn or wake) freezes whichever runs and stops the desktop's CLI it warms; a wake turn is never taken for the desktop's CLI ([#395](https://github.com/giantswarm/beekeeper/issues/395)).

## [0.84.0] - 2026-10-05

### Fixed

- A reply the Slack feedback watch delivers no longer carries what the Slack MCP server and the connector append to its text: its `Reactions:` line and the claude.ai connector's `*Sent using* <@…|Claude>` attribution.

## [0.84.0-rc.1] - 2026-10-05

### Added

- `beekeeper sandbox broker` caps the agent sandbox's builds from the host: on Linux the sandbox blocks every Unix socket, the user bus included, so a sandboxed `beekeeper run` asks the broker through request files in `<stateDir>/sandbox` to cap its slot and move it into its memcap scope, and the command runs there, still in the sandbox. The broker answers each request as the one process that holds it open, never a process the request names, and only for memcap's own slices and scopes; `beekeeper install` puts it in place as `beekeeper-sandbox.service`. With no broker answering, a sandboxed `beekeeper run` refuses (exit 1) instead of building uncapped. The policy lets sessions write the build slots ([README](README.md#the-agent-sandbox)).

## [0.82.0] - 2026-10-04

### Added

- `beekeeper agents park [--on <#note|owner/repo#n>] "<what it waits for>"` parks the calling agent on a decision: it keeps its task but counts parked, not busy, in `agents` and `capacity`, the doctor and the watch's `AGENTS STOPPED` leave it alone, and it ends its turn instead of sleeping in a slot. Once the note it parked on is closed or the issue or pull request merged or closed, the watch says one `AGENT RESUMABLE "<agent>"` line with what settled it, a person's answer word for word; with `agents.autoResume` the watch resumes the agent with those words, by name to a running CLI or headless. `beekeeper agents resume <agent>` does it by hand. A park without a task is refused ([README](README.md#agents-started-without-a-click)).

## [0.82.0-rc.2] - 2026-10-04

### Added

- `beekeeper board next` passes over an item whose `lease/<resource>` label names a lease another session holds (`needs lease agentlab-1, held by "…"`) and offers the items behind it, and prints the leases a picked item needs: an issue carries the lease its work needs, and no worker picks work it cannot start ([README](README.md#what-it-does)).

## [0.82.0-rc.1] - 2026-10-04

### Added

- The Slack feedback watch: with `feedback.context` set, the standby watch reads the thread of every report the scheduled reporter posted through the Slack MCP server behind muster, as the person, and delivers each reply of the report's author to the agent it names first (`<agent name>: <message>`) or else to the supervisor, as `agents wake` does; a delivered reply gets an :eyes: reaction and a `FEEDBACK` line, a thread that cannot be read one `FEEDBACK UNREADABLE` line ([README](README.md#feedback-on-the-reports-from-slack)).

## [0.81.1-rc.2] - 2026-10-04

### Changed

- A merge whose devctl runs on after its pull request merged no longer holds its lane: `beekeeper watch` ends the devctl of a running merge whose pull request merged more than `merge.hungAfter` (45m, devctl's own `--timeout`) ago, or closed, with one `MERGE HUNG` line and a `merge.hung` event, and records it at the next poll (merged: the lane settles, release unconfirmed). `lanes drop <owner/repo> <n>` ends such a run at once and records it; one whose pull request is open is refused ([README](README.md#the-merge-gate)).

## [0.81.1-rc.1] - 2026-10-04

### Changed

- The standby watch (`beekeeper watch --standby`, the `beekeeper-notify` unit) reads the installations' upgrades and sets and lifts their holds while no supervisor's watch runs: a relay, a crash or a frozen desktop no longer leaves an installation unheld at `UPGRADE` or held past `UPGRADE ENDED`. The supervisor's watch says when its upgrade cycle last began in `upgrades-watch.json`; the standby watch reads none of its own while that is younger than five `watch.interval`, and says `UPGRADES read by the standby watch` once when it takes over ([README](README.md#cluster-upgrades)).

## [0.81.0] - 2026-10-04

### Added

- `beekeeper secret copy <ref> --to-secret <context>/<namespace>/<name>/<key>` writes one value into a key of a Secret in a kind lab whose lab lease the caller holds, through kind's admin kubeconfig and a server-side apply, the value never reaching the session; any other context is refused. Every `beekeeper secret` command exits 78 when the shared vault cannot give a value, and a read of the vault fails after a minute instead of hanging ([README](README.md#secret-operations)).

## [0.80.0] - 2026-10-04

### Changed

- `beekeeper run` from a Claude Code session that holds a build slot already joins that slot at once instead of taking the second one: a lab proof in the background and a test suite beside it hold one slot, and the other stays free for another session. Each slot is a slice under `memcap.slice` (`memcap-slot<n>_<hash of the slot directory>.slice`, `memcap-slot<n>_test.slice` for a `MEMCAP_TEST=1` run) with the slot holder's `MemoryMax` and `MemorySwapMax`, so the runs sharing a slot share its cap; a kill at that cap is attributed by the killed task's scope (`task_memcg`), in `run`'s victim line, `snapshot` and `watch` alike.

## [0.80.0-rc.6] - 2026-10-04

### Changed

- `beekeeper watch` reads the installations' Cluster API upgrades every `upgrades.every` (5m) instead of every `watch.interval`, all four kinds in one `kubectl` call per installation (one call per kind only on an installation that does not serve them all); an installation an upgrade runs on or holds is read every `watch.interval` until `UPGRADE ENDED`. The readings are shared in `upgrades.json`: a second watch, `snapshot` and `ui` use one younger than `upgrades.every`. One watch over five installations starts 5 `kubectl` per 5 minutes for upgrades instead of 21 every 30 seconds ([README](README.md#cluster-upgrades)).

## [0.80.0-rc.5] - 2026-10-04

### Added

- A merge lane whose installation is central queues centrally as well: the gate runs a merge only when it is next in the machine's lane and in the central one, makes it the central lane's running merge, reports its outcome, and the watch takes it out of the central lane once its release rolled, so two people's merges into one lane roll one after the other. Holds on a central lane or on a repository in one go to the central instance (`hold set|lift|check|list`), and its gate refuses a held merge on every machine. `lanes central` lists the central lanes; `lanes leave owner/repo#n` takes a merge out of one by hand. `beekeeper serve`'s `lane_leave` takes a merge out of its lane (its person's or the team's supervisor role's), `lane_queue` and `lane_turn` refuse a held merge, and a waiting place unseen for `merge.queueTTL` (`MergeLane` `status.queue[].seen`) holds up nobody ([README](README.md#the-central-instance)).

### Fixed

- `beekeeper serve`'s `hold_list` lists the holds: it ran the `hold` verb with no arguments, which read serve's own command line (`serve --http=…`) and failed with `unknown flag: --http`.

## [0.80.0-rc.4] - 2026-10-04

### Added

- `beekeeper sandbox render|install`: the agent sandbox as a drop-in of Claude Code's managed settings, enforced by Anthropic's sandbox runtime on every session's commands: the home directory denied and only beekeeper's own paths, the harness's transcripts, plans, skills and plugins and `sandbox.allowRead` re-allowed; writes to the working directory, the temporary directory, beekeeper's state and `sandbox.allowWrite`; egress to GitHub and `sandbox.domains` only; `GH_TOKEN` and `GITHUB_TOKEN` masked, the real value going to GitHub's hosts only; no unsandboxed retry, and no session where the sandbox cannot start. `install` stages the file and prints the root command. The PreToolUse hook holds Read, Grep, Glob, Edit, Write and NotebookEdit to the same lists in every session the policy sets `BEEKEEPER_SANDBOX` in, in the hooks' scope or not ([README](README.md#the-agent-sandbox)).

## [0.80.0-rc.3] - 2026-10-04

### Added

- `beekeeper hold set --lift-when "<shell command>"`: the watch runs the probe every tick and lifts the hold once it exits 0, logged as `hold.lift` with the probe; `hold` lists the probe as the hold's end.
- A devctl release window installs its release: once the window's merge merged, the gate runs `devctl version update` right after it and the watch retries every 2 minutes while devctl still reports the version the window opened on (a failed update is logged as `hold.update`), so the window lifts without anybody updating devctl or lifting it by hand.

### Fixed

- A devctl merge that warrants no release lifts its release window at once; the window waited for a devctl version that never came.

## [0.80.0-rc.2] - 2026-10-04

### Added

- `central: {context: <muster context>}` puts the leases of the shared installations in the central instance: `lease claim|release|status` of a name that is none of the machine's own resources goes to `beekeeper serve` through muster as the person (its endpoint from the muster context, the token from `muster auth token` at each call), and `lease list` shows the central leases below the machine's. Another person's lease is refused with its holder; an unreachable central instance refuses the verb with exit 69 and no local fallback. `beekeeper watch` publishes the machine's agents to the central roster on their transitions and says `CENTRAL UNREACHABLE <context>: <reason>` once ([README](README.md#the-central-instance)).
- `beekeeper serve`'s `agents_register` takes `task` and `done` from a machine publishing its agents, and `agents_leave` takes an agent off the roster; a failed tool result carries beekeeper's exit code (`code`) beside its outcome.

## [0.80.0-rc.1] - 2026-10-04

### Added

- `agents.shell.path`: directories the agent shell's prelude puts first on `PATH`, in their order and once each, so an agent's own programs (a `gh` link to devctl that acts with the App's short-lived token) shadow the person's.

## [0.79.0] - 2026-10-04

### Added

- `beekeeper serve`'s `converse(text)` lets one of a person's agents on the roster, their guide above all, talk to them in Slack through klaus-gateway: the first message opens a conversation (`POST /conversations`), a direct message whose thread is kept on the agent's roster entry (`status.conversation`), later ones go into it (`POST /conversations/{id}/messages`), and a new thread opens once klaus-gateway dropped the old one. The person's reply there calls `serve.gateway.sendTool` (default `x_beekeeper_send_message`) through muster as the person and arrives in their mailbox with `from.source` `slack`; a reply to an agent not on the roster is refused as not running, which klaus-gateway says in the thread ([docs/feed.md](docs/feed.md#the-mailbox)).

## [0.78.0] - 2026-10-02

### Changed

- `beekeeper serve` shows each person only the agents they may read, alike in `list_agents`, `beekeeper://roster`, `beekeeper://feed` and their notifications: their own local agents, their team's local agents that hold an Environment's lease (work on a shared installation), and every remote agent; a feed event about a roster entry follows that entry. `send_message` to a `local:` address is accepted only from the agent's person. Every member read every person's local agents, and any member could queue messages into another person's mailbox ([docs/feed.md](docs/feed.md#who-reads-which-agents)).

## [0.77.1-rc.1] - 2026-10-02

### Fixed

- `board next` counts the `sessions serve` record of an agent on the roster busy with its task as covering its item while the agent's CLI is gone (the desktop warmed none, the watch marked the record ended), so `--claim` no longer hands it to a second worker; it is offered again once the agent reports idle or leaves the roster.

## [0.76.0] - 2026-10-02

### Added

- `beekeeper serve` offers its state as MCP resources a client subscribes to with `subscriptions/listen` and is told of by `notifications/resources/updated`: `beekeeper://environments/<name>`, `beekeeper://lanes/<name>`, `beekeeper://roster` and `beekeeper://feed` for every member, `beekeeper://notes/<person>` and `beekeeper://mailbox/<person>` for that person only (a read or a subscription by anyone else is refused). The feed has a versioned schema, `beekeeper.giantswarm.io/feed/v1` ([docs/feed.md](docs/feed.md)): event ids that only grow, across restarts too, kind, subject, actor, time and the line the watch prints. `send_message(to, message)`, `receive_messages(for)` and `ack_messages(for, ids)` carry A2A messages: to `local:<machine>/<name>` into the mailbox of the agent's person, in the `beekeeper` Postgres database `BEEKEEPER_DATABASE_URL` names (at least once, ordered per sender and receiver, 50 unacked per mailbox, kept until acked or expired with an `expired` event to the sender, a sender's `messageId` accepted once in 24 h, `LISTEN`/`NOTIFY` driving the mailbox's update); to `kagent:<installation>/<namespace>/<session>` through muster (`serve.muster`, `serve.kagent`) with the caller's token. `list_agents` and the roster name each agent by its address and state.

## [0.75.0] - 2026-10-02

### Added

- `beekeeper serve --http :8080` speaks MCP over streamable HTTP (mcp-go, the Giant Swarm line muster runs) behind muster and offers the coordination verbs as tools over the Kubernetes store: `lease_list`, `lease_claim`, `lease_release`, `hold_list`, `hold_set`, `hold_lift`, `lanes`, `lane_queue`, `lane_settle`, `lane_turn` (whether a merge is its lane's next), `note_add`, `note_list`, `note_answer`, `agents_register`, `list_agents` and `snapshot`, each with the command's text and structured content. Every request carries a Dex ID token, validated against `serve.issuer`'s keys and `serve.clientIDs`; the caller is its verified email, a member of `serve.organization`, of the team its group in `serve.teams` names, and anything else is refused with 401. Releasing another person's lease or lifting their hold is that person's or the owning team's supervisor role's (`serve.supervisors`); notes and roster entries go into the caller's team namespace; a note is answered by the person it is for or its filer. Every call leaves one Kubernetes Event on the resource it concerns (the caller's team namespace for a list) and one JSON log line with caller, tool and outcome.

### Fixed

- Two parties of different people or hosts are never the same party, whatever their names: a central lease or roster entry of one person's agent no longer passes as another's of the same name.

## [0.74.0] - 2026-10-02

### Added

- The `beekeeper.giantswarm.io/v1alpha1` resources of the coming `beekeeper serve` and a Kubernetes store over them: `Environment`, `MergeLane` and `Hold` (cluster-scoped), `Note` and `RosterEntry` (in the team's namespace `beekeeper-<team>`), each with a status subresource, `observedGeneration`, conditions, queues capped at 20, and printer columns (`kubectl get environments` shows holder, purpose, since, upgrade and queue length). The store implements `state.Store` with per-object compare-and-swap: of two concurrent claims of one Environment one wins and the other is told the holder, and an update of anything the resources do not carry is refused, never kept elsewhere. Tested against envtest (`make test-envtest`); the CRDs are generated from `pkg/apis` (`make generate-crds`).

## [0.73.2-rc.1] - 2026-10-02

### Fixed

- `beekeeper board next --claim` keeps a session's open serve: a second claim while the issue or pull request its record names is open exits 3, prints the record and changes nothing; `--claim --replace` takes the next item and replaces the record as before, and a record whose item closed, or of an agent reporting done, is replaced without it. A worker's second claim swapped its open item for a new one, and the first showed as free to every other pull.

## [0.73.0] - 2026-10-02

### Fixed

- `beekeeper free`, `snapshot`, `report`, `capacity` and the watch's SWAP line count swap as disk plus zswap: the machine reading takes `Zswap` and `Zswapped` from /proc/meminfo, and swap in use is shown as `disk <n> MiB + zswap <n> MiB in a <n> MiB pool`, the two adding up to SwapTotal − SwapFree. systemd-oomd's swap trigger and its headroom are printed only when `oomctl dump` lists a swap-monitored cgroup (its Swap Used Limit replaces the oomd.conf reading); otherwise the line says oomd watches no cgroup for swap. The SWAP check fires on disk swap over `watch.swapMaxMiB` while it grows and MemAvailable falls, OOMD IMMINENT only while oomd watches swap and disk swap grows as MemAvailable falls, and capacity's `swapGrowthMaxMiB` guard reads the same disk-swap growth. Swap held compressed in RAM, or pinned at its ceiling after a burst, read as pressure for hours with tens of GiB of RAM available.

## [0.71.1-rc.1] - 2026-10-02

### Fixed

- `beekeeper agents handover` archives the old session's desktop row once its CLI has exited, through a steward as the doctor archives a finished worker's, so only the follow-up's row carries the agent's name, also along a chain of hand-overs; a row it cannot archive at once the doctor owes and asks for again while no CLI of it runs a turn. Two rows carried one worker's name until a person archived the old one.
- A run of the supervisor or the guide a relay relieved no longer keeps its desktop CLI: once its successor's start took the role, the doctor owes the relieved run's desktop row the archive and does it once the run's CLI runs no turn, a `DOCTOR` line saying it was archived or why not; the successor's start line names the row. Relieved runs held the desktop's warm CLI slots for a day or longer, until the desktop reached its cap and started no CLI for a new worker.

## [0.71.0] - 2026-10-02

### Fixed

- A started worker whose next turn needs the desktop's browser now gets a desktop CLI within a minute of its headless turn's end. A locked screen (a running hyprlock, swaylock, gtklock or waylock) holds no link, so the import and the reopen no longer wait out 25 minutes on a window the compositor still names as focused (nor `desktop.typingQuiet`: keystrokes go to the locker). A worker started with `agents start --desktop`, or that runs `beekeeper agents desktop` before its turn ends, is imported without waiting for the window's focus at all, the person's typing holding the link 1 minute at most; `agents desktop <agent>` asks for another agent and shows an agent with neither a CLI nor a waiting reopen at once. The reopen records what it waits for and until when: `agents` shows `not running, import waits until <t>`, the watch says `IMPORT WAITS` once per wait, `lease grant` to an agent without a CLI records the grant and says that no CLI runs and whether an import is pending, and a SendMessage by name to such an agent is refused saying the same. A reopen now checks that the desktop runs the CLI its show warms, and a desktop at its cap of CLIs, which starts none for a show (`[CliGovernor] at cap=<n>; yielding warm spawn` in its log), is reported as a missed reopen naming the cap instead of a warmed CLI.

## [0.71.0-rc.1] - 2026-10-02

### Added

- `beekeeper agents keep <agent> [--until <time>] [--reason <text>]` marks a roster entry kept on purpose (a judge or reviewer session a person returns to, a spare, a worker parked on a long external wait): the doctor never takes it off the roster for staleness nor archives its desktop session. An open timer that wakes an agent by name keeps it the same way until the timer is done. `agents` shows what keeps an entry in a new KEPT column, `doctor --dry-run` names each kept entry it leaves (`would leave "<agent>" on the roster …`), and `agents keep <agent> --no-keep` returns the entry to `agents.staleAfter`. A session registering again keeps its entry's marker.

### Fixed

- `beekeeper board next` no longer offers an item served by an agent parked on purpose: the `sessions serve` record of an agent with a keep marker, or with an open timer that wakes it by name, covers its item whether its CLI runs or not, and the skip names it (`served by "<agent>" (parked, <what keeps it>)`). Without a keep the item is offered again once its session ended or reports idle.
- `beekeeper watch` says no `AGENTS STOPPED` line for an agent with a task that is kept on purpose; it repeated every arm for a worker parked on a timer.

## [0.69.4] - 2026-10-02

### Fixed

- A relay's successor whose headless first turn took the role while a person worked in the desktop's window now gets a running CLI and its watch without anyone's click: the standby watch no longer counts the start's reopen, which waits up to 25 minutes for the window to lose the focus, as a running turn, so past the `restartGrace` it resumes the holder headless (`RESUME`) and that turn keeps the role's watch. The waiting reopen yields to that turn instead of warming a second desktop CLI beside it, and every later gap without a CLI is resumed headless again until the desktop runs the holder's CLI.

## [0.69.3-rc.2] - 2026-10-02

### Fixed

- `beekeeper agents` shows in a new BROWSER column whether the desktop holds an agent's navigate to a new site for a person's site request (`asks`) or runs it without one (`skips`, Chrome permission mode `skip_all_permission_checks`), read from the agent's desktop record; `agents start` says which Chrome mode the import recorded. An imported start keeps no browser bypass: the desktop gives it `skip_all_permission_checks` only when a person allowed all browser actions, and its site requests reach no hook. The README states the mechanism.

## [0.69.3-rc.1] - 2026-10-02

### Fixed

- The model and title steward of `agents start`, `agents handover` and `agents reopen` never asks a handed-over session (a later start or another roster session carries its name): asking it had the desktop run a turn of it, and start its CLI again, beside the follow-up, so the agent's name named two CLIs.

## [0.69.1] - 2026-10-02

### Added

- `beekeeper note add --ref owner/repo#n` (repeatable) links a note to the issues and pull requests it asks about; `beekeeper watch` closes an open note for someone as overtaken (`NOTE OVERTAKEN`, `note.overtaken` with the reason) once every linked one is closed or merged. `watch --once` names the notes it would close and writes nothing. `beekeeper note done <id> --overtaken "<why>"` closes one so by hand, and `guide watch` says `GUIDE CLOSED #<id> overtaken: <reason>` once. Answered, pinned and login notes, and the note the guide asks now, are never overtaken. A note without `--ref` is never closed by the watch: once the worker session that filed it is archived (stopped is not archived), `guide watch` says `GUIDE ORPHANED #<id> for <person>, its filing session "<name>" is archived; ask it, or close it with note done <id> --overtaken: <text>` once, and the guide asks it or closes it; a role's run, current or relieved, never orphans its notes.

## [0.68.0] - 2026-10-02

### Added

- `beekeeper alerts own <installation/alertname | alertname | fingerprint> [--by <session>]` records the session that owns a firing alert (`alert.owned`); ownership ends when the owning session ends or the alert resolves. `beekeeper watch` says `PAGE UNOWNED <installation> <alertname> <where> for <duration>` for a firing alert at `alerts.pageSeverity` (default `page`), of `alerts.team` when it is set, that has had no owner for `alerts.ownerGrace` (default 15m), one line per alertname, and again every `alerts.ownerGrace` while it stays unowned; the owner's session ending starts the count again. `--notify` sends it as the new kind `page-unowned`, critical by default, so quiet hours do not hold it. Non-paging alerts never get the line, and `watch --once` writes nothing.

## [0.67.0] - 2026-10-02

### Fixed

- `guide queue` and `guide watch` no longer show long-ended sessions as waiting on the person: a session that waits on its person is listed while its CLI runs or stopped less than `guide.waitingTTL` ago (default 2h); older stopped ones fold into one line (`--full` lists them), and `guide watch` says nothing for a session that only aged out. An agent that reported its task after the wait (`agents idle`, `--done`) is not waiting, and taking an agent off the roster clears its session's wait.

## [0.67.0-rc.1] - 2026-10-02

### Added

- `beekeeper free` names the session of every Claude CLI: its title and id, its roster state (busy, parked on its person, idle), whether it is the supervisor, the guide or the spare of an open relay, and the hours since its transcript changed. A CLI whose session is untouched for `--cli-stale-hours` (default 12) with no role and not busy or parked is marked stale with the memory its exit returns; `--summary` appends the same fields to its `cli` rows. Nothing is stopped, `--apply` included.

## [0.66.1] - 2026-10-02

### Fixed

- A started agent's desktop turns run on its `--model`. Claude Desktop handles each import link twice and often keeps the record of the import that lost the transcript's title and model, so every desktop turn ran on the desktop's default. Once the import is done, `agents start` and `agents handover` (and the reopen after a first turn that deferred its import) have a steward, an idle desktop CLI of another session beekeeper started, set the dropped title and the model the first turn ran on through the desktop's session tools, and say so.

## [0.65.1] - 2026-10-01

### Fixed

- The Secret guard reads a kubectl sink's verb past its global flags, so `… | kubectl --context x apply -f -` passes as the consumer it is; the context's value was taken for the verb and the pipeline refused.

## [0.65.0] - 2026-10-01

### Changed

- The Secret guard is an allow list for credential tools. `sops` and `op` are refused in every form (encryption, `op read` and `op item list` included; `helm secrets`, `age -d` and `gpg --decrypt` too) but `op run -- <command>`, whose output op masks, and whose command is checked as a command of its own, until `beekeeper secret` ships, and so is `vault` beyond its listing and metadata commands. A Secret read, `vault kv get`, `base64 -d` of a secret's data or a render fed with secret values (`helm template -f secrets.yaml`, `kustomize build --enable-alpha-plugins`) passes only into key names, metadata or a length, a consumer that prints nothing, or, for a render, a filter that blanks the Secret data, so a diff of blanked renders passes. A file, `tee`, a variable, a hash, `grep -c|-q` and the clipboard are refused as ends, and a hash or diff of a plaintext secret file is refused. `kubectl edit secret`, `kubectl get --raw …/secrets`, `kubectl create secret … -o yaml` and templates naming annotations are refused; an unknown shape is refused by default. Every refusal names `beekeeper secret compare|fingerprint|set|copy|rotate` and no longer offers plaintext files or unkeyed hashes. The kube guard's own `op item get` refusal is gone: the Secret guard covers all of op.

## [0.64.0-rc.1] - 2026-10-01

### Added

- The transcript value scanner. `beekeeper hook posttooluse`, the PostToolUse hook `install` now registers for every tool, replaces a tool result that carries a secret value before the model sees it: each indexed value (keyed fingerprints of the SOPS files in `scan.sops` and the 1Password vaults in `scan.vaults`, built by `beekeeper scan index`, or one value by `beekeeper scan add <ref>`) and each token pattern of the outbound guard becomes `[redacted: <reference or rule>]`, the session goes on, and an indexed reference gets a rotation note. Claude Code writes the redacted result to the transcript on disk too. `beekeeper scan sweep` counts each reference and token rule in every existing transcript, never printing a value.

## [0.63.2] - 2026-10-01

### Fixed

- `watch` says `WATCH STALE` once for each running watch, itself included, whose binary `self-update` replaced, naming the version it runs, the one installed and that a re-arm picks it up; `watch --once` says it for the running watches too. A watch kept the code it started with after an install, silently, until something restarted it, so a fix in its poll (the doctor's chores) reached the machine only then. No watch re-executes itself.

## [0.63.1] - 2026-10-01

### Fixed

- The Teleport keeper retries a login once when the proxy's SSO callback exchange timed out after the browser's sign-in succeeded, before it records the failure and leaves the sign-in note; the first attempt is logged as `teleport.retry` and both attempts' output stays in `teleport/login.log`. Any other failure, or a failed retry, fails as before. A night-time renewal that hit this timeout left a sign-in note in the morning while a single fresh attempt would have renewed the login.

## [0.63.0] - 2026-10-01

### Fixed

- `board next` holds a sub-issue that is a board item to the order by its own Status whatever its Team: the Status, Kind and project of an epic's sub-issues are read with them, so a sub-issue without a Team (missing from the team's board read) in Inbox, too old for its step or still blocked is skipped with the reason instead of offered.

## [0.63.0-rc.1] - 2026-10-01

### Added

- `ui` takes a Claude Code session over from its pane (`t`): its permission requests come to the screen with its turns beside them, `a` allows and `d` denies the oldest, and the session's row says `approval`. The PermissionRequest hook holds a taken-over session's request for the screen and gives up itself after 290 s; unanswered, released (`t` again, or quitting the screen) or with the screen gone, the request goes to the session's own window and the pane says it was handed back. A session not taken over is decided at once without a lock, as before. `install` writes the hook's timeout as 300 s; omp sessions ask for no approvals and are refused.

### Fixed

- `ui` keeps the sessions tab's selection on the picked session when the list reorders between refreshes; `enter` could open whichever session had taken its row.

## [0.62.1] - 2026-10-01

### Fixed

- `board next` holds an epic's sub-issues to the epic's owners: a serve record, a busy agent's task or an open note naming the epic skips each sub-issue offered through it, with the reason and the epic (`note #n (waits on …), on epic owner/repo#n`). One note or serve on an epic covered none of its sub-issues, so a board pull was offered a teammate's slices one after another.

## [0.62.0] - 2026-10-01

### Added

- `ui` messages a session from its pane: `m` opens a message line, `enter` sends it stamped `From <guide.person> through beekeeper ui:`, by name to a Claude Code session's CLI (at its next tool call or as its next turn) or into the inbox of an omp agent beekeeper started; an omp session the person runs in its own terminal, and a name two CLIs share, are refused with the reason on the line. Each message is a `ui.message` event.

## [0.61.0] - 2026-10-01

### Added

- `ui` follows a session live: its pane re-reads the transcript at every refresh and shows the history, then the current turn with its tool calls, newest at the bottom; `k`/`j` scroll back and forward and hold their place while turns arrive, `G` follows again. The sessions table has a `state` column (busy, idle, waiting on its person, an omp session's own), and an omp session's pane reads omp's session file. The pane showed the last ten turns once, without tool calls, and an omp session's not at all.

## [0.60.4] - 2026-10-01

### Fixed

- An omp agent's beekeeper commands act as its roster entry, so `agents idle --done` finishes it; they were refused as an unregistered session. Taking an omp agent off the roster stops its unit: its process ran on, holding its model, after its work was done.

## [0.60.4-rc.1] - 2026-10-01

### Changed

- `agents start --harness omp` names the agent's model on omp's command line: `--model`, else `omp.model`, an exact `provider/id` selector `omp models` lists. With neither, or a model omp does not list, the start is refused before anything is recorded. An omp agent started without a model ran on omp's own default, which answered nothing while its server was busy with other work, and a pattern could match a model nobody meant.
- `agents` and `sessions` have a `MODEL` column; an omp session's model comes from its command line until its session file records one, so an agent not yet answered shows its model too.

## [0.60.3] - 2026-10-01

### Fixed

- `board next` holds a sub-issue reached through an epic to the order like any board item, by its own Status: one no step offers on its own (a Backlog item older than the Backlog step's `createdWithin`, a blocked one, one in Inbox) is skipped with the reason instead of being picked as the epic's remainder, and any item with an open recorded blocker is skipped in every step. `--claim` documents and prints that it leaves the item's Status alone: the caller moves it with `board move` once it judged the item, and gives the claim back with `sessions unserve <owner/repo#n>`, which now takes the issue as well as the session. Board pulls were picking a 99-day-old Backlog sub-issue of an epic in progress, which the Backlog step's 90 days ruled out, and sub-issues still blocked by an open issue.

## [0.60.2] - 2026-10-01

### Fixed

- A desktop notification sent after the session bus closed the connection is no longer lost: the notifier replaces a connection that is no longer connected before it calls, and retries once whenever a failed call left the connection closed. A call on a closed connection could fail with godbus's read error instead of `dbus.ErrClosed`, which skipped the retry; `TestDesktopKeepsItsConnection` failed about 1 in 12 runs under `go test -race` on a loaded machine.

## [0.60.1] - 2026-10-01

### Fixed

- The watch's loop tests (`TestWatchSamplesTheMachineWhileALaneReadHangs`, `TestWatchSlowsInstallationReadsUnderCPUPressure`, `TestLoopSkipsMissedTicks`) run on a clock they step: the watch's loops wait on an injectable clock, the wall clock outside the tests. Under `go test -race` on a machine at load 8 to 10 they failed on the wall-clock time a starved poll took.

## [0.60.0] - 2026-10-01

### Fixed

- Taking an omp agent off the roster (`agents remove`, the doctor) removes its inbox FIFO and lock file, also while its process still runs (no wake reaches an agent off the roster); they stayed in the state folder, one pair per agent ever started.

## [0.60.0-rc.1] - 2026-10-01

### Added

- `agents start` gives every worker the worker rules beekeeper ships with its role skills (the `worker-rules` skill, under the binary's version) ahead of its brief, and `agents handover` puts them ahead of the follow-up's prompt again: a brief carries only its task. A `SendMessage` to `the supervisor` or `the guide` goes to the session holding that role now, by the name its running CLI answers to, else its desktop session, and is refused while nobody holds it: a brief no longer names a run that the next relay makes stale. Workers started with 10 to 17k characters of brief, two disagreeing copies of the rules and the boilerplate of every task file repeated in each.
- `lint briefs <file or folder>...` refuses what makes a skill or brief go stale: a dated line, an "until X ships" clause, a note on the release that fixed something (`(v0.53.0: …)`, `since v…`), a workaround and a role's run number, one `path:line: rule: why` per finding, exit 3 on any. The shipped skills pass it in CI.
- `hook sessionstart`, which `install` registers, writes the agent shell's prelude into the session's environment file, sourced before each Bash command is parsed: the aliases and shell functions of `agents.shell.unalias` (default `grep`, `find`, `ls`, `cp`, `mv`, `rm`) are removed, so each name runs the tool on `PATH`, and an unmatched glob stays as written (`agents.shell.globs: literal`, the default) instead of failing the command. The person's aliases, the harness's tool shadows and zsh's `nomatch` caused 32 shell errors in agent sessions in one week.

## [0.59.0] - 2026-10-01

### Added

- omp (oh-my-pi) is beekeeper's second local harness: every running omp session shows in `sessions`, `snapshot` and `free` with harness `omp`, its state (busy, idle), title, model, checkout, memory, tool commands and last hour from its session file, and `tail` follows it; `sessions --all` adds the omp sessions of the last 24 hours no process runs. `agents start --harness omp [--model m]` (`omp.model`, `omp.sessionsDir`) starts `omp --mode rpc` in a transient unit with its stdin on a FIFO inbox, registered on the roster as `omp_<id>`; the brief and every `agents wake` reach it as an rpc `steer` command, at its next tool round or as its next turn, and a wake of an ended omp agent is refused. omp agents on a locally served model were invisible to the supervisor's capacity and the person's views, and could not be started or messaged.

## [0.58.0] - 2026-10-01

### Added

- `beekeeper ui` opens the person's screen: six tabs over the live state of the machine — the watching tab (load, RAM, swap, memory pressure, the desktop scope, tmpfs and disk, build slots, kind clusters, the commands sessions sit on and the OOM kills of the last hour), the sessions (idle age, memory, context fill, the last hour's turns and errors and cost, the command each runs, what it waits on its person for — a waiting session stands out, and `enter` reads its last turns from the transcript), the sharing (held leases with how stale each holder is, free resources, the grant queues, the holds and the merge lanes), the supervising (the supervisor and guide roles with their live/restarting/gone state, context against the relay point, an open relay and the spare, the agents with their tasks, the open notes with their defaults and deadlines, the timers and the session records), the alerts (each installation's set by severity from the watch's baseline, the running upgrades, the GitHub budget against its floor and who is drawing on it) and the event log, newest first. It refreshes every two seconds; the GitHub budget and the installations' upgrades are re-read at most every minute, in the background, so the screen never waits on the network. It reads only: it takes no lease and lifts no hold — those stay with the commands, whose exit codes the sessions gate on. `q` quits, `r` refreshes, `1`–`6` and tab switch, `j`/`k` move.

## [0.58.0-rc.1] - 2026-10-01

### Added

- `note add --for <guide.person>` refuses a note that links an open pull request of a plans repository (`plans.repositories`) while that pull request's stage check (`plans.check`, default `plan-stages`) is red, pending or missing on its head, naming what the check annotated (the missing ground-truth report, contrarian verdict or website); once the check is green, the note is accepted. Notes about other repositories, merged or closed plan pull requests and memos are unaffected, and with `plans.repositories` unset nothing is read. Plan revisions were sent for approval without some of their stage outputs.

## [0.56.1-rc.1] - 2026-10-01

### Fixed

- A role's successor comes up instead of being replaced in a loop. It starts in `supervisor.dir` (`guide.dir`), else in the folder its predecessor's desktop session started from, not the desktop's worktree of it: the desktop could not warm a session there (the worktree's branch is checked out already), so no successor got a desktop CLI and each was declared gone within the grace, nine in 33 minutes. The resume goes to the desktop CLI, never to the headless first turn. A successor with no desktop CLI after its first turn (the import waited out the focus overnight, nine successors 25 minutes apart) is resumed headless (`agents wake`) and keeps the watch there; one that still goes down is one note, and the next successors start 5 and 10 minutes later, none after three.

## [0.56.0] - 2026-10-01

### Added

- Conditional timers: `timer add <time> <what> --when "<condition>"` (`pr-merged owner/repo#n`, `issue-closed owner/repo#n`, `helmrelease-ready context/namespace/name`, `controlplane-ready context/namespace/name`) or `--probe "<command>"` waits on the condition from its time; the watch checks it every `--every` (5m), each distinct check once per tick however many timers share it and a GitHub one only above the budget floor, and fires the timer within one tick of it holding. `--until` fires it as timed out, `--expire` closes it unfired instead. `--wake <agent>` wakes the agent with the timer's text, `--run "<command>"` runs a command and logs its exit code (`timer.ran`); such a timer closes when it fires. In one week about three of four timers stood in for a condition: a session woke, found it unmet and set the timer again.

## [0.56.0-rc.1] - 2026-10-01

### Added

- `board next [--claim] [--waits <what>]` picks the next free item of the project board (`board.owner`, `board.project`, `board.team`) by the configured order (`board.order`: steps by Status, Kind and labels, the open sub-issues of an epic, blocked items whose blockers all closed, items created within a window, a GitHub search), skipping what a running session serves (a `sessions serve` record, a busy agent's task), what an open note waits on, what is assigned outside `board.people` and what had no activity for `board.staleAfter` (a year); it prints the item, why it is picked and why each item above it was skipped, from one read of the board. `--claim` records the pick as the calling session's serve under the state lock, so two concurrent claims never get the same item; the claim ends with the session, its agent's `agents idle` or `sessions unserve`. `board move <issue> <status>` sets an item's Status by the board's canonical name or an unambiguous part of it and refuses anything else with the board's values. A board pull read the whole board and the roster by hand from a brief of pick instructions, a median of 22 tool calls, and two pulls started together took the same item.

## [0.55.0] - 2026-10-01

### Added

- The outbound secret guard: the PreToolUse hook scans what a call sends off the machine (the command line of `gh`, `devctl`, `git commit|tag|remote`, an HTTP client's body, the files these send, the commits a `git push` publishes, every connector tool's input, a Write or Edit under `outbound.paths`) for gitleaks' token patterns and `outbound.phrases`, and refuses a hit naming the rule, never the match; `gitleaks:allow` exempts a line. `outbound.storeDeny` refuses matching `op` and `vault` store writes. The watch says `EXPOSED <path>` for a world-readable private key or a git remote URL with a credential under `outbound.sweepRoots`. The hook's matcher gains `|mcp__.*`. The guards so far looked only at what a command reads; in one week the leaks went out through issue bodies, commits, posts and remote URLs.

## [0.54.0] - 2026-10-01

### Added

- `report [--since 1h|15:04] [--until 15:04] [--tz zone]` renders the status report from beekeeper's own state, as Markdown that passes `reporter check` by construction: the merges and promotions of the window (title, release, rollout), the running work with its issue, waits and leases, what waits on `reporter.person` (notes per owner, waiting sessions, the open drafts of `reporter.reviews`), the merge lanes, timers and holds, and the machine. The scheduled reporter's prompt has its session run it and post the output: a report took 26 tool calls, six minutes and up to 128k tokens of context, and was refused by `reporter check` in more than half the runs. `reporter.tz` sets the zone of the report, `reporter check` and the post hook.

## [0.53.0] - 2026-10-01

### Added

- `doctor` (with `--dry-run`, `--fault <name>`), also run by every watch poll in the background (`DOCTOR` lines): takes an agent off the roster once it reported its work finished (`agents idle --done`) and its CLI runs no turn, once it was relieved of a role, or once it stayed idle `agents.staleAfter` (24h) with no CLI running, and archives the desktop sessions beekeeper started for the finished and stale ones in one steward's turn; gives a started session whose desktop title was dropped its roster name back; probes the known faults of `doctor.faults` and runs a failing one's remedy when it may run unattended, any other fault still failing being one note for `guide.person` that closes once its probe passes. Archiving a finished worker, restoring titles and restarting a known unit were notes for the person, a third of a week's notes, and the roster carried forty finished workers.
- `agents idle --done` marks the work finished; `log add <text>` logs a status line (verb `status`).

### Changed

- `note add --for <guide.person>` refuses a note that asks nothing (no question mark, no `--option`, no request verb opening it), pointing to `beekeeper log add`.
- A relieved role holder gets no `HANDOVER DUE`, and a session named as a role's run counts as one that held the role also once its relief is forgotten: it is never archived or used as a steward.
- `agents remove` archives through the doctor's batch request, one steward turn for any number of sessions.

## [0.51.0] - 2026-10-01

### Added

- `install` and `uninstall`, both with `--dry-run`: install merges the PreToolUse and PermissionRequest hooks into Claude Code's user settings (keys, order and layout kept), writes and starts the standby service (`beekeeper-notify.service` as a systemd user unit, a launch agent on macOS), on systemd the memory guard sized to the machine's RAM (`memcap.slice`, a drop-in named after the Claude Desktop scope that runs), and a starter config (the example with every key commented out) where none exists, all with the absolute path of the binary (`--binary` names another). It writes what is missing and updates what an earlier install wrote; a file or hook in place that it did not write it keeps, so a second run changes nothing. `install.json` in the state directory records what it wrote; uninstall stops the service and removes exactly that, and a file changed since stays; config and state stay unless `--purge`. Setting a machine up was a manual series of steps, each easy to get wrong, and nothing took them away again.

## [0.50.0] - 2026-10-01

### Added

- `note add --for <guide.person>` refuses a note, naming what it lacks in one line (exit 2), without `--status-quo` and `--why`, with a `--default` that is no action (`wait`, `none`, empty), an `--option` without `: <consequence>`, a `#N` or `owner/repo#N` without its full URL, or a claim of merged, green, released, rolled or closed without `--checked "<source>"`. Such a note on an issue or PR an open note for the same person names, asking the same verb (the first word), folds into that note (`note.folded`). `--kind login --until "<probe>"`: the watch runs the probe every tick and closes the note once it exits 0 (`NOTE CLOSED`). Notes without `--for`, and with `guide.person` set notes for another name, are unchecked. A third of a week's notes made the person ask for links or context, or were answered already.

## [0.45.1] - 2026-09-30

### Fixed

- The merge gate settles a lane at once when no installation range admits the merge's release: it reads each HelmRelease's OCIRepository (`semver`, or a `tag` as the one version it follows) or chart template `version`, and says `<installation> does not follow 4.105.0-rc.3: flux-giantswarm/agent-platform follows semver >=4.0.0 <5.0.0`; a wait names the range (`… rolling to 4.105.0 (semver >=4.0.0 <5.0.0)`). A merge that cut only a release candidate under a stable range held its lane until the settle timeout and a `lanes clear` by hand.

## [0.43.0] - 2026-09-30

### Added

- `ps [name|pid...]`: the process table (PID, parent, age, CPU, memory) with every command line masked like the `STACKED` line, a masked line marked `(masked)`. The PreToolUse hook refuses the reads that print whole command lines or environments, as Secret reads naming `beekeeper ps`: `ps` with its args column (`ps aux`, `ps -ef`, `-o args`), `pgrep -a`, `pstree -a`, `/proc/<pid>/cmdline` and `environ`, `docker|podman inspect` without a narrow `--format`, `docker ps --no-trunc`. A process started with `-e PASSWORD=…`, `--token …` or `-p …` showed the values to every agent debugging load.

### Changed

- `ps` is no alias of `sessions` any more: it is the masked process table.

### Fixed

- A printed command line (`STACKED`, `ps`) leaves out what follows a short option's letter (`-psecret`), and masks a program that rewrote its argv into one line (Electron, setproctitle), which it printed whole.

## [0.42.0] - 2026-09-30

### Added

- `watch` says `PROCESS STORM: N forks/s (usual M/s), top: kubectl 40 %, …; sessions: "…" 70 %` once two samples in a row read a fork rate (the `processes` counter of `/proc/stat`) more than `watch.forkRateMax` (50/s) over the machine's usual rate (the last 10 minutes' average outside a storm; a machine with 35 sessions forks 70–130 a second at rest), naming the commands and sessions of the processes started since the last sample that still run at it; a Go program's pidfd probe child counts as its parent. It says `STACKED N × <command>: oldest …, parent …, session "…"` for more than `watch.stackMax` (3) copies of one command line from the same place in the process tree, each running over a minute; one MCP server under each CLI and interactive shells are none. Both end with an `ENDED` line; a negative threshold turns its line off. A printed command line keeps the program, its subcommands and flag names, never a value. A fork storm of 2,025 `kubectl`, 1,168 `helm` and 655 `tsh` in ten minutes and seven stacked `beekeeper free --summary` had no line.

## [0.41.0] - 2026-09-30

### Changed

- A role's runs are numbered: every holder is "Supervisor run N" or "Guide run N", N one above the highest run recorded. `supervisor relay` and `guide relay` take no session any more: they start the next run as a fresh session (as `agents start` does, so its desktop title, roster name and messaging name are one) and open the relay to it; its first turn takes the role with `<role> start` and ends, and the standby watch resumes it in its desktop CLI (`RESUME`). A successor that does not start withdraws its relay. After a crash the standby watch starts the next run the same way, for the guide too (`GUIDE GONE`, `SUCCESSOR`), once per gone holder; a session between beekeeper's start or wake and its desktop CLI is not gone. A start in a session not named as its run records the next run, and a steward sets its desktop title to it; `supervisor status` and `guide status` name the run. A desktop title set later does not change the name a running CLI takes messages under, so a renamed role session was unreachable by its new name.

### Removed

- The spare: `supervisor spare`, its record, the standby watch's keep-awake and `supervisor.keepAwake`. Relays and crashes start fresh sessions instead of repurposing one kept awake.

### Fixed

- A wake of a roster entry known by its CLI session id alone, whose session the desktop imported, gets the reopen after its turn: the supervisor's session ran headless for 45 minutes and never showed in the desktop, and opening it by hand started a second CLI on the session.

## [0.40.4] - 2026-09-30

### Fixed

- A desktop steward that declines a title or archive request (a model, which can refuse a request from another session, and did: an idle roster agent whose brief forbade archiving) no longer fails the request: it goes to the next steward, three at most, and a finished worker off the roster is asked before an idle roster agent. The request says it comes from beekeeper on the operator's behalf and that archiving is reversible.

## [0.40.3] - 2026-09-30

### Changed

- `agents remove` archives the desktop session beekeeper started for the agent, unless it runs a turn or holds a role (`--keep-desktop` keeps it): an idle desktop CLI of another session beekeeper started calls the desktop's `archive_session` on it. Finished workers piled up in the sidebar.
- `agents reopen` restores a start's lost desktop title through any idle desktop CLI of a session beekeeper started, not only the session's own: the desktop starts no CLI for an import that lost its title and model, so the session could not retitle itself, and dozens of workers a day showed as untitled "General coding session" rows.

## [0.40.2] - 2026-09-30

### Fixed

- The hook's model-server guard refuses an HTTP request to the model server only when an HTTP client sends it: at a command position, or as the command a `kubectl exec … --` or `docker exec <container>` runs. The endpoint quoted in another command's argument (an issue comment's body) was refused as a load.

## [0.40.1] - 2026-09-30

### Fixed

- `snapshot` and `watch` no longer name a model's loader while the request that loaded it still runs: the journal logs a request when it ends, and the load took the client of the same model's earlier load, so a host request under the `model-server` lease read as a lab's and the watch unloaded it.

## [0.40.0] - 2026-09-30

### Added

- The host's model server is the leasable resource `model-server`, claimed like a lab with a budget: `lease claim model-server -p "<purpose>" --gib <n>` (default `ollama.budgetGiB` 12, at most `ollama.maxBudgetGiB` 24); `lease list` and the hand-over show it. `watch` prints `MODEL SERVER` for a loaded model no lease covers: loaded while nobody holds `model-server`, by a lab another session holds (the client's kind node maps to its lab and the lab's holder), or beyond the holder's budget. It unloads such a model (`keep_alive: 0`, the weights stay on disk) unless `ollama.nameOnly`; a load from the host without a lease is only named. The PreToolUse hook refuses, outside the session holding `model-server`, `ollama run|pull|create|push|cp`, an HTTP request to the model server's port on a generate, chat, embed, pull or `/v1/` path, and the agentlab tests in `ollama.labTests` (default `models-test`).

## [0.39.0] - 2026-09-30

### Added

- `watch` says CPU starvation before the machine saturates: `CPU PRESSURE: some avg10 N% over 40%` once two samples in a row read `/proc/pressure/cpu` over `watch.cpuPSIMax`, and `LOAD RISING: 1m N, 5m M (C cores)` once the 1-minute load passes one per core at more than twice the 5-minute load, each with an `ENDED` line. These and `HIGH LOAD` name the five commands that burned the most CPU since the last sample (`; top CPU over 30s: go 11.8 cores (8), …`). `snapshot` prints the cores, the HIGH LOAD threshold and CPU PSI some avg10. A load climb from 8 to 300 took two minutes, and the only line, `HIGH LOAD`, came when the desktop already lagged.

### Changed

- `HIGH LOAD` fires over `watch.loadPerCoreMax` (1.5) × cores, 36 on 24 cores, instead of a fixed 45; a configured `watch.loadMax` still replaces it.

## [0.38.0] - 2026-09-30

### Added

- `snapshot` and `watch` count the iGPU's GTT: system RAM the amdgpu driver pins for the GPU, which no cgroup counts. `snapshot` prints `iGPU GTT <used> of <total> MiB` with the host ollama's loaded models (`/api/ps`) and, where ollama's journal shows it, the client whose request loaded each one (a kind node by name). `watch` prints `IGPU GTT` above `watch.gttMaxMiB` (24 GiB) and names GTT and the models as the cause in its `LOW RAM`, `SWAP` and `OOMD IMMINENT` lines: a lab's request loaded a 30B model next to a small one, GTT reached 42 GiB and systemd-oomd killed a desktop scope while every cgroup looked modest. New settings `ollama.url` and `ollama.unit`.

## [0.37.6] - 2026-09-30

### Fixed

- A reopen right after its start's import waits up to 5s for the start's switch back before it notes where the desktop was: a first turn that ended within a second left the desktop on the new session.
- `watch` notifies no `OOMD IMMINENT` while a `swapoff` runs: the kernel takes the device out of SwapTotal before it drains its pages, so swap read 100 % full (`SWAP 1690 of 1690 MiB used`) while it was being emptied. The watch says `SWAPOFF IN PROGRESS` to the supervisor instead.

## [0.37.5] - 2026-09-30

### Changed

- `agents start`, and the reopen after a start's or a wake's turn, open their `claude://` links only while Claude Desktop's window is without the focus (Hyprland's active window): each link switched the desktop's main window to another session and back under the person typing there. A start whose import waits out 2 minutes leaves it to the reopen after its first turn, which imports the session and waits up to 25 minutes.
- The PreToolUse hook refuses a command that opens a page in the person's browser (`muster auth login`, `gh auth login --web`, `xdg-open`) outside the session holding the `browser` lease: a worker's sign-in tab opened in front of the person, who closed it as a stray pop-up.

## [0.37.4] - 2026-09-30

### Changed

- `watch --notify` sends a memory notification (`oom-line`) only when a systemd-oomd swap kill is imminent: less than `watch.oomdHeadroomMinMiB` (1024) of swap growth left before oomd's `SwapUsedLimit`, read from `oomd.conf` and its drop-ins, or the trigger reached within `watch.oomdWithin` (30m) at the last hour's growth rate. Low RAM, swap over `watch.swapMaxMiB`, memory pressure and the desktop scope's anonymous memory stay watch lines for the supervisor. Cold pages parked in swap on a quiet machine notified the person every 30 minutes.
- The watch's `SWAP` line says the distance to systemd-oomd's trigger and the growth rate over the last hour, e.g. `SWAP: 10627 of 16383 MiB used, 4117 MiB before systemd-oomd's 90 % trigger, +250 MiB/h over the last hour, trigger in 16h28m0s`. `beekeeper free` reads the trigger from `oomd.conf` too instead of assuming 90 %.

## [0.37.3] - 2026-09-30

### Fixed

- `events.jsonl` survives an unclean shutdown. An append lost to a power cut leaves its range as NUL bytes, and the next event landed on the same line behind them, so every reader dropped that event too. Readers now skip a line's leading NUL bytes, and an append starts a new line when the file does not end in one. An existing file is repaired once with `tr -d '\000'` while no beekeeper writes.

## [0.37.2] - 2026-09-30

### Fixed

- A merge lane settles once its HelmReleases are Ready on the merge's release or a later one in semver order. A merge cuts a release candidate `X.Y.Z-rc.N`, and an installation on a stable range runs its promotion `X.Y.Z`, never the candidate, so the lane never settled and the next merge was queued behind an installation already Ready on the promoted release.

## [0.37.1] - 2026-09-30

### Fixed

- `beekeeper watch --notify` keeps its desktop bus connection between notifications. The connection was opened with the first send's context, and godbus closed it once that send returned, so every other notification was lost to `dbus: connection closed by user`, and the watch alternated between `NOTIFY unavailable` and `answers again`. A connection the bus closes underneath is now replaced within the send.
- `beekeeper agents wake` runs every wake in a unit of its own, `beekeeper-wake-<id>-<wake>`. An earlier wake's unit stays loaded while a process its turn started runs on, and systemd refused the next wake under the same name (`Unit … was already loaded or has a fragment file`).

## [0.37.0] - 2026-09-30

### Changed

- `watch` holds back other teams' and team-less `notify` alerts by default once `alerts.team` is set (`{severity: notify}` in the default `alerts.quiet`): logged as `watch.quiet`, while every page, the team's alerts and an installation in play still wake.

## [0.36.0] - 2026-09-29

### Added

- `beekeeper watch` holds back what is noise for the supervisor: other teams' alerts matching `alerts.quiet` (default: their e2e test clusters, `t-*`), an alert back after a reading that missed it with its old start, and the start and end of the sessions in `watch.quietSessions` (default: `test: *`). The team's alerts, an installation in play (leased, claimed or merged into) and pages outside a rule naming a cluster always wake. What is held back is logged as `watch.quiet` and counted in `beekeeper snapshot`.

## [0.35.0] - 2026-09-29

### Added

- `beekeeper free --summary` runs one scan at a time, at nice 10, bounded by `--timeout` (default 60s). A second caller waits for the running scan and prints its rows instead of starting another, and a caller within 30 s of a finished scan with the same options reuses its rows under a `cached <age s>` row. On timeout it prints what the scan has collected so far and a `partial <timeout s>` row. A front end clicked again and again on a starved machine no longer stacks full scans.

## [0.34.1] - 2026-09-29

### Fixed

- `events.jsonl` writes every event's `at` in UTC. `run.start`, `run.end` and `hook.allow` carried the local offset while every other verb was in UTC, so a reader comparing `at` as a string put hours of events in the wrong place. `beekeeper log` still prints local times.

## [0.34.0] - 2026-09-28

### Added

- The PreToolUse hook refuses a kubectl plugin's write to a `gazelle` cluster: `kubectl <plugin> …` and `kubectl-<plugin>` binaries such as `kubectl ate delete actor` or `kubectl-ate admin make-ca-pool`, by the same targets as kubectl, through the same shells, prefixes, variables and functions. Plugin subcommands that read (`get`, `logs`, `top`, …) and read-only plugins (`tree`, `resource-capacity`, …) pass.

## [0.33.0] - 2026-09-28

### Added

- The PreToolUse hook refuses a kube context switch (`kubectl config use-context`, `set current-context`, `kubectx`, `kubectl gs login` without `--self-contained`, and `tsh kube login`, `tsh login --kube-cluster`, `kind create cluster` or `kind export kubeconfig` into the machine kubeconfig), a write to a `gazelle` cluster through `kubectl`, `helm` or `flux` (by `--context`, `--kube-context`, `--cluster` or the current context of the kubeconfig in use), and `op item get` in every form. Reads and writes to other clusters pass. `beekeeper snapshot` names a current context the machine kubeconfig has.

## [0.32.0] - 2026-09-28

### Added

- The PreToolUse hook gives a session its first write's repository's instructions: on the first `Edit`, `Write`, `NotebookEdit` or `git commit` in a git repository other than `$CLAUDE_PROJECT_DIR`, the call carries that repository's `CLAUDE.md`, `AGENTS.md`, `.claude/rules`, the hooks and permissions of `.claude/settings.json` and the files they name as mandatory reading as `additionalContext`, once per session and repository, at most 10,000 bytes, never a secret's file, never refusing the call. The hook's matcher becomes `Bash|Edit|Write|NotebookEdit|AskUserQuestion|SendMessage`.

## [0.31.3] - 2026-09-28

### Fixed

- A lane hold that excepts one pull request (`hold set --lane <l> --except <owner/repo>#<n>`) is that merge's fix window: the gate runs it without the lane's HelmReleases Ready or the previous release rolled, logged as `merge.window`. The fix of a broken rollout waited on a Ready only it could restore, then was refused.

## [0.31.2] - 2026-09-28

### Fixed

- The gate merges a repository devctl does not serve (`merge.devctlOwners`, default `giantswarm`) with a plain squash merge as the gh login, in the merge's unit as for devctl: it waits for the head's checks, refuses a draft, closed, conflicting or another person's pull request, and squash-merges with the head as the expected one and `<title> (#<n>)` as the subject, then deletes the branch. devctl's GitHub App login reaches the giantswarm organisation and public repositories only, so every merge of a private teemow repository ended in exit 7 and a failed `beekeeper-merge-*` unit, and each retry in another.
- Each gated merge's stderr and document are kept in `<state>/merge-output/<repo>-<n>-<time>.log` for 7 days, named in its `merged`/`merge.failed` event; the run's own files no longer take devctl's reason with them. After exit 7 or 8 the gate says the same command fails the same way until the reason is fixed.
- `agents reopen`, a started or woken session's `ExecStopPost`, no longer fails its unit when the desktop did not show the session or keep its title: the miss is an `agent.reopen` event.

## [0.31.1] - 2026-09-27

### Fixed

- `agents handover` asks for the note in one call again: the relay turn is told that the target's name is an opaque title and is run again, three turns at most, when it called no SendMessage, which it did for names that read like a task. An idle agent is handed over with the last task it reported idle on, as its prompt's Task line and its follow-up's roster task; a repeated `agents idle` and a re-register no longer erase that last task.

## [0.31.0] - 2026-09-27

### Added

- `beekeeper agents wake <agent> "<message>"` wakes and messages a session past Claude Desktop's cap on `local_` sends. A running CLI gets the message by name. A stopped session is resumed headless (`claude -p --resume <id>` in its directory, mode and model, unit `beekeeper-wake-<id>`), then reopened in the desktop. `agents` shows `live, first turn running` and `live, wake turn running`.
- The PreToolUse hook sends a `SendMessage` to a `local_` id whose session has a running CLI to that CLI by name, so a desktop send during a first turn or a wake starts no second copy of the session and spends none of the desktop's cap. A name two running CLIs carry is refused. The hook's matcher becomes `Bash|AskUserQuestion|SendMessage`.

## [0.30.2] - 2026-09-27

### Fixed

- The Secret guard takes the null-safe keys form `(.data // {})|keys` as keys only.

## [0.30.1] - 2026-09-27

### Fixed

- `watch --notify` sends the critical "the machine is near its OOM line" notification only while a threshold is crossed. It sent one every `notify.repeat` whatever the machine's state, with the body `beekeeper snapshot` and no condition. The desktop scope's `memory.current` no longer counts toward the OOM line, because it is mostly reclaimable page cache under the scope's `memory.high`. The scope's anonymous memory (`watch.scopeAnonMaxMiB`) is the line, and `watch.scopeMaxMiB` is gone.
- A gated merge's `beekeeper-merge-*` unit no longer fails when devctl merged or declined the pull request (exit 0–6, 9: not applicable, refused, red, timed out). The gate hands that outcome to the calling session and the event log. The unit fails only where a person must act: devctl's usage or tooling failure (7), its authentication (8), a signal, or the merge child itself.
- A session beekeeper stops with SIGTERM (a reporter past its timeout, a stopped agent) ends its unit successfully instead of as a failed unit.

## [0.30.0] - 2026-09-27

### Added

- The PreToolUse hook refuses `AskUserQuestion` in every session but the guide's, telling the agent to file `beekeeper note add --for <person> … --default …` and carry on; register the hook with the matcher `Bash|AskUserQuestion`.

## [0.29.1] - 2026-09-27

### Fixed

- The Secret guard lets a jq slice or index (`.[:3]`, `.[0]`) through after a keys-only filter; `.[]` and `.["key"]` on the object stay refused.

## [0.29.0] - 2026-09-27

### Added

- The PreToolUse hook refuses a Bash command that would print secret values into the transcript: `kubectl get secret -o yaml|json`, templates over a Secret's data, `kubectl view-secret`, `sops -d`, `op read`, `op item get --reveal`, `vault kv get` and `base64 -d` of `.data`, also through kubectl wrappers and inside shell `-c` strings. Keys, metadata, hashes, files, variables and consumers such as `kubectl apply -f -` pass; the refusal names the safe forms.

## [0.28.1] - 2026-09-27

### Fixed

- A started session keeps its name as its desktop title after its first turn: the desktop handles each resume link twice and, when both deliveries import, keeps an untitled record (default name `<dir>-<n>` in ListAgents). `agents reopen` now checks the desktop's record once the first turn ended and, when the name is missing, has the session set it with the desktop's `set_session_title` through its desktop CLI's socket, waiting for the desktop to record it; the unit's `TimeoutStopSec` is 5 minutes for that.

## [0.28.0] - 2026-09-27

### Added

- `beekeeper reporter final <time>` schedules one last report outside the slots (covering the time since the last one) and pauses the schedule with its start; `reporter pause` pauses at once and `reporter resume` ends the pause, the current slot's report starting at the next poll. The state's `reportPause` field is new; older binaries carry it unchanged.

## [0.27.4] - 2026-09-26

### Fixed

- A gated merge survives a harness that kills its caller's process tree, or a session unit's cgroup: devctl runs in a transient user service (`systemd-run`, the hidden `beekeeper merge-child`) with the caller's environment and directory, its document, stderr and exit code in the state directory. A merge whose gate was killed is recorded by `watch` from those files once devctl ended (`MERGE RECORDED`), or from GitHub when there is no document, instead of `MERGE LOST`.

## [0.27.3] - 2026-09-26

### Changed

- `reporter check` and the reporter's post hook refuse every bare `#<n>`, `note #<n>` and `timer #<n>` included: a report names a beekeeper note `note <n>`, and a session name with a reference links it.

## [0.27.2] - 2026-09-26

### Fixed

- The watch reads `alerts.team`'s alerts that only `InhibitionOutsideWorkingHours` inhibits: an Alertmanager that inhibits `cancel_if_outside_working_hours` alerts outside working hours no longer hides the team's night-time alerts from the watch (every other inhibited alert stays out). A failed port-forward or Alertmanager request is tried again within `alerts.timeout`, and an unreachable installation's line says how long its alerts have been unseen (`ALERTS <installation> unreachable, alerts unseen for 20m (since 18:50Z): …`), again every 15 minutes while it stays unreachable; `reachable again` says how long they were unseen, and `alerts` and `handover` show an unreachable installation's last reading.

## [0.27.1] - 2026-09-26

### Fixed

- A gated merge survives its caller's session: the gate runs devctl in a session of its own with its document and stderr in the state directory (followed onto the gate's stderr), forwards only SIGINT, and waits on through SIGTERM and SIGHUP to record the outcome. A run that ends without its document or by a signal is judged by GitHub, not by its exit code: merged, it settles with its release unconfirmed (the gate line names `devctl release wait`) and keeps no retry place. A devctl release window whose merge ended unrecorded lifts once GitHub reports its pull request not merged; `watch` checks it too.

## [0.27.0] - 2026-09-26

### Added

- The scheduled reporter's post is checked before it goes out: `beekeeper reporter check` (new, stdin) and the PreToolUse hook `beekeeper hook reportcheck` (new), which beekeeper adds to each reporter session with `--settings`, refuse a `slack_send_message` whose message has a bare `#<n>` or `repo#<n>`, a pull request or issue link whose label does not name its repository and number, Slack's `<url|label>` syntax (the connector takes Markdown), a first line that does not name the machine's time zone, or a time in UTC; the reason lists what to fix. The reporter's prompt gives the time range in the machine's time zone, read for each run.

## [0.26.1] - 2026-09-26

### Fixed

- `agents start` and `agents handover` freeze the first turn's unit (`systemctl --user freeze`) while the desktop imports the session and thaw it once the desktop recorded it. The import reads the transcript's identity and then its end for title and model, and drops that read when the transcript changed in between, so a first turn that wrote meanwhile (a restart or takeover under an existing name most often) left the session untitled in the sidebar and without a model. Both commands now print the title the desktop recorded.

## [0.26.0] - 2026-09-26

### Added

- A scheduled status reporter: with `reporter.every` and `reporter.brief` (new; with `model`, `person`, default `guide.person`, `dir` and `timeout`, 20m), the standby watch starts a one-off reporter session once per interval, on its multiples: one headless turn in bypass in a transient user unit, registered on the roster as `Status report HH:MM`, not imported into the desktop. The session posts its report with the Slack connector; beekeeper sees the post in its transcript, takes it off the roster and stops it by PID (`reporter.posted`). A turn that ended without a post or ran past the timeout is ended the same way and said (`reporter.unposted`, `reporter.timeout`); while one runs, the next slot is skipped, once (`reporter.skip`). `beekeeper reporter` shows the schedule and the current or last run. The state's `report` field is new; older binaries carry it unchanged.

## [0.25.10] - 2026-09-26

### Fixed

- A lifted upgrade hold stays lifted: `hold lift upgrade:<installation>/<cluster>` records the lift (who, when) on the hold instead of deleting it, and the watch no longer sets the hold again while that upgrade (installation, cluster, target release) runs. The lift ends with the upgrade; a different target release holds the installation again. The gate and lease claims let work through meanwhile, and `hold list` and `status` show the lifted hold with who lifted it.

## [0.25.9] - 2026-09-26

### Fixed

- A supervisor or guide renamed after its `start` is known by its current title: `watch`, `supervisor start`, a lease claim and the other calls that observe the holder's CLI record the running session's title (a `supervisor.rename` or `guide.rename` event), and `supervisor status`, `guide status` and `status` show it without a second `start`. Workers that look the supervisor up by name no longer message the old one.
- `guide queue` and `guide watch` count only explicit asks as waiting on the person: the notes filed for the person and the sessions whose record says `--waits "<person>: <ask>"`. The desktop's summary of a session's last turn no longer feeds the queue; it read a status report (the supervisor's) as an ask, and `GUIDE WAITING` relayed asks nobody made.

## [0.25.7] - 2026-09-26

### Fixed

- `agents start` and `agents handover` give the imported desktop session the agent's name as its sidebar title. The desktop's import reads the title only from the transcript's last 256 KiB, and a first turn that grew past it left the name `-n` wrote at the start out of reach, so the session showed untitled; beekeeper now appends the name's `custom-title` line right before the import.

## [0.25.6] - 2026-09-26

### Fixed

- `watch` keeps checking a settling merge past `merge.settleTimeout` and drops it once its release rolled and the lane's HelmReleases are Ready, instead of leaving the lane settling until `lanes clear`. While it has not settled past the timeout, `watch` says `LANE STUCK` with what the lane waits for, and `lanes` marks the settle `stuck since`.

## [0.25.2] - 2026-09-25

### Added

- `watch` says `AGENTS STOPPED` once per registered agent with a task and no running CLI, and the `handover --prompt` Agents section marks it, each with how to resume it (`claude --bg --resume <session> "…"`, or the desktop session's `claude://code/continue` link). The state's format is unchanged.

### Fixed

- After a reboot the standby watch reopens the recorded supervisor's desktop session when no running spare can take over. The app starts at login before the watch's first poll, which first saw the supervisor's CLI gone only then, so the app looked started before the CLI stopped and the reopen never fired. The watch now also reopens when it never saw that CLI run under the running app.
- `watch` settles a running merge whose gate process is gone (`MERGE LOST`, a `merge.lost` event) instead of leaving it running until the lane's next gate call; `lanes clear` drops such a merge too.

## [0.23.1] - 2026-09-25

### Fixed

- `agents handover` sends the note request to the name the agent's running CLI answers peer messages under (an imported session's desktop derives one, such as `w-c2`), not the roster name, which no CLI answers to.

## [0.23.0] - 2026-09-25

### Added

- `beekeeper agents handover <agent>` hands a registered agent over to a fresh session near its context limit: it asks the agent by peer message for a note (`beekeeper agents note`, new), builds the follow-up's prompt (`--prompt`: the roster task, the brief, the session record, its gated merges, its last events, its note), starts the follow-up under the agent's name as `agents start` does, taking over the roster entry, task and session record, stops the old session's CLI and the processes under it by PID, and logs `agents.handover`; one line per step. It refuses (exit 3) before any step unless a PermissionRequest hook running `beekeeper hook permissionrequest` is configured where the follow-up starts (the user settings, or the folder's or checkout's `.claude/settings.json` or `settings.local.json`). `watch` says `HANDOVER DUE` once per agent session whose context reached `agents.relayAt` (new, default `supervisor.relayAt`), at its first quiet moment. The state's format is unchanged: the note is an event, the report is kept in the watch's mark.
- `agents start` and `agents handover` pass a configuration named with `--config` or `$BEEKEEPER_CONFIG` to the started session as `$BEEKEEPER_CONFIG`.

## [0.22.1] - 2026-09-25

### Fixed

- `guide queue` and `guide watch` leave out the guide's own session, archived sessions and test runs (titled `test: …`), and add the stopped, unarchived sessions waiting on the person, marked `(stopped)`. `sessions` marks archived and test sessions in its role column, `sessions --json` has `archived`.

## [0.22.0] - 2026-09-25

### Added

- `beekeeper agents start <name> <brief file>` starts an agent without a click: `claude -p` in `bypassPermissions` under a session id beekeeper chooses, in a transient user unit, recorded as one of beekeeper's starts and registered on the roster under the name before the session exists, then imported into Claude Desktop with `claude://resume`. The state's `starts` field is new; older binaries carry it unchanged.
- `beekeeper hook permissionrequest`, a PermissionRequest hook: it allows a request only for a session `agents start` started in bypass that now runs in `acceptEdits` (the desktop import's downgrade), each allow a `hook.allow` event; every other session gets the normal card, and deny rules still win.

## [0.21.1] - 2026-09-25

### Fixed

- `guide queue` and `guide watch` show only the notes for the guide's person, `guide.person` (new; matched without case, an older note marked only by a `[for <person>]` text prefix included, the prefix left out of its line), plus the sessions waiting on the person. A note the feed said before that is open but filed for someone else leaves it without a line. With `guide.person` unset, both show every `--for` note as before and say once that it is unset.

## [0.21.0] - 2026-09-25

### Added

- `beekeeper supervisor spare <session>` records the relay spare; `supervisor status` and `handover` show it, and the spare's `supervisor start` clears it. The state's `spare` field is new; older binaries carry it unchanged.
- The standby watch (`watch --standby`) keeps the spare awake: once it sat idle for `supervisor.keepAwake` (default `25m`, under the desktop's 30-minute idle timeout for an off-screen CLI) it sends `beekeeper keep-awake: reply "ok", nothing else` from the command line, silent unless it fails (`KEEP-AWAKE FAILED`, `SPARE ASLEEP`).
- After a supervisor crash the standby watch hands the role to the running spare from the command line once the CLI has been gone past `supervisor.restartGrace` (`HANDOVER`); `SUPERVISOR GONE` and its notification name the spare. A supervisor CLI back under a new PID is told to resume the role (`RESUME`).
- `beekeeper supervisor reopen` and the login unit `contrib/systemd/beekeeper-supervisor-open.service` start the desktop app on the recorded supervisor's session after a reboot; the standby watch does the same once after an app restart with no spare to take over.
- `internal/peer` sends a message to a running session from the command line: one headless `claude -p` turn with SendMessage, the only interface Claude Code offers for it.

### Changed

- `supervisor.restartGrace` defaults to `30s` instead of `1m`: the desktop app never restarts a crashed CLI, so it only debounces a supervisor someone woke.

## [0.20.0] - 2026-09-25

### Added

- A guide role for the person's decisions, next to the supervisor and never in its session: `guide start|stop|status|relay <successor>` with the supervisor's relay mechanics (`guide.relayAt`, `guide.relayTTL`, `guide.restartGrace`; relief exits 4), `guide handover [--prompt]` for its successor (`guide.skill`, default `guide`, or `guide.instructions`), `guide queue` (the open `--for` notes with their owning session, deadline and default, and the sessions the desktop files as waiting on their person) and `guide watch`, its feed of one line per new decision, waiting session, answer and guide relay (`GUIDE RELAY DUE` at `guide.relayAt`). `note answer <id> <answer>` records an answer verbatim in the `note.answered` event and closes the note. The supervisor and the guide share one role implementation; the supervisor's behaviour and output are unchanged. The state gains a top-level `guide` record, which older binaries carry unchanged. `sessions --json` shows a session's `waiting`, read from its desktop session record.

## [0.19.0] - 2026-09-25

### Added

- An installation is held while its clusters upgrade. `watch` reads the Cluster API clusters of `alerts.installations` every `watch.interval`, read-only (4 lists per installation: Clusters, KubeadmControlPlanes, MachinePools, MachineDeployments, within `alerts.timeout`, in parallel). A cluster's upgrade begins when its release label differs from the release cluster-api-events recorded, cluster-api-events marks it upgrading, or its scheduled upgrade is due, and ends once none of that holds and its control plane and node pools have rolled; a workload cluster on an older release is not upgrading. While it runs a hold `upgrade:<installation>/<cluster>` naming the cluster and both releases makes the merge gate refuse the installation's lanes (exit 77) and `lease claim <installation>` refuse; `watch` says `UPGRADE <installation>/<cluster> <from> → <to>` and `UPGRADE ENDED …` once each, `lanes` shows the hold and `snapshot` the upgrade with its progress. An unreadable installation is one line and keeps its holds.

## [0.18.0] - 2026-09-25

### Changed

- Reading commands print only what the caller has not read: `snapshot`, `sessions`, `lanes`, `agents` and `handover` keep a read mark per caller and command (`seen.<command>.<caller>.json`), and a caller inside a Claude session that has read before gets the facts that are new or changed since, `gone: …` for those gone, or one `no change since HH:MM` line; a moving figure (an age, a countdown, a memory size, a context in tokens) is no change. `--full` prints everything; `--json`, `handover --prompt` and a person at a terminal always get the whole text. `snapshot --changes` is now the default and hidden. Marks are per host session: a subagent shares its parent's and passes `--full` or `--json`. Against v0.17.0 a second read with nothing changed drops from 5533 to 22 bytes (`snapshot`), 11746 to 22 (`sessions`), 590 to 22 (`lanes`), 577 to 22 (`agents`) and 23424 to 22 (`handover`).
- `watch` says a lasting condition (a threshold breach, an unreadable source, a stalled lane) once when it starts and once as an `ENDED <condition> (since HH:MM)` line when it ends, instead of repeating it every `watch.repeat`, across restarts: the open conditions and one-time events (runaways, stale leases) are kept in the caller's mark (`seen.watch.<caller>.json`), so a watch restarted after an install says only an `ENDED` or what is new (`--once` keeps no mark); `watch.repeat` now only paces the notifications, which keep `notify.repeat`. Its lines carry no restated threshold or explanation: `LOW RAM: <n> MiB available, swap <n> MiB`, `SWAP: <n> of <n> MiB used`, `HIGH LOAD: <n>`, `TMPFS /tmp: <n> MiB`, `LOW DISK: / <n> GiB free`, `DESKTOP SCOPE anon: <n> MiB`, `DESKTOP SCOPE near its cap: …`, `GITHUB BUDGET <n> of <n>: hold GitHub work until HH:MM`, `SESSIONS ended: …`, `SESSIONS restarted: …`. An `ALERT RESOLVED` line leaves out the `since` its NEW line said, and an alert reading says each bracket part (a lease, a merge) once. A real hour of a supervisor's watch drops from 6977 bytes under v0.15.5 to 5862, each fact once, kept by a byte-budget test.

## [0.17.0] - 2026-09-25

### Changed

- Claims stay gated after a supervisor crash: the grant rule holds from the first `supervisor start` until a deliberate `supervisor stop` or a successor's `supervisor start`, instead of lifting once `supervisor.restartGrace` has passed. A claim during the gap is refused naming the gone supervisor and the successor's start; `supervisor status` and `status` say it is gone and claims wait. `supervisor.restartGrace` now only delays the gone line and notification: a CLI back within it is still a restart that keeps the role.
- A supervisor gone past the grace is `SUPERVISOR GONE` once, naming it and that claims stay gated, said once the grace has passed instead of after two polls; its `no-supervisor` notification is critical by default and repeats after `notify.repeat` while the gap lasts. A supervisor back after it is one `SUPERVISOR BACK` line.

## [0.16.0] - 2026-09-25

### Changed

- `supervisor.relayAt` (tokens, default `400k`) replaces `supervisor.shift`: `watch` says `RELAY DUE` once the supervisor session's context, read from its transcript's last request like the `CTX` column, reaches it, at the first quiet moment (no gated merge running or settling, no grant waiting, no claim queued, no relay open). It is one line naming the context and `beekeeper handover --prompt`, said once per supervisor and again only after a relay is cancelled or expires. `supervisor status` and `handover` show the supervisor's context in tokens. The state's `relayDue` record replaces `shift`.

### Removed

- `supervisor.shift`: a supervisor is relayed at a context size, not after a time.

## [0.15.5] - 2026-09-25

### Fixed

- `agents register` replacing the entry of a session that no longer runs no longer drops the task that entry was assigned and never finished: the new entry takes it over with its assignment time, the `agents.register` event says `takes over "<task>"` and the output tells the fresh session to work it. Re-registering keeps the session's own open task, and a registration that would drop two entries with open tasks is refused (exit 3).

## [0.15.4] - 2026-09-25

### Fixed

- The PreToolUse hook gates every `devctl pr merge` that runs as a command: behind `flock <lock>`, `nohup`, `setsid`, `stdbuf`, `ionice`, `chrt`, `nice`, `timeout` (with options), `env` (with `-u`), `time`, `command`, `exec` and `VAR=value`, in any segment of a pipeline or list, and with devctl named by path (`~/bin/devctl`, `./devctl`). The gate wraps only the devctl invocation, so pipelines and `pipefail` behave as written. 43 of the 577 real merge invocations in the lab machine's transcripts ran ungated before (`flock <lock> devctl pr merge … | tee … | ledger.sh record …` among them); all are pinned in `internal/guard/testdata/merges.jsonl`.
- A `devctl pr merge` inside a `sh`, `bash` or `zsh -c` string that the rewrite cannot reach is refused, naming the command with the gate written in, instead of running ungated.

## [0.15.3] - 2026-09-25

### Fixed

- A cap kill in a memcap scope no `run.start` names is reported with `cap unknown` (and the owner unknown when its process is gone) instead of "memcap cap on one command", which read as a build hitting the default 12G cap.
- The guard test's deliberate 64M kill no longer reads as a build's: a run with `MEMCAP_TEST=1` (the capped-run tests set it) takes a `memcap-test-…` scope, and `snapshot` and `watch` report a kill in one as a test kill, `watch` as one quiet `test kill:` line instead of `KERNEL OOM`, `snapshot --changes` as `N test kills` apart from the kernel OOM kills.

## [0.15.2] - 2026-09-25

### Fixed

- `agents register` in a `claude --bg` worker registers it under its `-n` title instead of its session id: a session's name falls back to the name its CLI's record holds (`~/.claude/sessions/<pid>.json`) before its id, for every command that names the caller. `--name` still overrides.
- A fresh session registering under the name of an agent whose session no longer runs replaces that entry instead of adding a second one, so `agents assign <name>` reaches the new session; the output names the replaced session and a task it left unfinished. A name a running session's entry holds is refused (exit 3), and `agents assign` and `agents remove` refuse a name several entries share instead of taking the first.

## [0.15.1] - 2026-09-25

### Fixed

- A `claude --bg` worker the daemon served from its pre-started spare CLI is listed in `sessions` under its session id and name, so `lease grant`, `agents` and every lookup by session find it: the second worker started through a daemon, and a worker woken with `claude --bg --resume <id>` while a spare was ready. A spare no session has claimed is not listed.
- A woken `claude --bg` worker is listed under its session id instead of its transcript's path.
- A resumed CLI that went on under a new session id is listed under that id, and its transcript and last activity are the new one's.
- A session's id and name come from the record its CLI keeps of the session it runs (`~/.claude/sessions/<pid>.json`, configurable as `claude.sessionsDir`), ahead of its command line and environment; a record a dead CLI left behind is not taken for a later process under its PID.

## [0.15.0] - 2026-09-25

### Added

- `lanes` marks a lane `stalled` when no merge runs and its first arrived merge has waited longer than `merge.stallAfter` (default 5m) behind places whose merges are not in the gate (seeds that have not arrived, merges that left it), naming the waiting merge and those places; `lanes --json` carries it as `stall`. `watch` says each stall as one `LANE STALLED` line, repeated at most every `watch.repeat` while it lasts.
- A `devctl pr merge` gate call waiting for its turn re-executes the new binary when `beekeeper self-update` (or any rename) replaces its own: the same process, arguments, stdio and deadline, the merge's place found again by session, repository and number, so a fix reaches calls that already wait. It says `continuing under beekeeper <version>`; a call whose devctl runs is never re-executed. Linux only.

### Fixed

- A lane no longer reads `settling … until vX rolls` after its release rolled: `watch` drops a settling merge once its release rolled and the lane's HelmReleases are Ready (logged as `lane.settled`), so `lanes`, `handover` and the relay's quiet moment see the lane free before its next merge arrives. The roll check itself already matched a chart version with build metadata (`4.74.0+971d12027db0`) against its tag (`v4.74.0`); a test now pins it against a real `kubectl get helmreleases -A -o json` answer.
- A merge into a repository whose lane has no installation leaves nothing settling: its lane frees when devctl returns.

## [0.14.1] - 2026-09-25

### Fixed

- A relieved supervisor's `supervisor status` keeps exiting 4 after its successor relays onward, cancels a relay or is relieved in turn: the relief is recorded per party (`relieved` in the state) instead of read from the latest relay, and ends when that session supervises again or after 7 days. The message names who relieved it and who supervises now.
- A supervisor CLI restart no longer opens an ungated window for claims: the grant rule holds for `supervisor.restartGrace` (default 1m) after beekeeper first saw the supervisor's CLI gone (`supervisorCLI` in the state, recorded by a claim, the watch and the supervisor commands), and a CLI back under the same session id or desktop record within it keeps the role and an open relay. A refused claim says the supervisor is restarting and until when, `supervisor status` and `status` (`~<name>` in `--bar`) show the restarting state, another session's `supervisor start` stays refused, `SUPERVISOR GONE` waits for the grace, and the restart is logged (`supervisor.restart`) and said by `watch` (`SUPERVISOR RESTARTED`). A CLI that does not come back lifts the rule once the grace has passed, as before.

## [0.14.0] - 2026-09-25

### Added

- Per-session metrics, read from the transcripts, the process table, the memcap scopes, the event log and the leases, never from GitHub: busy and idle time, turns, tool calls and their errors (and the most repeated failing call), GitHub calls, tokens and cost, context size and fill, the capped runs' memory, `gh`/`devctl` processes, merges queued, merged, refused and failed, and lease times. `sessions` shows each session's context and last hour, `sessions --json` every figure over the transcript window and the last hour, `sessions --json` and `snapshot --json` the totals, `snapshot` the three sessions that spent the most in the last hour. Cost comes from `metrics.models` (the Claude API list prices by default); a model without a price is `cost unknown`. The transcript is read once for its work and its figures, all sessions in parallel.
- `watch` says `RUNAWAY` once per session and figure over `metrics.runaway`: GitHub calls in the last hour (1000), the same failing tool call repeating in it (10), the context's fill of its window (0.9).

## [0.13.2] - 2026-09-25

### Fixed

- A merge that ends with nothing merged (a branch behind, exit 3) keeps its place in its lane: `lanes` shows it `retrying`, and its session's retry runs before every merge that was behind it instead of joining at the end. It holds the lane for `merge.queueTTL` after the failure and keeps its place for `merge.seedTTL`; devctl's refusal (exit 5) leaves the lane.
- A free lane no longer waits for a seeded place whose merge has not arrived: an arrived merge that was not seeded runs ahead of it, and a seed never holds up an earlier pull request of its own session. Seeds keep their order among themselves, and `merging` names the places a merge passed.

## [0.13.1] - 2026-09-25

### Fixed

- A session started from inside another session no longer reads as that session restarting: a CLI whose environment names a session is its child, listed under its own `--session-id` (or `--resume`) and `-n` name and `started by` it, and `watch` reports no restart of the parent. A `claude -p` its tool shell runs without an id of its own stays one of its commands, and its memory is no longer counted twice once the child is a session.
- `claude --bg` workers appear in `sessions` under their session id and name; the `claude daemon`, its terminal hosts, the spare CLI, the `--bg` launcher and the other `claude` subcommands (`stop`, `logs`, `mcp`, …) are no longer listed as sessions.
- `--session-id` on a CLI's command line names the session, ahead of the environment.

## [0.13.0] - 2026-09-25

### Added

- Desktop notifications: `watch --notify` sends the events that need a person to `org.freedesktop.Notifications` over D-Bus and still prints every line: a note or timer due, the machine near its OOM line, a kernel OOM kill outside a build slot or a systemd-oomd kill, the GitHub budget under the floor, a stale lease and a supervisor whose session ended with no successor (`notify.kinds`). Each event is one notification however many watches notify, claimed in `notify.json` under `notify.lock`; a lasting condition notifies again after `notify.repeat` (30m); `notify.quietHours` hold everything but a critical one (`notify.urgency`) and send what they held as one notification when they end. With no notification service the watch runs on and says so once.
- `watch --standby` and the user unit `contrib/systemd/beekeeper-notify.service` (shipped, not enabled): a watch for when no supervisor runs, which leaves a running supervisor's notes, timers, session records and relays to its watch and never reads the alerts.
- `watch` says `SUPERVISOR GONE` once when the recorded supervisor's session has ended with no relay open.
- `status [--bar]`: the supervisor, the leases held, the holds and what is due in one line; `--bar` is a fixed tab-separated row for a desktop bar, the shape of `free --summary`'s rows.

### Changed

- The owner hints on a NEW alert line read as timing, not cause: `[during merging … since …]`, `[during merged … at …]`, so a merge that merely coincides with an alert is not read as its cause.

## [0.12.0] - 2026-09-25

### Added

- Alert triage: `alerts.installations[].floor` is the lowest severity (none, info, warning, notify, critical, page) of an installation's alerts that `watch` and `snapshot` show; the alerts below it stay in the baseline, so a changed floor prints no burst. The flap damper (`alerts.flap`, default 4 changes within 1h) turns an alert that keeps firing and resolving into one `ALERT FLAPPING` line and holds its changes back until it has been stable for the window; its records live in `alerts.json`. A NEW line names the merges into the installation's lanes (`merging`, `merged`) and the lease claims on it of the last 30 minutes, each with its session.
- `alerts capture <dir>` records each installation's Alertmanager answer; `alerts replay <dir>...` prints what the watch would for recorded answers, from the current baseline without writing it. `handover` shows the floors and the damper.

## [0.11.0] - 2026-09-25

### Added

- `beekeeper run` records every capped run in the event log: `run.start` (the scope, the slot, the cap, the command) and `run.end` (the exit code, the duration, the cap's victims), both as the calling session. `snapshot` and `watch` name the session and the command of a memcap cap kill from its scope's `run.start`, also after the run and its session have ended. Logging never fails or delays the run: an event the log cannot take within a second is dropped.
- `log --verb <prefix>` shows only the matching events (`--verb run.`: the build runs); `handover` leaves the runs out of its latest events.

## [0.10.0] - 2026-09-25

### Added

- `supervisor relay <successor>`: the supervisor names its successor, whose `supervisor start` takes the role, the grant queue and the pending grants in one step, so no claim goes ungated in between; the event log shows both steps. A relay not taken expires after `supervisor.relayTTL` (default 15m) or is withdrawn with `relay --cancel`; the outgoing supervisor keeps the role meanwhile. `supervisor status` exits 4 in the session a relay relieved. `watch` says `RELAY TAKEN` or `RELAY EXPIRED` once.
- `supervisor.shift`: `watch` says `RELAY DUE` once the supervisor has served its shift, at the first quiet moment (no gated merge running or settling, no grant waiting, no claim queued), and again only when a quiet moment follows a busy one.

## [0.9.0] - 2026-09-25

### Added

- `note add --default "<what happens if unanswered>"`: a decision carries its default; `watch` reports a note once when it is due.
- `timer add <time> <what>`, `timer done`, `timer list`: `watch` prints one line when a timer is due.
- `sessions serve <session> <owner/repo#n> [--waits "<what>"]` and `sessions unserve`: a record for any session; `sessions` and `handover` show it, and `watch` prints one line when the session ends, naming the issue to re-query, instead of the plain `SESSIONS ended` line.
- `handover` shows the session records, the notes' defaults and the timers; `handover --prompt` prints the successor supervisor's session prompt: the configured instructions (`supervisor.skill` or `supervisor.instructions`), the scope (`supervisor.scope`), the pending state in full and the commands that read the live values, without standing rules or live values.
- `state.json` keeps the fields a newer beekeeper wrote when an older one still running rewrites it.

## [0.8.1] - 2026-09-25

### Fixed

- The merge gate reads the release where devctl's `pr merge` document reports it (`release.tag` beside `release.verdict`, the tag counted only when the verdict is `available`): a released merge holds its lane until the release rolled, no longer only for `merge.settle` as a merge of unknown release.

## [0.8.0] - 2026-09-25

### Added

- `lanes settle <owner/repo> <n> [--for <session>]`: a merge run outside the gate heads its lane until it merges, so its own `devctl pr merge` passes the gate while the lane's other merges wait; merged through the gate or outside it (GitHub asked at most once a minute), it settles the lane like a gated merge. `lanes` and `handover` show it.
- `hold set --except owner/repo#n` on a lane or repository hold: the hold refuses every merge but that pull request's; `hold check` takes `owner/repo#n`, and `hold`, `lanes` and `handover` show the exception.

### Fixed

- A merged run whose release devctl could not confirm no longer keeps its lane until `merge.settleTimeout`: after `merge.settle` only the lane's HelmReleases decide.

## [0.7.0] - 2026-09-25

### Added

- `snapshot` and `watch`: the machine tick and the Monitor source, with every OOM kill attributed, session starts, ends and restarts, stale leases, and the installations' alerts.
- `alerts watch|snapshot|import` and the `alerts:` configuration: each installation's Alertmanager (Mimir's with the `giantswarm` tenant, else the plain one) read natively over bounded `kubectl port-forward`s, in parallel with a timeout each; NEW and RESOLVED lines with pages and the configured team marked, a burst of one alertname collapsed, one line when an installation stops or starts answering, and the lease holder and sessions on the installation named; `alerts.ignore` for known flappers; one baseline with one owner, and `import` to take over an existing one. `watch` and `snapshot` include them, and `handover` shows what the alert watch reads.

### Removed

- The `checks:` configuration and `snapshot --no-checks`: the alerts it ran a script for are read natively (`snapshot --no-alerts` skips them).

### Fixed

- `watch` stopped by SIGINT or SIGTERM during a poll no longer reports the journal or the GitHub budget as unreadable.

## [0.6.0] - 2026-09-25

### Added

- The merge gate: the PreToolUse hook puts `beekeeper gate` in front of every `devctl pr merge`. Merges queue per lane (`lanes` in the configuration), one at a time, the next once the lane's HelmReleases are Ready and the previous release rolled; a held repository, lane, `merges` or `github`, a budget under the floor and an unreadable installation refuse (exit 77); a wait past its bound exits 76 and keeps the merge's place; the machine runs at most `merge.cap` devctl processes; a giantswarm/devctl merge opens the tool-release window. devctl's document and exit code pass through unchanged.
- `lanes [queue|drop|clear]`: each lane's running, settling and waiting merges, also in `handover`; `queue <owner/repo> <n> --for <session>` seeds a session's place so an agreed order carries over.
- `hold set|lift|check --lane <name>`, the `merges` target and `--except`.

## [0.5.0] - 2026-09-24

### Added

- `free [--apply] [--only …] [--summary]`: where the memory is, and what can be freed without touching running work (dead sessions' dirs in the tmpfs `/tmp`, throwaway temp dirs, orphaned workers), with the `free-memory` script's TSV summary for its desktop front end; a live session's dirs stay however idle it is, and what needs root is printed as the command to run.

### Fixed

- `hook pretooluse`'s third-lab refusal names each lease holder as `lease list` does (the claim's or the live session's name), no longer the `user@host` holder field.

## [0.4.0] - 2026-09-24

### Added

- `run` and `hook pretooluse`: a build, test or lint command in one of memcap's build slots under a memory cap (exit 75 when the wait runs out, 137 with the victim when the cap fires), and the PreToolUse hook that routes heavy commands through it and refuses a third kind cluster with the held leases.

## [0.3.0] - 2026-09-24

### Added

- `self-update [--check]`: install the latest release once its Sigstore bundle verifies, with one rename that leaves a running `watch` alone; `--check` exits 125 while a newer release is out.

## [0.2.0] - 2026-09-24

### Added

- `sessions` and `tail`: every running Claude Code session from disk (processes, desktop records, transcripts, checkouts), what it is on, what it runs, overlaps.
- `budget`: the GitHub budget from a conditional request's headers, with every `gh` and `devctl` process and its session.
- `lease`, `hold`, `supervisor`, `agents`, `note`, `handover`, `log`: shared resources and the supervisor's state on disk, with grants required while a supervisor runs.
