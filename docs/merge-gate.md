# The merge gate

Every merge on the desk goes through the gate: sessions type `devctl pr merge` as
usual, and beekeeper queues, admits and records the merge in its lane.

A merge waiting behind a live gate reads only its local queue position: it makes no GitHub
calls and does not rewrite the state. Its sleeps back off from five seconds to one minute,
so the next merge notices its turn within one minute. A dropped place, a paired refusal,
a dead predecessor, or the turn itself resumes the ordinary gate checks; deadlines and
cancellation still bound the wait. A live gate keeps its queue place without heartbeat writes.
Before handing the wait to a detached run, it renews the place once so the queue TTL covers
the successor registering after the original process exits.

## devctl pr merge through the gate

Sessions keep typing `devctl pr merge <owner/repo> <n> [flags]`; beekeeper never replaces devctl.
`devctl release promote <owner/repo>` (one repository, no `--dry-run`) passes the same gate: it
queues in its repository's lane and runs like a merge, and the stable release it dispatches settles
the lane as a merge's release does; a promotion of several repositories or of a `--team` runs
ungated. A promotion is queued for one release candidate. Pinned, `devctl release promote <owner/repo> --version
vX.Y.Z-rc.N` (the candidate proven live), the place records that candidate (`merge.queued … for
candidate vX.Y.Z-rc.N`) and devctl promotes it at its turn, whatever was cut after it.

Unpinned, the gate asks `devctl release promote <owner/repo> --dry-run` for the newest one when the
call arrives, records it on the place, and asks again when the turn comes, right before devctl runs.
When the newest candidate is another by then (a merge behind it cut a new one), the promotion is
refused (exit 77, `… was queued for candidate A, the newest is candidate B now: nothing promoted`),
its place leaves the lane and the refusal wakes its owner, who decides again: a promotion queued by
one worker never promotes another worker's candidate.

An unpinned promotion never waits on outside its call: where a merge would be queued (exit 76) it is
refused (exit 77, `… only a pinned promotion waits on outside the call`), naming the newest
candidate, since its later turn would promote a candidate nobody proved. `beekeeper lanes drop <owner/repo>
promote` takes a waiting promotion out of its lane; its gate, a queued run of its own included,
refuses and wakes its owner, as for a merge's place taken out with `lanes drop <owner/repo> <n>`.

The PreToolUse hook rewrites the call to `<this binary> gate -- devctl pr merge …` (a background
call gets `--wait 30m`), and the gate decides. The hook finds the merge wherever it runs as a
command: in any part of a pipeline or a `;`, `&&` or `||` list, in a subshell, a loop, a group or a
condition (`if`, `while`, `until`, `!`, `{`), behind the prefix commands that run their arguments
(`flock <lock>`, `nohup`, `setsid`, `stdbuf`, `ionice`, `chrt`, `nice`, `timeout`, `env`, `time`,
`command`, `exec`, `VAR=value`, each with its options), and with devctl named by path
(`~/bin/devctl`, `./devctl`, `$HOME/bin/devctl`).

It wraps only the devctl invocation: `flock m.lock devctl pr merge o/r 7 | tee m.json | jq .verdict`
becomes `flock m.lock <this binary> gate -- devctl pr merge o/r 7 | tee m.json | jq .verdict`, so
the pipeline and `pipefail` behave as written and the gate's exit code and devctl's document reach
the rest of it. A here-document a shell reads as its program (`bash <<'EOF'`, `cat <<'EOF' | sh`) is
a command line too: a merge in it is gated in place; a here-document that is content (a file
written, an issue body sent) stays as written.

A merge the rewrite cannot reach is refused, the refusal naming the place and the line and, where it
has a place, the command with the gate written in: one inside a `sh`, `bash` or `zsh -c` string or
an `eval` string; one in a script file the call runs, by a shell (`bash merge.sh`), by its path
(`./merge.sh`), sourced, or by an interpreter (`python3 merge.py`), read relative to the call's
working directory and followed into the scripts it runs in turn, a script the call writes before it
runs it judged by the call's own text; one in an interpreter's code (`python3 -c`, `node -e`, `perl -e`,
…); and one in the program a shell or an interpreter reads from its input (`echo '…' | bash`, `bash <<<
'…'`, `sh < merge.sh`, `python3 - <<'EOF'`).

Shell `-c` strings also follow the script files they execute, source or pass to an interpreter (`zsh -c
'bash merge.sh'`, `sh -c '. merge.sh'`, `bash -c './merge.sh'`). Nested `-c` strings share the
script recursion bound; a refusal names the script and its line.

Configure a fork repository with `merge.methods: {example/fork: rebase}` to keep
its upstream-ready commits separate and linear. The gate adds `--rebase` before
checking or queuing its devctl PR merges, including when the worker omits the flag or
passes `--rebase=false`. Repositories without this setting keep the default squash
method; an explicit worker `--rebase` still works.

A merge a runner runs from its arguments (`xargs devctl pr merge o/r`, `find … -exec devctl pr
merge … \;`, `parallel`, `sudo`, `watch`, `ssh`, …) or a backquote substitution runs (`` x=`devctl
pr wait o/r 7` ``) is refused too, on the command line, in a here-document or here-string a shell
reads and in a script the call runs. The refusal names the call with the gate written in before
devctl (`xargs <this binary> gate -- devctl pr merge o/r`), except under `ssh`, whose remote host
has no gate. It fails closed: an unquoted merge the rewrite did not gate is refused whatever command
it is an argument of (an unlisted wrapper, `busybox`). A text command that reads it passes (`echo`,
`grep`, `gh`, `git commit`, `beekeeper note`, `xargs echo`, `find … -exec grep`).

Only what the call runs is read: a file after the program (`python3 -c '…' log.txt`,
`bash x.sh log.txt`, `python3 - log.txt <<'EOF'`), a module's argument (`python3 -m json.tool
log.txt`), a read-only command's argument (`grep merge log.txt`) and a file read into a quoted
argument (`--report "$(<report.txt)"`) are the program's data, whatever they say; a refusal a file
causes names the file, the matched token and its line, so a data file the call does run as its
program (`bash log.txt`, `cat log.txt | sh`) is seen as such. A merge the gate precedes in the
script or the code (`<this binary> gate -- devctl pr merge …`) passes, and runs through the gate
like a merge typed on the command line.

- **Refused, exit 77**, one line starting `beekeeper gate: refused,` that says why and what to do:
  the repository, its lane, `merges` or `github` is held, or a cluster upgrade runs on the lane's
  installation (the hold's reason); the GitHub budget is unknown (no login), checked before devctl
  makes a single request; the lane's installation cannot be read (a lapsed `tsh` login). A read that
  fails in transit (a resolver error, a refused or reset connection, a timeout, a killed `kubectl`,
  gh's `error connecting to`) is sent again three times, 10s, 30s and 1m20s apart, each retry a
  `reading … failed in transit (…): retrying in …` line, the merge keeping its place; only then does
  it refuse, naming the read's last error and the tries; the GitHub budget's read exits 74 instead
  (below).

  Nothing was queued: a 77 never says `queued`. A hold refuses a new merge only: a merge that has
  its place in the lane keeps it during a hold and runs once the hold lifts or lets it through, and
  a held place holds no place against the one merge the hold lets through (`--except owner/repo#n`),
  which enters first. A hold with such an exception set while a merge's devctl still waits for CI
  ends that devctl before it merges, and the merge goes back to its place (`merge.preempted` in the
  log); one GitHub reports merged all the same is recorded as merged.

  Nothing else stops a merge: a lane whose release has not rolled past `merge.settleTimeout` keeps
  it waiting, and `watch` says `LANE STUCK`.
- **Unread, exit 74**, one line starting `beekeeper gate: unread,`: the GitHub budget's read
  failed in transit on every try (`reading the GitHub budget failed in transit (…), the merge was
  not judged`). It is no refusal: nothing was judged or queued, and the same command runs again
  once GitHub answers.
- **Queued, exit 76**, one line starting `beekeeper gate: queued,` with the merge's position and
  whom it waits behind (when the machine-wide devctl cap is the reason, `<n> devctl processes run
  machine-wide (cap <n>), not a lane problem`), once the merge has waited `--wait` (2 minutes, 30
  in the background), or at once with the budget and its reset when the GitHub budget is under
  `github.floor`. The merge is not dropped: the gate hands it to a run of its own outside the
  caller, the same gate under `--queued`, which keeps its place for up to `merge.seedTTL`, runs
  devctl when its turn comes and wakes the owner with the outcome (`devctl.unheard`, as below).
  Nobody runs it again; a second `devctl pr merge` of the pull request while it waits is refused
  with exit 3.
- **Left at the tool limit, exit 76**: a desktop Bash call stops its command at 10 minutes without
  an exit code, so a foreground call leaves `merge.toolLimit` (9m) after it started, whether the
  merge still waits for its turn (queued as above) or its devctl runs. A running devctl goes on
  outside the call (`beekeeper gate: o/r#7's devctl runs past this call's limit …`, logged as
  `merge.left`), its outcome wakes the owner (`devctl.unheard`, as below) and `watch` records the
  run once devctl ended. A merge expected to outlive the limit runs with `run_in_background`: the
  hook gives it `--limit 0` and a 30-minute wait.
- **Otherwise devctl runs once**, its JSON document and exit code (devctl's own 0–9) unchanged, and
  the event log records `merging <owner/repo>#<n>` (`promoting <owner/repo>` for a promotion) and
  `merged` with the release; a run that merged or promoted nothing says so in one gate line after
  devctl returns (`nothing promoted (not_built)`, devctl's state of the promotion). Its wait for the
  CI outcome is `merge.ciTimeout` (1h, passed as `--timeout`) unless the command names its own
  `--timeout`: devctl's default of 30m is shorter than a CI that `--update-branch` restarts from
  zero, and a timeout (exit 2) merges nothing. devctl serves the repositories of
  `merge.devctlOwners` only (its GitHub App login reaches the giantswarm organisation); any other
  owner's repository takes the **plain squash merge** instead, in the same place and unit: as the gh
  login, it waits up to `--timeout` (45m) for the head's checks, refuses what devctl refuses before
  its wait (a draft, a closed, merged or conflicting pull request: 3; another person's: 5), refuses
  a reviewer's feedback as the gate's review read (below) does (5, read before the wait and again
  right before the merge call; its document's `unansweredReviews` lists what the last read found,
  empty when nothing held the merge), and squash-merges green with the judged head as the expected
  one and `<title> (#<n>)` as the subject, then deletes the branch; red is 1, a timeout 2, a gh
  failure 7, and it waits for no release.

  Each run's stderr and document are kept in `<state>/merge-output/<repo>-<n>-<time>.log` for 7
  days, named in its event. A merge call (or branch update) GitHub answers with a 5xx, devctl's exit
  7 naming `…/pulls/<n>/merge: 5xx`, is sent again while GitHub reports the pull request open, up to
  three times 10 s, 30 s and 1 m apart (`merge.retry` each), with the expected head as before; the
  caller reads one document, the last run's. A merge with `--update-branch` whose base moved during
  every one of devctl's update waits (exit 3, "… moved during the wait) after 3 waits") keeps its
  lane place, so no other gated merge of the repository moves the base, and its merge-child runs
  devctl again after a pause (1m, 2m, 5m, then 10m each), which updates the branch once its checks
  are green, until it lands or `merge.busyBudget` (4h from devctl's first run) is spent: each run's
  line names the budget spent, a landing says so, and a spent budget ends the run with devctl's exit
  3 and a reason naming the budget (`busy-base budget spent: <n> devctl runs in <d>`).

  A red run (exit 1) is judged against its base branch: when every red check is neither required by
  the base (branch protection or a ruleset, read with `gh api`) nor a security scan (nancy,
  gitleaks, govulncheck, trivy, CodeQL, ...) and is red on the base branch's head too, devctl runs
  again with `--accept-red <check>` for each (`merge.acceptred`, naming the checks and why), the
  caller reading that run's document; a red check green, still running or without a result on the
  base head, a required one or a security scan ends the run red as before, and so does a devctl
  without `--accept-red`, the gate's line naming why.

  A run that ends with nothing merged is `merge.failed` and leaves the lane with its run: a place
  dies with its process, and the session's retry joins the lane anew.

## Lanes and turn order

A merge runs when no merge before it in its lane's queue holds its place, nothing else of the lane
runs, fewer than `merge.cap` devctl merges and promotes run on the machine (a `pr wait`, `release
wait` or `rollout wait` only polls and does not count), the calling session runs fewer than
`merge.sessionCap` merges (default 2; each running devctl polls GitHub, so ten merges one session
queued poll as two would, the others wait in their lanes without reading GitHub and start in the
order they queued; `beekeeper budget` lists each session's polling and waiting merges), and the
lane's installation is ready: every HelmRelease of the lane's charts Ready, and the previous merge
rolled.

Rolled means each HelmRelease of the merged repository's chart that ran the newest version when the
merge started now reports the released version, or follows a range that never admits it: the
HelmRelease's OCIRepository ref (`semver`, a `tag` as that one version) or its chart template's
`version`. A merge that cuts only a release candidate (`v4.105.0-rc.3`) under a stable range
(`>=4.0.0 <5.0.0`) settles at once, and the gate says `production does not follow 4.105.0-rc.3:
flux-giantswarm/… follows semver >=4.0.0 <5.0.0`; a wait names the range, and `lanes` calls a
candidate a pre-release.

A merge whose release failed (devctl exit 6: its auto-release run or tag pipeline failed) rolls
nothing and settles at once; `lanes` and the `merged` event say `release failed`. A release devctl
could not confirm (exit 9, a run killed by the tool timeout) settles for `merge.settle` instead.
Versions compare as semver: a tag `v4.74.0` matches a chart version `4.74.0+971d12027db0`. The
installation is read with `kubectl --context <lanes[].context>` (default: the kubeconfig context
named after the installation or ending in `-<installation>`).

A merge whose lane has no installation has nothing to roll, and its lane overlaps the release with
the next merge's CI: devctl ends at the merge (`--no-release-wait`), the merge stays in its lane as
`releasing` while its gate confirms the release (`devctl release wait <repo> --pr <n>`, the
`merge.release` event), and the lane's next merge starts meanwhile, updates its branch, waits for
its CI and merges only once that release is confirmed or failed, so the lane drains one merge per CI
run and never merges ahead of a release's verdict.

A call with `--until-merged` (the supervisor loop's reviewed merges) ends once the merge landed
and its merge-child awaits the release: exit 0 and a `merge.left` event, the merge-child
confirming the release and recording it with the run, which settles the lane as when the gate
leaves at its limit. The caller no longer waits on a release it does not act on.

The gate's exit is what devctl pr merge's own release wait would have ended with (6 failed, 9
unconfirmed). That wait is the merge's own: a stop (SIGINT, `beekeeper gate stop`, a TaskStop) ends
it with nothing merged and the place out of the lane, a hold that lets another pull request through
first puts it back to waiting, the reviews are read again before devctl starts, and a verdict still
unknown past the release wait's bound refuses the merge rather than land it.

A releasing merge whose gate is gone, or whose release the gate could not confirm, and one whose
release is unknown (merged per GitHub, its run lost) settles that lane for `merge.settle` all the
same, so a promotion queued behind it does not race the release candidate it may be cutting. A
central lane, devctl's own and a branch without repository-wide Auto-release keep their own release
handling. A running merge whose devctl ended after its gate left is judged (its document, else
GitHub) before the next merge or promotion of its lane starts.

A settling merge leaves its lane once the lane has settled: `watch` checks every poll, logs
`lane.settled`, and `lanes` then shows the lane free before its next merge arrives.

A free lane never idles for a merge that is not there. A place holds against the merges behind
it while its merge is in the gate or was within `merge.queueTTL` (a rerun after exit 76, the retry
of a failed run), and a merge registered with `lanes settle` always does. A place seeded with
`lanes queue --for` whose merge has not arrived holds up only the seeds behind it, so seeds keep
their order among themselves, and never an earlier pull request of its own session and
repository, which cannot arrive first; an arrived merge that was not seeded runs ahead of it, and
`merging` names the places it passed.

A merge the gate did not wrap leaves its lane looking free while its release rolls. `beekeeper
lanes settle <owner/repo> <n>` registers it: until the pull request is merged it heads the lane, so
its own `devctl pr merge` (a retry of a failed run) passes the gate as the lane's next merge and
the others wait behind it, through a lane hold's refusal too. Once merged through the gate, it
settles like any gated merge; merged outside it, GitHub reports no release, so the lane settles for
`merge.settle` from the merge and then frees once its HelmReleases are Ready. A merge waiting
behind the entry asks GitHub (`gh pr view`) at most once a minute; a pull request closed without a
merge leaves the lane. The same holds for every place whose merge is not in the gate (a seed, a
failed run's retry place, a place kept after its gate left): `watch` asks GitHub about it at every
poll, a pull request merged outside the gate settles the lane (`merged … outside the gate`) and a
closed one drops the place (`merge.dropped`), so no place waits for a `lanes drop`.

## Replaced binaries and surviving callers

A gate call keeps deciding by the code it started with only until the binary is replaced: a call
waiting for its turn when `beekeeper self-update` renames a new binary over its path re-executes
it, the same process, arguments, stdio and deadline, and the new code finds the merge's place by
its session, repository and number. It says `continuing under beekeeper <version>`. A call whose
devctl already runs re-executes the same way and follows the same devctl on from where its
stderr was copied (`following <merge>'s devctl (pid <n>) on`), so the installed release records
the outcome; the `merge-child` that hands the outcome to the owner re-executes before it does,
carrying the outcome. A lane that stays stuck anyway (a seed whose session is
gone) is flagged: `lanes` shows it `stalled` and `watch` says `LANE STALLED` once the first
arrived merge has waited `merge.stallAfter` behind places whose merges are not in the gate.

A merge survives its caller. A harness that stops a command kills its process tree, and a
session run as a unit takes its cgroup down with it, so the gate runs devctl outside both: a
transient user service (`beekeeper-merge-<repo>-<n>-…`, through `systemd-run`) runs the hidden
`beekeeper merge-child`, which runs devctl with the caller's environment and directory, its
document, stderr and exit code in files under the state directory (`merges/`); without a user
service manager, merge-child runs in a session of its own. The gate follows devctl's stderr onto
its own output and records the outcome. When the caller ends mid-merge (SIGTERM, SIGHUP, its pipes
closed), devctl merges on and waits for the release, and the gate waits on to record it; when the
gate is killed too, `watch` records the outcome from the files once devctl ended (`MERGE
RECORDED`, a `merged` or `merge.failed` event naming the gone gate). SIGINT, a person's Ctrl-C,
reaches devctl at once; a SIGTERM reaches it when it is aimed at the gate: its caller (the gate's
parent) is still there two seconds later and no SIGHUP came, so `kill <gate pid>` stops the merge. A
`TaskStop` of the background task a gate runs (the task's output file, recorded on the merge: the
gate's stdout, or that of the task's shell when the gate's output is piped on, `| tail`) is caught
by the PreToolUse hook before the stop's SIGTERM, which the gate cannot tell from its caller's
session ending: it ends a running merge's devctl through its merge-child and drops a waiting
merge's place (a seeded place stays), `merge.stopped` each, and the gate or the next watch tick
records the run. The stopping session is told the signal and each merge's outcome, as GitHub
reports it once devctl is gone: cancelled, nothing merged, it leaves the lane; or merged before
the stop. Before a pull request merge takes a lane place, and again at its turn before devctl
runs, the gate reads its reviews and comments afresh and refuses (77, nothing merged, the reviewer
and link named) while a reviewer holds it: a `CHANGES_REQUESTED` that is the reviewer's latest
verdict, which newer commits and replies leave standing and only their approval or its dismissal
clears, or a `COMMENTED` review or comment newer than the head and unanswered by the author or the
caller since (devctl's review guard drops a request for changes once a commit or reply is newer);
the author's and automation's own feedback holds nothing, and a read failing in transit is 74.
The review guard reads its login and all feedback through GraphQL, with a full
REST fallback when GraphQL cannot answer. Both paths paginate all feedback and
apply the same review rules; partial reads never authorize a merge. A rate limit
is also an unread result (74). The supervisor retries unread merge gates on a
budget-admitted pass instead of applying its general failure backoff, including
quota refusals recorded by an older version.
`beekeeper gate check <owner/repo> <n>` is that read alone, the gate's dry path: 77 naming the
reviewer, 74 unread, 0 when the reviews let the merge through; nothing queues or is logged.
`beekeeper gate runs` lists every run the gate runs outside its caller (a running merge or
promotion, one waiting in a queued run of its own, a devctl wait, also one its call left to run on):
its id, owning session, command, start, merge-child and devctl pids. `beekeeper gate stop <id>` ends
one, never a kill by pid: a merge as a `TaskStop` of its task does (a seeded place leaves too); a
wait's merge-child gets SIGTERM, which it passes on to devctl, killed when it has not ended 5 s
later. The wait's owner is woken with who stopped it, unless the owner did, and the stop is logged
(`merge.stopped`, `devctl.stopped`); its devctl gone, the run holds no lane place and no devctl
slot. Only a pid the run recorded is signalled. The watch records a run
whose gate is gone once its devctl wrote its exit code, also while merge-child still hands the
outcome over. A run that ends without its document or
by a signal (exit 128+n) is judged by GitHub (`gh pr view`), never by its exit code: merged, it
settles its lane with its release unconfirmed, for `merge.settle` from the pull request's
`mergedAt` when it is recorded after that (a hung run dropped or recorded late), and the gate
line names `devctl release wait <owner/repo> --pr <n>`; not merged, it leaves the lane; with
GitHub unanswered, the lane settles as for a lost merge.

A unit beekeeper starts (a merge-child, a wake's turn, a reopen) fails in the user manager only
on a real fault: devctl's documented codes, the gate's 74, 76 and 77 and a stop as asked end it
successfully, an unmapped exit or a kill fails it. An isolated harness state, whose stub tools
and deliberate kills are not faults, sets `units.quiet: true` in its configuration: every end of
its units then counts as success (`SuccessExitStatus` lists the exit statuses 1–255 and SIGKILL,
SIGTERM and SIGABRT), the real exit still in the run's `.rc` and `.result` files, and none reaches
the desktop's failed-unit alerts. The desk's own configuration leaves it unset.

devctl's blocking waits, `pr wait`, `release wait` and `rollout wait`, run the same way outside
their caller, without a queue, their files under `runs/`. One poller per command on the machine: a
wait whose identical command line already runs follows that run (the hidden `follow-run`, its
stderr, document and exit code) instead of starting a second devctl, so five sessions waiting on one
pull request cost the budget of one. Whichever of the four it is, its outcome reaches the session
that started it.

A caller still listening sees the output and exit code as ever; the gate then leaves merge-child a
marker. When it does not (a headless turn that ended with the command in the background, a caller
killed, its CLI gone), merge-child logs `devctl.unheard` and wakes the owner, on the roster or off
it (by its session id), with one line, `<command> exit N: <reason> (output in <file>)`, through
`agents wake`: by name when its CLI runs, else as a headless turn.

An owner whose session is gone (no transcript, no harness process) gets no wake and one
`devctl.unheard` line, and the issues an owner off the roster served go back to the board. The
reason is the document's verdict and reason (a merge's release), else devctl's last stderr line; the
output is kept under `merge-output/` for seven days. A second `devctl pr merge` of a pull request
whose merge runs is refused with exit 3, naming that run's start, owner and last line.

## Merges that release nothing

A merge that ends with nothing merged (red CI, a timeout, a refusal in devctl's document) always
reaches its worker, the same way: the gate leaves merge-child no marker, so the owner gets the wake
above also when the call listened (a foreground call reads the gate line as well), and a queued
merge's own run leaves it to the run that waits for it, so the owner is woken once. The line is
`devctl pr merge <owner/repo> <n> exit N: <verdict>: <reason>; failed: <checks> (output in
<file>)`, `failed:` naming the checks and CircleCI workflows the document reports failed that the
reason does not already name; the gate line and the `merge.failed` event carry the same reason.
`lanes` shows the failed attempt on its lane, `failed <owner/repo>#<n> by "<owner>" at <time>,
nothing merged, exit N: <reason>; shown until its next attempt`, until the pull request's next
`devctl pr merge` joins the lane or a merge of it is recorded; an attempt nobody retries leaves
after seven days.

A merge into a base branch without auto-release releases nothing by itself: a maintenance branch
whose tags are cut by hand, a fork line's backport branch. Before devctl's turn the gate reads the
pull request's base branch and the Auto-release workflow on it
(`.github/workflows/zz_generated.auto_release.yaml`), once per merge; when the workflow is absent
or its `on.push` branch filter does not name the branch, devctl runs with `--no-release-wait`, the
merge leaves its lane the moment devctl reports it merged (`merged … release none awaited
(<branch> has no auto-release)`), and the gate line and the wake text say no release is awaited.
A base branch with auto-release holds its lane until the release rolled, as above. GitHub not
answering for the base branch refuses the merge (exit 77).

A plugin marketplace tags per plugin instead (giantswarm/claude-code): without the generated
workflow, a `.github/workflows/auto-release.yaml` that runs when a pull request into the branch
closes and reads `.claude-plugin/marketplace.json` tags `<plugin>/vX.Y.Z` for every plugin whose
version the merge changed. devctl runs with `--no-release-wait` there too, and the gate then awaits
those tags itself, from the merge commit's manifest against its parent's, for up to five minutes:
all cut, they are the merge's release (`merged … release gs-plans/v0.3.0`); no version changed,
none is warranted; a tag still missing leaves the release unconfirmed, naming the tag.

A repository on the release-PR flow releases through a pull request of its own: without the
Auto-release workflow, `.github/workflows/zz_generated.create_release_pr.yaml` opens a
`<base>#release#<level>` pull request titled `chore(release): vX.Y.Z`, and
`.github/workflows/zz_generated.create_release.yaml`, which a push to the branch runs, tags its
merge. devctl runs with `--no-release-wait` there, and the gate line names the flow. Any other pull
request's merge cuts no tag and leaves its lane at the merge (`release none warranted`). The release
pull request's merge holds its lane while the gate runs `devctl release wait <repo> vX.Y.Z` for up
to 35 minutes: available, the tag is the merge's release and rolls as above; a failed tag pipeline
or commit status is a failed release; anything else leaves the release unconfirmed, naming the
command that resumes the wait.

## Stuck merges

A running merge whose gate process and devctl are both gone (killed, or lost with the machine in a
reboot) is lost: whether it merged is unknown. `watch` turns it into the lane's settling merge with
an unknown release, one `MERGE LOST` line and a `merge.lost` event, so the lane settles for
`merge.settle` and frees once its HelmReleases are Ready; `lanes clear <lane>` drops it at once.

A running merge whose devctl runs on after its pull request merged (a hung release wait) holds
its lane for nothing. `watch` asks GitHub about each run older than `merge.hungAfter` (45m)
and ends the devctl of one whose pull request merged longer than that
ago, or closed: SIGTERM to its merge-child, one `MERGE HUNG` line and a `merge.hung` event. The
run is then recorded like any devctl ended by a signal, by its gate or by the next poll: merged,
it settles its lane with its release unconfirmed. `lanes drop <owner/repo> <n>` does the same at
once for a running merge whose pull request merged or closed, and refuses one whose pull request
is open.

A `devctl pr merge`, or a `devctl release promote` of one repository, found running with no
beekeeper gate among its ancestors (the gate runs its command in a merge-child; a merge a script
ran past the hook has neither) runs outside the lanes, the holds and the budget floor, with
devctl's own bypass. `watch` says one `UNGATED MERGE` line naming the command, its pid, its session
and its parent while it runs and one `ENDED` line when it ends, `doctor` lists it as `ungated
merge`, and `snapshot` marks the wait `ungated`. Nothing stops it: a merge runs as its own Bash
command line, which the hook gates.

## Tool-release windows

A merge or promotion of giantswarm/devctl opens a tool-release window by itself: a `merges` hold
with giantswarm/devctl excepted, since the release makes every new devctl run refuse until updated.
Until the release is coming the window stops no other merge: while the devctl pull request waits for
its CI the installed devctl is the latest release and runs, so another repository's merge and
promotion proceed. Once GitHub reports the pull request merged (a promotion's window from its start)
the window holds them until the release is installed; a merge already running keeps its devctl,
which checked its version at its start. giantswarm/devctl's own next merge waits for the window's
release either way.

A merge that releases only a candidate (`vX.Y.Z-rc.N`) lifts it once the candidate's tag is
published with this platform's binary, while the merge's devctl still waits for the rest of the
release, or once the merge is recorded: `devctl version update` installs only a stable release, so
no devctl run outdates, and the candidate's promotion opens the window anew. `hold list` and `hold
check` name that condition as the window's end. Otherwise it lifts as soon as the release's binary
assets exist and verify, not after the release workflow devctl's own merge still waits for: once
GitHub reports the window's pull request merged (a promotion's from its start), the watch and the
waiting gates run `devctl version update` every minute, which installs a release only once its
binary verifies against its Sigstore bundle, and the window lifts once the local `devctl version`
reports the window's release (another version than when the window opened, while the release is
unknown), the devctl merge still running or not.

The merges held behind it then start on the new devctl. An update that fails (a release whose binary
never verifies) keeps the window, logged as `hold.update` and named on the hold (`beekeeper hold`, a
held merge's refusal: `its last update failed, the window stays: <reason>`). The window also lifts
once its pull request did not merge: the gate lifts it after a run with nothing merged or no release
warranted, and `watch` (or the next gate call) asks GitHub about a window whose merge ended
unrecorded (its gate killed) and lifts it when the pull request is open or closed; the waiting gates
and the watch share that read, one per 30 s over all of them, claimed in the state before it is
made.

The gate also runs `devctl version update` right after the merge's record (also when the window was
lifted by hand), a failure logged as `hold.update`. Every merge or promotion that runs devctl starts
with `devctl version update`, which installs the latest release when the local devctl is behind it
(a failure logged as `merge.update`), and the next merge of giantswarm/devctl waits while the
previous one's window waits for its release: devctl refuses to run behind its latest release (exit 7).
Nobody has to update devctl or lift the window by hand.

The window closes only when the stable release carrying the change is promoted and installed: a
merge that releases a candidate opens none, the candidate's promotion opens it, and it lifts once
the local `devctl version` reports the promoted release. A promotion of giantswarm/devctl queued
with a merge of it takes its place first in the lane while a window is open, whatever it joined
after, and does not wait for the window itself: the merges wait for the window and the promotion
is what closes it.

## The budget floor

The budget floor uses the last reading in the state when it is younger than `merge.budgetFresh`
(1m, less than one merge's draw at the floor's margin) and its reset has not come, else a fresh
conditional request, which costs nothing when GitHub answers 304; a failed read refuses.

A merge queued under the floor waits for the reset with that reset recorded on its place, and for
`merge.resetFirst` (5m) from the reset on the merges queued for it go first, the oldest queued
first: a gated `devctl pr wait`, `release wait` or `rollout wait` started then waits until none of
them waits any more (the gate's line names them), the watch's GitHub timer checks wait as well,
`beekeeper budget` names them and `budget --gate` refuses (exit 3). For such a merge only the
running merges count against `merge.cap`, and an older merge
queued for the reset that is free to start takes a slot before it. A merge leaves the window when it
starts; the waits run with what the merges leave, at the window's end at the latest.

## Urgent and paired merges

A merge whose delay keeps something exposed, a privacy or security fix's, is the supervisor's to
mark: `beekeeper lanes urgent <owner/repo> <n> --reason "<why>"`. Its gate runs it under the floor
instead of queueing it for the reset, as long as the budget keeps `github.urgentBound` (200) for
it, and logs it as `merge.urgent` with who asked and what its run drew against that bound
(`merge.urgent.spent`); `beekeeper budget` says the same. A merge already queued for the reset
picks the mark up at its next check. One urgent merge runs under the floor per reset window: a
second mark in the window, or while one waits, is refused with the reason, and a mark that reaches
its gate after the window's run is dropped (`merge.urgent.refused`) and its merge waits for the
reset like any other.

A change that needs two pull requests to land together, a credentials rotation split over two
repositories of one installation, names the other on each merge: `devctl pr merge <owner/repo> <n>
--update-branch --pair <owner/repo#m>` (the gate takes `--pair` off before devctl runs). Each keeps
its place in its own lane; the first to reach its turn waits there until the other is next in its
lane too, then starts with two `merge.cap` slots (and two of its party's `merge.sessionCap`), and
the other starts at its next check outside both caps. A refusal of either before its devctl runs
refuses the other with the same reason (`merge.refused`); a run that merged nothing is no refusal.
`beekeeper lanes` shows each place with the merge it lands with and where that is.
