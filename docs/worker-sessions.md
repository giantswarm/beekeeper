# Worker sessions

What a session beekeeper starts can do, how it runs headless or in the desktop, and how it
is reviewed, parked and archived.

## Worker profiles

A session beekeeper starts otherwise carries the person's whole interactive tool surface: the
desktop's tools (artifacts, the browser pane, questions to the person, workflows), every desktop-app
and claude.ai connector, every plugin's skills, agents and SessionStart hooks. `agents.profile`
names the profile the workers `agents start` (and `agents handover`) creates get, `guide.profile`
those of the guide's relay successors; `agents.profiles` defines the desk's profiles by name, and
beekeeper ships `minimal`: the built-in tools Bash, Read, Edit, Write, Skill, ToolSearch,
SendMessage, ListAgents, TaskStop, WebFetch and WebSearch, the plugin `beekeeper`, and the MCP
server `claude-in-chrome`.

A profile lists the built-in `tools` kept (`--tools`), the `plugins` kept (by name or id; every
other installed plugin, of any marketplace, is disabled through `enabledPlugins`, its skills,
agents, MCP servers and hooks with it), the `mcpServers` kept (`allowedMcpServers`, claude.ai
connectors included) and further `deny` rules (`permissions.deny`). beekeeper writes them to
`<state>/profiles/<name>.json` and passes the file with `--settings`, whose flag settings Claude
Code ranks over the person's user and project settings for that process only; the person's settings
files are never edited, and their hooks (beekeeper's own among them) keep running.

Claude Desktop spawns the CLI of a desktop turn itself, with its own tool surface, so a profile
reaches only the turns beekeeper launches. A profiled worker's task therefore runs as a headless
turn under the profile (`claude -p --session-id <id> --chrome --tools … --settings …`, the worker
prompt as its first prompt) and no seed turn runs; the session is imported into the desktop beside
that turn, past the window's focus, and its reopen warms the desktop's CLI once the turn ends, so
later desktop turns are the desktop's.

Every headless wake of one of beekeeper's starts runs under its role's profile too. `start` and the
log say `its task runs as a headless turn under profile <name>`; without a profile they say `its
task runs as a desktop turn`, as before. `agents start --dry-run` prints the profile applied, the
first turn's command line and the first prompt, and is judged by the same start gate a real start
is: the start gate's verdict prints with it (the auto choice, and with `--force` the verdict it goes
over), and a verdict without room refuses the dry run with exit 3 too, `--force` aside.

## Headless turns

`beekeeper agents` shows an agent in a headless turn (a fallback, a successor's first turn, a wake
turn) `live, first turn running` or `live, wake turn running` and says under its table how many
agents are in one; `--json` marks each with `headlessTurn`. While such a turn runs beside an import,
beekeeper stops the desktop's CLI of the session (it waits up to 15s for it): two CLIs on one
session id are two peers under one name. The same holds later: a headless resume (`agents wake`, a
task turn the desktop does not run, a hand-over's note turn) whose session's desktop CLI runs sends
the message to that CLI instead, and a desktop send that starts the desktop's CLI (a wake, the
standby's revive) waits up to two minutes for the session's headless turn to end and is refused
while it still runs.

The watch says `TWIN CLI` for any session that runs two CLIs anyway (the person opened its row),
naming each CLI's PID and directory. Once the headless turn has ended, its unit's `ExecStopPost`
starts `beekeeper agents reopen <id>` in a transient unit of its own
(`beekeeper-reopen-<id>-<reopen>`, 35 minutes of runtime at most, `RuntimeMaxSec`) and returns at
once, so the turn's unit stops within a minute (`TimeoutStopSec`, under the user manager's default)
and a shutdown never waits for a reopen's wait for the person.

The reopen shows the session in the desktop for a moment and switches back, so its desktop CLI is
warm again. It reopens only a start the roster still holds, never one a hand-over or `agents remove`
took off. One reopen waits per session (a lock under `<stateDir>/reopen/`): a later turn's reopen
leaves the showing to the one that waits. A waiting reopen reads the roster every 15 seconds and
ends, showing nothing, once its agent reported its work done, left the roster or is a relieved role
run; a desktop CLI of the session that started meanwhile (looked for every 30 seconds) ends the wait
too, kept as the warmed one.

A reopen the desktop did not take (the session not shown, its title not restored) is an
`agent.reopen` event and ends the unit successfully: the turn ended as it should.

A headless turn's end is the end of its process, so a background Bash it launched never wakes it.
When a start's or wake's turn ended with its task open on a background wait (a `run_in_background`
Bash its transcript holds no completion notice for, or a process the unit still runs), the reopen
resumes the session headless instead, once per task, with a turn that names the wait and tells it
to wait in the foreground; the event log says `agent.resumed-wait`, and the resumed turn's reopen
shows it in the desktop. A worker done, kept, or parked on a person is left alone, and so is a
`devctl` wait or merge the gate runs, whose outcome wakes its owner by itself (`devctl.unheard`).

## Desktop import and archiving

A session the desktop never imported has no row in its sidebar: nobody sees it there, reads its
transcript or types into it, and the roster and the event log are its only signs. The usual cause
is the desktop's cap of CLIs: at the cap the import, the show and the reopen start nothing
(`makeRoom`), the task runs headless, and the reopen after it misses. `beekeeper agents` marks such
an agent `no desktop row` in REACHABLE (`--json`: `noDesktopRow`) and counts them under its table;
the watch says `NO DESKTOP ROW` once per worker, with what shows it: the standby watch imports one
whose headless turn runs beside the turn (above), and the doctor reopens a worker whose CLI does not
run once the desktop runs fewer CLIs than its cap, `beekeeper agents reopen <id>` in a transient
unit `beekeeper-reopen-<id>-<n>` of its own, which gives the session its row and warms its CLI, one
`DOCTOR reopens …` line and an `agent.reopen` event each, with no person acting. It starts no
reopen within seven minutes of the start (a start imports by itself), beside a start's or wake's
unit still running its turn or its reopen, or beside a reopen of its own, and counts the reopens
under way against the cap; `doctor --dry-run` names the workers it would reopen and those the cap,
or a desktop that does not run, leaves without a row.

The desktop handles each `claude://resume` link twice. When the second delivery arrives while the
first import still runs, both import, the second drops the transcript's title and model as stale
(the first touched the file), and the desktop keeps its untitled record: the session shows
untitled in the sidebar and to ListAgents under a default name (`<dir>-<n>`), which a message by
its name does not reach, even when the record file showed the title for a moment. The desktop
rereads neither the transcript nor its record files, and a record without a model gets no CLI
when the desktop shows it (only a preview shell), so the session cannot be asked to retitle
itself. Every CLI the desktop runs, though, has the desktop's session tools
(`mcp__ccd_session_mgmt__set_session_title`, `archive_session`), and they act on any session by
its id. So `agents reopen` checks the record once the first turn has ended and, when it lacks the
roster name, asks a steward through its socket (`$XDG_RUNTIME_DIR/cc-socks/<pid>.sock`) to set
it, then waits up to 80 seconds for the desktop to record it. A steward is a model and can
decline a request from another session, so an unanswered request goes to the next steward, three
at most (within the reopen unit's `RuntimeMaxSec`). The steward is the session's own desktop CLI
when the desktop warmed one. Otherwise it is the idle desktop CLI of another session beekeeper
started, a finished worker off the roster before an idle roster agent (whose brief can forbid the
call), each idle longest: transcript quiet for
30 seconds, no tool command, no headless turn, no task on the roster. It is never the supervisor
or the guide, nor one relieved within 7 days (it follows its role's rules still), nor
a session its person started, nor a session handed over (a later start or another roster
session carries its name): a request would have the desktop run a turn of it, and start its CLI
again, beside its follow-up under the same name. A title set that way is the desktop's "set
by an agent", which its own titling never overwrites.

`agents remove` archives the removed agent's desktop session the same way (`archive_session`;
the desktop's Archived list brings it back), and prints and logs (`agents.archive`) what it did.
It does so only for a session beekeeper started that runs no turn and holds or held no role, unless
a relay relieved it;
`--keep-desktop` leaves it in the sidebar. When no other steward is idle, the session's own
desktop CLI archives it: beekeeper shows the session in the desktop for a moment once the person's
typing pauses, as a reopen does, which warms its CLI, and the window then shows the session it
showed before. When that cannot be done (the desktop does not run, the person keeps typing, the
desktop at its cap of CLIs) it says so and the removal still stands; the doctor then owes the
archive (below), and a run that asked no steward counts none of its tries.

Beekeeper archives finished workers automatically through a steward. The steward first runs
`beekeeper agents archivable <local_id>…`, which confirms each session is a finished worker
beekeeper started, off the roster, unarchived and running no turn. A session the person started
is never confirmed; the command exits 3 for any session it refuses. The steward archives only
the sessions it confirms. An archive
counts once the desktop records it, and the line names the steward whose `archive_session` call
did it. A steward that archived nothing is read from its turn: one that declined is reported with
the first line of its reply (`agents.archive`, "steward local_… declined to archive: …") and asked
for no archive for 24 hours; one that did not answer is reported as such.

After the doctor gives up an archive, run `beekeeper doctor retry-archive <session>`
with its agent session ID or desktop session ID, then `beekeeper doctor` (or wait for
its next watch poll). This queues the archive again with a fresh five-turn, 24-hour
budget, including after installing a fixed build. It accepts only beekeeper-started
desktop sessions off the roster and holding no role. Busy-session
checks still apply; another exhausted budget records the give-up as before.

## The doctor's chores

`beekeeper doctor` does these chores by rule, and every watch poll (not `--once`) runs it in the
background, one `DOCTOR` line per thing it did. It takes an agent off the roster once it reported
its work finished with `agents idle --done` and its CLI runs no turn, once it was relieved of the
supervisor's or the guide's role (a relieved role relays, it never hands over), or once it stayed
idle `agents.staleAfter` (24h) with no CLI running; the desktop sessions beekeeper started for the
finished and the stale ones are archived in one steward's turn, under the rules above. An entry
kept on purpose is never removed for staleness and its desktop session never archived by the
doctor: `beekeeper agents keep <agent> [--until <time>] [--reason <text>]` marks a judge or
reviewer session a person returns to, a spare or a worker parked on a long external wait, and an
open timer that wakes the agent by name (`timer add --wake`) keeps it the same way until the timer
is done. `agents` shows what keeps an entry in its KEPT column, `doctor --dry-run` names each kept
entry it leaves, the watch says no `AGENTS STOPPED` line for one, and `board next` counts its
`sessions serve` record as covering its item. `agents keep <agent> --no-keep` lifts the marker and
returns the entry to `agents.staleAfter`.

An entry whose session queued a report for its current assignment (`agents idle`, `agents park`,
`agents problem`, `note add`, `sessions serve`; see [session reports](session-reports.md)) stays on
the roster while that report waits in the inbox: taken off first, the loop would reject the report
as one of an ended assignment. `doctor --dry-run` names the waiting report; once the loop applied
it, a done report retires the entry as finished. The doctor and the loop read the inbox again under
the state lock before they take an entry off.

## Reviews

A Claude worker's security reviews follow its model. The security-guidance plugin reviews a
session's Stop, commit and push in a session of its own (entrypoint `sdk-py`) on
`SECURITY_REVIEW_MODEL`; every headless turn beekeeper starts or wakes on a model (`--model`,
`agents.models`, the task kind's) carries that model as `SECURITY_REVIEW_MODEL` in its `--settings`
(added to a per-model copy of a profile's settings file), which outranks the person's settings.

A turn without a model, and a desktop turn, keep the person's setting. `agents` lists each worker's
reviews in its SDK-PY column, as their count, cost and models (`--json`: `sdkChildren`, one entry
per review): the `sdk-py` sessions of the worker's transcript folder since it registered, each owned
by the worker in that folder that registered last before its first request.

A task holder with no running CLI or turn and no transcript or roster activity for
`agents.staleAfter` is **gone**. Kept and parked agents, agents with a wake timer, and remote
executions are protected. `capacity` releases its busy and provider slots; `agents` and the watch
show its last activity. The doctor retires it through the usual archive path, ends its serve record
so the board can offer its item again, and logs its unfinished task and latest hand-over note as an
`agents.gone` event without asking for a decision.

Before retirement, a wake, resume or new activity restores its slot. Setting `agents.staleAfter`
negative disables stale retirement. The doctor also reopens a worker whose session the desktop never
imported, once the desktop runs fewer CLIs than its cap
([above](agents.md#agents-started-without-a-click)).

`beekeeper agents review <owner/repo>#<n> --verdict` prints the pull request's desk
verdicts as the loop and the merge gate read them, with the head each judged (exit 3
when there is none): GitHub refuses APPROVE and REQUEST_CHANGES on a pull request of the
reviewer's own login, so a verdict there is a COMMENT review with no review state. The
session that started a review gets its verdict: the watch wakes it with the reviewer's
final report.

`beekeeper agents review <owner/repo>#<n>` starts a fresh independent reviewer
in a checkout of that repository. It requires green CI at the pinned head. A
pull request gets one desk review
([#3861](https://github.com/giantswarm/beekeeper/issues/3861)): the command refuses
(exit 3) once the pull request has a desk verdict on any head, or an assigned round
reported one, and refuses a second session while its review runs, even after a push, including
after a restart. An assigned reviewer that is gone without a result (done or off the
roster, no live process, no running remote execution) no longer holds its head: the loop's
next start of that review releases the assignment, logs `review.released` and starts
the next round's reviewer, since the one review has still to come. For a pull request parked
for desk review, the loop starts that round itself 15 minutes after the gone reviewer's round
opened (a failed remote start, too), within the rounds a pull request gets while none gave a
verdict. The brief includes closing issues, their parent epic and the round. The shipped [review skill](../plugin/skills/review/SKILL.md)
checks acceptance criteria and behavioural coverage as well as correctness.
`--model` overrides `supervisor.reviewModel` (default `opus`).
`--light-lines` overrides `supervisor.reviewLightLines` (default 150): smaller
PRs and docs/tests-only PRs use diff, issue and CI without checkout or build.
Other reviews use an independent detached worktree and trust green CI on that
head; only the reviewer's own probes need local builds.

The comment's first line is always `Desk review (opus): approved` or
`Desk review (opus): changes requested`, regardless of model. The next line pins
`Head reviewed:` to the SHA. The reviewer fixes its own findings on the branch,
pushes one commit and approves the head it pushed, naming that SHA, without waiting
for its CI; the loop gives that head the merge go once it is green. Findings the
reviewer cannot fix (a product or design decision, or a refused publication) go
back to the worker, who fixes them, pushes and parks again; its green fix gets the
merge go without a second review. Nobody merges the base into an approved branch or
rebases it to bring it up to date: the loop merges the approved head as it is, and
the merge-result check against the current base is the gate. A real conflict or red CI on the
reviewed branch returns to the reviewer for a repair turn; a red merge result goes
back to the worker. The repaired head merges on the same approval. The supervisor may still
attest a nit-only push with
`beekeeper --as <supervisor> agents review <owner/repo>#<n> --nits-of <approved-full-sha>`,
for a head no park asks the go for. Workers cannot attest their own fixes.
`merge.requireReview: true` requires the fixed approval comment covering the
head (including a supervisor-attested nit delta); a merge go alone is insufficient.
Review-raised issues use the `follow-up` label, stay unranked in backlog and are
never attached as epic sub-issues. `board next` skips that label by default.

A park on a pull request requests the supervisor loop's independent review only
when its reason starts with "desk review", the pull request is not a draft,
and it runs from that pull request's worktree branch at its published HEAD. The
command records and prints the verified head. Other parks, including older parks
without this evidence, are plain waits: no reviewer, merge go or findings
forwarded; the watch resumes them when the pull request merges or closes. If
publication could not be verified, request review in one shell call:
`cd "<worktree>" && beekeeper agents park --on <owner/repo#n> "desk review"`.

The park hands over the head it verified, and a head that only merges the base into it. A head
its author pushes past the park is still being worked on: it gets no review round and no merge
go until the author parks on it, and the loop says so once in a `supervisor.leave` line. A head
the author moves past a running round's ends that round without a verdict: a review its reviewer
posts afterwards on the round's head (GitHub attaches it to the newer head) is no verdict, its
findings reach nobody and are not the next round's previous findings. The next round starts on
the new head as soon as its author has parked on it, without waiting for the old reviewer, which
the loop takes off the roster once its turn ends. Only the last of the two rounds a pull request
gets still waits for its reviewer's done.

## Parking

A worker that waits on a person, a merge lane or a release parks instead of sleeping in a turn that
holds its slot and its CLI's memory: `beekeeper agents park [--on <#note|<context>#note|owner/repo#n>]
"<what it waits for>"` (`<context>#note` a central instance's note) keeps its task, shows it parked
in `agents` (KEPT) and `capacity`, not busy, and it ends its turn; an agent without a task is
refused. `lease claim <resource> --park` parks it on the lease its queued claim waits for.

A GitHub target is resolved once when parking; a confirmed missing reference is refused. A failed
read still records the park with an unresolved type, classified later with retries at most every
five minutes. A pull request enters the usual review flow. An issue is a plain wait, checked at most
every five minutes, and never read as a pull request by the supervisor; existing parks are
classified on their first read and keep that type across restarts.

When a worker parks on an issue it serves, an open pull request linked to it under the same GitHub
identity is suggested for desk review when available. Once the note is closed (answered, defaulted,
done or overtaken), the issue or pull request merged or closed, or the lease free and the agent's to
claim (a grant to another session first keeps it parked), the watch says one `AGENT RESUMABLE
"<agent>"` line with what settled it, a person's answer word for word.

With `agents.autoResume` the next poll resumes the agent with that answer as `agents wake` does (by
name to a running CLI, else headless); without it, or for a park without `--on`, `beekeeper agents
resume <agent>` does it by hand. A resume that cannot wake the agent leaves it parked (`AGENT RESUME
FAILED`); `agents idle` ends the park with the task. A parked background session beekeeper started
(`agents.mode: background`) costs no memory: once its turn ended (no command, no headless turn, its
transcript and its subagents' quiet for 30 seconds), the watch stops it (`claude stop`, which keeps
its conversation; `PARKED SESSION STOPPED` and an `agents.parkstop` event), and the wake or resume
that ends the park continues it from its transcript (`claude --bg --resume`).

A task has one worker. When two hold it (a hand start beside the loop's, a relay, a continuation)
and one reports it done (`agents idle --done`), the parks of the others on the same task issue are
retired with that report: their tasks end as done, their serve records and the timers that wake
them go, an `agents.park-retired` event says so and the doctor takes them off the roster. The
issue's close then wakes nobody, also a park it made resumable before the report, and no fresh
continuation starts. A park whose continuation is already starting, a desk review or CI park (the
loop owns that pull request's flow) and a role holder's park stay.

The stop and every wake of the session hold one lock per session: under it the stop looks again (the
same park, still quiet, not woken in the last 30 seconds, and the transcript's last message of its
own answers the last message and stopped — a model thinking over a wake's or resume's message writes
nothing, and that turn is never stopped) and holds the lock until the CLI exited, so a wake that
comes meanwhile waits and then resumes the stopped session.

A wake is never silently undone. A stop that fails says `PARKED SESSION STAYS` and is tried again
the next poll. The supervisor's and the guide's sessions, a park already resumable and an agent on
an execution host stay. An archive that stayed (the CLI ran a turn, no steward recorded it, also
after `agents remove`) is owed, and so is, once, every finished worker beekeeper started whose
desktop record stayed unarchived, its CLI running or not: the doctor asks for at most 20 of them per
run, while the CLI runs no turn, in the same steward's turn, until the desktop records it, up to 5
stewards' turns 10 minutes apart within 24 hours, each try a `DOCTOR` line and an `agents.archive`
event with its reason; an agent back on the roster or the session archived by hand is owed nothing.

The old session of an `agents handover` and the run of the supervisor or the guide a relay relieved
(once its successor took the role) are owed the same way, so a relieved run frees its desktop CLI
slot. A session beekeeper started whose desktop record shows another title than its roster name gets
the name back through a steward, at most every 30 minutes per session. Each fault of `doctor.faults`
is probed (the probe exits 0 while the fault is absent); a failing one whose remedy may run
unattended is remedied and probed again, any other only with `doctor --fault <name>`, and a fault
still failing is one note for `guide.person`, closed once its probe passes, and one `DOCTOR FAULT`
line while it lasts.

The Go build cache every session's builds share (`$GOCACHE`, else `~/.cache/go-build`) is kept under
`doctor.goCacheMaxGiB` (20 GiB): Go itself drops only entries unused for five days, so a busy
machine's cache grows without bound. Every watch, the standby watch included, reads its size every
`doctor.goCacheEvery` (1h) and, over the cap, removes the least recently used entries down to three
quarters of it; it does not start while one of the user's `go` commands runs and stops when one
begins, trying again five minutes later, until the cache is past the cap by a quarter: then it trims
whatever `go` command runs outside a `beekeeper run`, since the go command takes a removed entry for
a miss and builds it again.

No trim runs while a `beekeeper run` holds the cache: every run holds the cache's use lock
(`beekeeper-use.lock` at the top of the cache) shared for the length of its command, and a trim
takes it exclusive for the length of the trim. Under the margin a trim the runs hold back tries
again five minutes later; past it, the trim waits for them as `beekeeper cache clear go` does, up to
five minutes, new runs waiting behind it, so overlapping runs never hold it off for ever.

A run that begins during a trim waits for its end, up to its `--wait`. One process trims at a time,
and each trim is one `gocache.trim` event with the size before and after; a cache that stayed over
the cap across two `doctor.goCacheEvery` intervals is one `GO CACHE` line with its size, the cap and
how long it has waited, as is a cache it cannot read. `beekeeper doctor` trims it the same way
(`--dry-run` says the size). An explicit cache clear uses `beekeeper cache clear go [--modcache]`,
which takes the use lock exclusive: it waits up to `--wait` (1h) for the runs that hold it, holds
back the runs that start meanwhile, runs `go clean -cache` (and `-modcache`) and logs one
`gocache.clear` event.

The hook refuses `go clean -cache`, `go clean -modcache` and a delete of either cache's directory,
of every entry in it or of a directory that holds it, naming the command: a build running meanwhile
fails on the entries it loses. Docker's images and build cache, which dev images loaded into kind
labs pile up, are kept under `doctor.dockerImagesMaxGiB` (40 GiB) and
`doctor.dockerBuildCacheMaxGiB` (20 GiB): every `doctor.dockerEvery` (1h) a watch reads `docker
system df` and, over a cap, removes the unused images created or last tagged longest ago (dangling
ones first; the tag time is `docker image inspect`'s `LastTagTime`, as an image rebuilt from cache
or retagged keeps its old Created) down to three quarters of it, and trims the build cache record by
record (`docker builder du` by last use, `docker builder prune --filter id=…`) likewise: Docker's
own `builder prune --max-used-space` is a best-effort pass of the daemon's GC policy and frees
nothing while the policy's reserved-space or min-free-space floor sits above the target, which on a
nearly full disk is always.

The images a running kind lab's node holds (`crictl images` in each node, by ID, tag or digest) go
last: the node keeps its own copy, though a lab created again loads it anew. Each removal is counted
by the image's whole size, shared layers included, which is at most what it frees, and once that
count says the target may be reached `docker system df` is read again: the prune stops where Docker
says the target is reached and goes on while it says more.

The images Docker refuses to remove (a child image still built on one) are tried again, the newest
first, as long as a pass removes one. An image a container uses (a kind node's included) is never
removed, nor one created or tagged within the last 24 hours, or since the oldest held lab lease was
claimed (72 hours back at most): a session's dev image waits for its lab. A prune that leaves the
images over their cap names the largest images that kept them there, with why (a container uses it,
in flight, or Docker's refusal), and is a `DOCKER` line; build cache records left over their cap in
use or unreadable are named with their space.

Nothing is pruned while an image import or build runs (`kind load` or `create`, `docker build`,
`load`, `save` or `import`, buildx, `ctr … import`), and none while a lab lease was claimed within
the hour, its holder possibly mid-load; the prune tries again five minutes later and stops between
images when an import begins. One process prunes at a time, and each prune is one `docker.prune`
event with what it reclaimed and the images it removed; a prune that waited `doctor.dockerEvery`, or
a Docker it cannot read, is one `DOCKER` line.

`beekeeper doctor` prunes the same way (`--dry-run` says the sizes). What dead sessions and dead go
commands leave in the tmpfs `/tmp` is RAM nothing gives back: every `doctor.freeEvery` (1h) one
watch runs `beekeeper free --apply --only session-dirs,go-build` by itself, under a lock, the
watches sharing when it ran last. It removes a dead session's scratch (its CLI gone, also a newer
one of its desktop session; no roster agent with its task and no role holder is the session; the git
worktree its transcript names is gone; untouched for three hours; no process works or holds a file
inside) and an orphaned go-build dir (no go command of the user running that began before its last
change, unchanged for an hour, nothing held inside).

A run that freed something is one `FREED` line with the count and size of each and one `free.apply`
event; a failed one is `FREE FAILED` until a run succeeds; the `TMPFS` line says when it ran last.
`beekeeper doctor` frees the same (`--dry-run` says what it would); a negative `doctor.freeEvery`
turns both off. `beekeeper snapshot` lists the top three immediate child directories across
`~/.local/state`, `~/.cache`, and Docker's configured data root (falling back to `/var/lib/docker`).

Each entry shows logical file size and growth since that caller's previous snapshot; JSON retains
all measured directories. The watch samples every two minutes and adds the fastest-growing measured
path to `DISK FILLING` alongside the writing processes. These are independent measurements, not
proof that a particular process wrote that path. Scans read metadata only, skip symlinks, and stop
at six directory levels, 50,000 entries, or a two-second deadline (including Docker root discovery).

Unreadable or truncated trees show partial lower bounds; growth is unknown until two complete
readings exist. Missing or inaccessible roots are skipped.

## Desktop turns and reopens

The desktop's import takes the session's model from the transcript's last reply and falls back to
its own default model without one. So beekeeper imports the session only once the transcript
holds its first reply and has stayed unchanged for 2 seconds (up to 5 minutes), and says which
model the desktop recorded for the desktop turns. Claude Desktop on Linux currently handles each
`claude://` link twice, and the second of the two concurrent imports records no title and no
model. So once the import is done, the start has a steward, an idle desktop CLI of another
session beekeeper started (the desktop refuses a session's switch of its own model), set what
the record dropped: the title to the agent's name and the model to the one its first turn ran
on (`set_session_title`, `set_session_model`), and says which steward did, or why none could;
the reopen after the first turn tries again for a model still missing.

The import switches the desktop's main window to the new session. Once it has (up to 15s),
beekeeper switches the window back to the session it showed before, the last focus change in the
desktop's log (`claude.desktopLog`, `~/.config/Claude/logs/main.log`), so the person keeps working
where they were; the new session waits in the sidebar. A desktop that showed no session, or that
the start had to launch, stays on the new one, and so does one that showed a relieved role run or
a hand-over's predecessor: the desktop warms the CLI of the session its window shows, and the
watch would stop that CLI as revived after every start, reopen and wake.

Neither link goes while the desktop's window has the focus (Hyprland's active window, `hyprctl
activewindow`, app id `com.anthropic.Claude`): a switch there would land under someone reading or
typing, whose keystrokes then reach another session. Nor does one go until the person's keyboards
and pointers have been idle for `desktop.typingQuiet` (30s): a stray keystroke would go into the
window the link opens. beekeeper reads only when an event arrives on `/dev/input/event*` (the
devices with keys or relative motion; the user needs the `input` group), never which, and grabs no
device; it watches from the start, so a first reply after quiet input imports at once. Input that
cannot be read stops the start before it runs anything, with the reason; `desktop.typingQuiet: -1s`
opens the links without watching the input. The import waits up to 2 minutes for the
window to lose the focus and the input to go quiet; past that the start ends without it
(`not imported yet`, with what held it), and the reopen
after the first turn imports the session instead, waiting up to 25 minutes more (the reopen unit's
`RuntimeMaxSec` covers the wait). While it waits, the agent records what holds it and until when:
`agents` shows it `importing, until <t>`, the watch says `IMPORT WAITS` once per
wait, and a `lease grant` or a message by name to the agent says that no CLI of it runs and that
its import is pending. A reopen that waits it out is recorded as missed. Without a Hyprland session
there is no focus to ask, and the links go at once. A locked screen is no focus: under a running
screen locker (hyprlock, swaylock, gtklock, waylock) the compositor still names the window focused
before, nobody reads or types there and keystrokes go to the locker, so the links go at once,
without the `desktop.typingQuiet` wait.

A worker whose next turn needs the desktop (its browser: the Claude in Chrome tools exist only in a
desktop CLI, never in a headless turn) does not wait for the window's focus. It is started with
`agents start --desktop`, or asks with `beekeeper agents desktop` before its headless turn ends
(`beekeeper agents desktop <agent>` asks for another agent): its import and reopen wait only for the
person's input to pause, 1 minute at most, show the session for a moment and switch the window back.
A reopen already waiting goes ahead within a second, and an agent with neither a CLI nor a waiting
reopen is shown at once; the ask ends once the desktop runs its CLI, so its next message or grant
reaches that CLI.

A reopen checks for the CLI its show warms (up to 15s). The desktop runs at most a cap of CLIs (its
`CliGovernor`, 28 here): at the cap it starts none for a show and logs `at cap=<n>; yielding warm
spawn`. The reopen reads that line and reports it as missed (`the desktop warmed no CLI … it runs
its cap of <n> CLIs`), and the agent's ask for a desktop turn stays: the CLI starts once a person
opens the session or one of the desktop's CLIs ends (`beekeeper free` lists the stale ones).

## Permission modes

A follow-up by message crosses permission modes: the started session runs in `acceptEdits`, a bypass
supervisor sends to it, and it answers back. Claude Code decides each cross-session message on the
receiving side by its `crossSessionInbound` setting; unset, it holds a message from the other
permission class for the person's approval, and a headless receiver lets it expire. Set it once in
the user-level `~/.claude/settings.json` (a project or local settings file can only tighten it):

```json
"crossSessionInbound": "accept"
```

Every message between the user's own sessions is then delivered, whatever the two modes; each
session's tool permissions stay its own.

Claude Desktop's import turns `bypassPermissions` into `acceptEdits` for every desktop turn, with
no setting to change that, and raising the mode again takes the person's approval card each time.
So in its desktop turns such an agent would stop at the first request no allow rule covers.
`beekeeper hook permissionrequest` answers those requests.

**What the hook allows:** every request that would otherwise show a permission card, and nothing
else, only in sessions whose id `beekeeper agents start` recorded together with the mode
`bypassPermissions` it passed at start, and only while such a session runs in `acceptEdits`, the
mode the import gives it. That is exactly what the session's bypass start already allowed. Its
subagents share its session id and are answered the same way. Each allow is a `hook.allow` event
in `beekeeper log`, naming the session and the tool.

**What it leaves to the person:** every other session gets no answer and the normal card: desktop
sessions, sessions started any other way (a `claude -p` in bypass that beekeeper did not start
included), beekeeper's starts the person set to `default` or `plan`, and starts older than 30
days. Deny rules still win: Claude Code refuses a denied call before it asks, so the hook never
sees it. The desktop's own consent cards (raising a mode, deleting a session) are the app's, not
permission requests, and the hook cannot answer them. On malformed input or an unreadable
configuration or state the hook gives no answer, never an allow; a request in any mode but
`acceptEdits` is decided without reading the state, and the state is read without its lock, so
a permission request never waits on beekeeper.

A session cannot add itself: its id enters the record only through the start that created it,
written under the state lock before the session existed.
