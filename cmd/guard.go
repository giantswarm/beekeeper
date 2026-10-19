package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/guard"
	"github.com/giantswarm/beekeeper/internal/knownhosts"
	"github.com/giantswarm/beekeeper/internal/lease"
	"github.com/giantswarm/beekeeper/internal/machine"
	"github.com/giantswarm/beekeeper/internal/platform"
	"github.com/giantswarm/beekeeper/internal/post"
	"github.com/giantswarm/beekeeper/internal/proc"
	"github.com/giantswarm/beekeeper/internal/sandbox"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/takeover"
)

// memcapCPU is memcap.slice's CPU budget from the configuration, the part
// of a run's cap the host decides: for its own runs, the broker's and
// install's slice unit.
func (a *app) memcapCPU() platform.Cap {
	return platform.Cap{CPUQuota: a.cfg.MemcapCPUQuota(runtime.NumCPU()), CPUWeight: a.cfg.Memcap.CPUWeight}
}

// buildSlots is the build slots' directory and count, the one reading every
// taker and reader of a slot shares (beekeeper run, a start that builds, the
// capacity, snapshot, free and ui views): MEMCAP_STATE (the directory
// holding slots/) and MEMCAP_SLOTS over memcap.slotDir and memcap.slots. A
// MEMCAP_SLOTS that is no positive number is a usage error, the configured
// count returned with it for a reader that only shows the slots.
func (a *app) buildSlots() (dir string, n int, err error) {
	dir, n = a.cfg.Memcap.SlotDir, a.cfg.Memcap.Slots
	if s := os.Getenv("MEMCAP_STATE"); s != "" {
		dir = filepath.Join(s, "slots")
	}
	if s := os.Getenv("MEMCAP_SLOTS"); s != "" {
		v, err := strconv.Atoi(s)
		if err != nil || v < 1 {
			return dir, n, usageErr("MEMCAP_SLOTS=%q: want a positive number", s)
		}
		n = v
	}
	return dir, n, nil
}

// slotView is buildSlots for a reader that only shows or names the slots.
func (a *app) slotView() (dir string, n int) {
	dir, n, _ = a.buildSlots()
	return dir, n
}

func (a *app) runCmd() *cobra.Command {
	var maxFlag, swapFlag, waitFlag string
	c := &cobra.Command{
		Use:   "run [--max SIZE] [--wait DURATION] -- command [args...]",
		Short: "Run a build, test or lint command in a build slot under a memory cap",
		Long: `run takes one of the machine's build slots and runs the command in a
transient systemd user scope in the slot's slice under memcap.slice, both
with MemoryMax=SIZE and MemorySwapMax=0, the scope with OOMPolicy=continue.
A run of a Claude Code session that holds a slot already (a lab proof in
the background, a test suite in the foreground) takes no second slot: it
joins the session's slot at once, its scope in the same slice, and shares
the slot's cap. A command a capped run starts runs inside that run's scope. When the command's process tree
reaches the cap the kernel kills its biggest process and nothing outside the
scope notices; run then names the victim in one line starting with
"` + guard.LogPrefix + `" and exits 137 (or the tool's own failure code).
Bound the command's parallelism rather than raising --max.

When every slot is held, or MemAvailable is below the cap, run waits up to
--wait in the build slot queue it shares with agents start --builds: a
ticket per waiter under <slotDir>/queue/, the slots taken first in first
out (the memcap wrapper takes none), one line when the wait starts and one
when it ends. A wait that runs
out exits 75, states the wait it applied and lists the holders: run the
command in the background instead, never poll. The wait is --wait, else
MEMCAP_WAIT, else 8m, or 60m in a background Bash call: the PreToolUse hook
exports MEMCAP_BACKGROUND=1 into every background call, so a run reached
directly, after a cd or export chain or through a script gets the 60m.

The slots are memcap's flock files, shared with the memcap wrapper. The
command's arguments reach it verbatim: systemd-run's own ${VAR} expansion is
off. Without a user systemd (containers, CI) the command runs uncapped. In
the agent sandbox, which keeps the user systemd out of reach, the host's
broker (beekeeper sandbox broker) caps the slot and moves the command into
its scope, the command still in the sandbox; with no broker answering, run
refuses (exit 1) rather than run a build uncapped.

For the length of the command a run holds the Go build cache's use lock
shared, so no trim of the cache (doctor.goCacheMaxGiB) removes an entry a
compile reads; a run that begins during a trim waits for its end, within
the same wait.

A capped run gives the command its own temporary directory under
<stateDir>/tmp/ on disk, named by GOTMPDIR and TMPDIR, and removes it
after the command: the go command's work directories (go-build*) and a
test's files stay out of the tmpfs /tmp, whose files take RAM. A caller's
GOTMPDIR or TMPDIR off tmpfs wins, so a nested run keeps its outer run's
directory. A leftover of a run that never cleaned up goes after a day.

Every capped run leaves a run.start and a run.end event in beekeeper log,
naming the scope, the session and the command, so that a cap kill found
later (snapshot, watch) names them after the run has ended. Logging never
fails or delays the run: an event the log cannot take within a second is
dropped.

The runs share memcap.slice's CPU budget: its CPUQuota (memcap.cpuQuota,
default half the cores) bounds the cores they use together, its CPUWeight
(memcap.cpuWeight, default 50 against the desktop's slices' 100) their
share while the desktop wants the cores too; run sets both at every slot it
takes, the slots share them by equal weight, and the command starts at nice
10. snapshot and free show the slice's budget and use.

Environment: MEMCAP_MAX (memcap.max, default 14% of RAM), MEMCAP_SWAP (0), MEMCAP_WAIT (8m, 60m with MEMCAP_BACKGROUND=1),
MEMCAP_SLOTS and MEMCAP_STATE (the directory holding slots/) override the
configuration; the flags override the environment. MEMCAP_TEST=1 marks a
test's run: its scope is memcap-test-…, and snapshot and watch report a
kill in it as a test kill, not a build's.

SSH_KNOWN_HOSTS: when the caller sets none and ~/.ssh/known_hosts or
/etc/ssh/ssh_known_hosts holds a line Go's knownhosts cannot parse (a host
key type it does not know), the command gets SSH_KNOWN_HOSTS naming a copy
of both without those lines, <stateDir>/ssh/known_hosts, so that go-git's
SSH transport, which refuses a whole file for one such line, reads it. The
machine's files stay untouched.`,
		Args: cobra.MinimumNArgs(1),
		PersistentPreRunE: func(*cobra.Command, []string) error {
			return a.loadConfig()
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			cpu := a.memcapCPU()
			o := guard.Options{Max: env("MEMCAP_MAX", a.cfg.MemcapMax(ramMiB())), Swap: env("MEMCAP_SWAP", "0"),
				CPUQuota: cpu.CPUQuota, CPUWeight: cpu.CPUWeight,
				Stderr: os.Stderr, Record: a.runRecorder(), Test: os.Getenv("MEMCAP_TEST") == "1",
				GoCache: goCacheDir(), TmpDir: filepath.Join(a.cfg.StateDir, "tmp")}
			var err error
			if o.SlotDir, o.Slots, err = a.buildSlots(); err != nil {
				return err
			}
			if inSandbox() {
				exe, _ := os.Executable()
				o.Sandbox = sandbox.Capper{Dir: sandbox.SpoolDir(a.cfg.StateDir), Exe: exe}
			}
			f := cmd.Flags()
			if f.Changed("max") {
				o.Max = maxFlag
			}
			if f.Changed("swap") {
				o.Swap = swapFlag
			}
			var wait string
			wait, o.WaitFrom, o.Background = guard.ResolveWait(waitFlag, f.Changed("wait"))
			if o.Wait, err = guard.ParseWait(wait); err != nil {
				return usageErr("%v", err)
			}
			for _, s := range []string{o.Max, o.Swap} {
				if _, err := guard.ParseSize(s); err != nil {
					return usageErr("%v", err)
				}
			}
			a.parseableKnownHosts()
			if rc := guard.Run(o, args); rc != 0 {
				return &exitError{code: rc}
			}
			return nil
		},
	}
	c.Flags().SetInterspersed(false)
	c.Flags().StringVar(&maxFlag, "max", "", "the command's memory cap, a systemd size (default $MEMCAP_MAX or memcap.max)")
	c.Flags().StringVar(&swapFlag, "swap", "", "the command's swap cap (default $MEMCAP_SWAP or 0)")
	c.Flags().StringVar(&waitFlag, "wait", "", "how long to wait for a slot and memory (default $MEMCAP_WAIT, else 8m, 60m in a background call)")
	return c
}

// parseableKnownHosts exports SSH_KNOWN_HOSTS for the run's command when
// the machine's known_hosts holds a line Go's knownhosts cannot parse
// (knownhosts.Parseable); a caller's own SSH_KNOWN_HOSTS wins. A failure
// costs one line and the run goes on with the machine's files.
func (a *app) parseableKnownHosts() {
	if os.Getenv(knownhosts.Env) != "" {
		return
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	dst := filepath.Join(a.cfg.StateDir, "ssh", "known_hosts")
	dropped, err := knownhosts.Parseable(dst, knownhosts.Defaults(home)...)
	switch {
	case err != nil:
		_, _ = fmt.Fprintf(os.Stderr, "%sno parseable known_hosts for SSH_KNOWN_HOSTS: %v\n", guard.LogPrefix, err)
	case dropped > 0:
		_ = os.Setenv(knownhosts.Env, dst)
	}
}

// noSession names a process no Claude Code session started.
const noSession = "no session"

// runRecorder appends a run's events to the event log as the calling
// session, or nil when the log cannot be opened: a build runs regardless.
func (a *app) runRecorder() func(verb, detail string) {
	st, err := state.Open(a.cfg.StateDir)
	if err != nil {
		return nil
	}
	by, err := a.caller()
	if err != nil {
		by = state.Party{Name: noSession}
	}
	return func(verb, detail string) {
		_ = st.Log(event(by, verb, "%s", detail))
	}
}

func (a *app) hookCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "hook",
		Short: "Claude Code hooks",
		Long: `The Claude Code hooks beekeeper install registers user-wide. They act
only in the desk's scope: a session beekeeper started or knows (an agents
start, the supervisor's or the guide's holder, an agent on the roster), or
one whose working directory or project ($CLAUDE_PROJECT_DIR) lies under one
of hooks.scope.dirs. Every other session, a person's own project, gets
nothing from them: no refusal, no rewrite, no redaction, no prelude, no
answer to a permission card; the hook exits 0 at once. With
hooks.scope.dirs unset every session is in scope; a configuration or state
that does not load keeps the guards on.`,
		Args: cobra.NoArgs,
		PersistentPreRunE: func(*cobra.Command, []string) error {
			// a hook runs on the host for a sandboxed session too, where the
			// policy's environment stays its session's
			a.hook = true
			return nil
		},
		RunE: func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	c.AddCommand(&cobra.Command{
		Use:   "pretooluse",
		Short: "The PreToolUse hook: GitHub writes outside the allowed organizations, builds into a slot, the kind lab limit, the merge gate, Secret reads, outbound secrets, questions via the guide, desktop sends by name, a repository's instructions on the first write",
		Long: `pretooluse reads a PreToolUse event on stdin. A build, test, lint or lab
command is rewritten to run through "beekeeper run -- <shell> -c '<command>'"
(the absolute path of this binary, the configured shell), the tool timeout
raised to 10 minutes; a background run gets a 60-minute wait instead. A
command that would start a kind cluster past maxKindClusters is refused with
the running labs and the held leases. With kube.production set, the kube
guard refuses writes to that installation and context switches of the
machine kubeconfig. A configuration that does not load refuses every Bash
call, naming the error.
Every devctl pr merge gets "<this binary> gate --" in front of it (a
background one "gate --wait 30m --limit 0 --"), the timeout raised the same way; see
beekeeper lanes. That is behind prefix commands (flock <lock>, nohup, setsid,
stdbuf, ionice, chrt, nice, timeout, env, VAR=value) and in any segment of a
pipeline or list, devctl by name or path, wrapping only the devctl
invocation; a here-document a shell reads is gated in place too. A merge the
rewrite cannot reach is refused, naming the place and the line and, where it
has a place, the command with the gate written in: one inside a sh, bash or
zsh -c string or an eval string, in a script file the call runs (by a shell,
by its path, sourced, or by an interpreter such as python3), in an
interpreter's code (python3 -c, node -e, perl -e, …), or in the program a
shell or an interpreter reads from its input (a pipe, a here-string, a
here-document, a redirected file). Other devctl commands pass untouched.
A SendMessage to a desktop session id (local_…) whose session has a
running CLI goes to that CLI by its name instead: the desktop would start a
second CLI of the session beside a headless turn (an agents start's first
turn, an agents wake), and every send by local_ id counts against the
desktop's cap on messages between sessions. A name two running CLIs carry is
refused, naming them. A send to a session with no running CLI passes: the
desktop starts it. A send by name to an agent on the roster whose CLI does
not run (its headless turn ended, its import into the desktop waits) is
refused, saying so and whether an import is pending.
A SendMessage to "the supervisor" or "the guide" (any case, "the" optional)
goes to the session holding that role now: its running CLI by name, else
its desktop session. A brief names the role, so a relay never makes it
stale; with nobody holding the role the send is refused.
A SendMessage from the session holding the supervisor role whose message
says "yours <resource>", "<resource> yours" or "<resource> is yours" for a
leasable resource records the grant of the resource to the message's
target, as beekeeper lease grant does, and the sender reads what was
recorded, or why nothing was with the command that records it, as
additional context. A worker's "yours" records nothing.
Anything else, malformed input included, passes unchanged.

What leaves the machine is scanned for secret values: the command line
(here-documents included) of gh, devctl, git commit, tag and remote, and of
curl or wget sending a body; the files these send (--body-file, -F, @file,
$(cat file)); for git push the messages and added lines of the commits no
remote has yet; the input of every connector (mcp__) tool; and a Write or
Edit of a file under outbound.paths. A token pattern (gitleaks' rules: a
GitHub, GitLab, Slack, AWS, GCP, npm, OpenAI, Anthropic or 1Password token,
a private key, a JWT, a password in a URL) or one of outbound.phrases
refuses the call, naming the rule or the phrase's number, never the match.
A line marked gitleaks:allow is skipped. An op item or document create or
edit, or a vault kv put or patch, that an outbound.storeDeny rule matches is
refused. The connector tools need "|mcp__.*" in the matcher.

A Claude in Chrome call (mcp__claude-in-chrome__*) in a desktop turn of a
session beekeeper agents start started in bypass (it runs in acceptEdits
now) is refused, naming beekeeper browse: Claude Desktop holds such a
session's navigate to a site it was not allowed on yet for a person's site
approval, which no hook answers. Its headless turns and every other
session's browser calls pass.

Every GitHub write aimed at a repository outside github.allowedOrgs
(default giantswarm) is refused before it runs: gh pr create, comment,
review, close, edit, ready, merge, reopen, lock, unlock, update-branch; gh
issue create, comment, close, edit, reopen, lock, unlock, delete, transfer,
pin, unpin, develop; gh release, label and repo writes; gh api with POST,
PATCH, PUT or DELETE (-X, --method; -f, -F, --field, --raw-field or
--input without one imply POST) on repos/<owner>/<repo> or orgs/<owner>,
and curl doing the same to api.github.com; a gh api graphql mutation that
posts unless every owner it names is allowed; git push to a GitHub remote
of another owner, its push URL read in the worktree the command runs in (a
branch of our fork, upstream/<slug> on origin, passes); a GitHub connector
post with such an owner. The repository is --repo or -R, a URL or
owner/repo#n argument, the API path, else the one gh acts on: GH_REPO, the
gh repo set-default remote, else upstream, github, origin. The refusal is
one line naming the rule, a human decides what goes upstream: patches land
on our fork and as a row on github.upstreamReview; it is logged as a
hook.refused event with the session, the repository and the command, which
the supervisor's watch says as a REFUSED line. Reads (gh pr view, gh issue
view, gh api GET, gh search, git fetch) pass.

A board-wide project read is refused: gh project item-list without
--query, and a gh api graphql query over a projectV2's items without a
narrowing query: argument. Such a read pages the whole board with its field
values and spends the GraphQL limit every session shares; the refusal names
the estimated cost, the GraphQL budget as beekeeper last read it, and
gh project item-add, gh project item-edit by item id, beekeeper board move
and a filtered read instead. An issue's projectItems passes.

An AskUserQuestion call is refused in every session but the guide's (the
one beekeeper guide names): the agent files beekeeper note add --for
<guide.person> and carries on.

The guide asks and relays, it never works itself: in the guide's session
an Edit, Write or NotebookEdit, a git commit or push, a devctl pr merge or
release promote, a gh pr merge or review, a GitHub connector tool that
merges, reviews or pushes, and a browser (claude-in-chrome) action other
than opening, reading or looking at a page are refused with the hint to
hand the work to the supervisor in one line. Its AskUserQuestion is checked
as note add checks a note for the person: every question asks, carries a
"Status quo: …" and a "Why: …" part, a "Checked: …" part for a claim that
something is merged, green, released, rolled or closed, the full URL of
every #N or owner/repo#N, and a consequence as every option's description.
Questions, notes, SendMessage, reads and beekeeper pass. Other sessions
are not affected.

A session's first Edit, Write or NotebookEdit, or first git commit, in a
git repository other than its own project ($CLAUDE_PROJECT_DIR, its
worktrees included) carries that repository's instructions as additional
context, once per session (and subagent) and repository: CLAUDE.md and
AGENTS.md with their @imports,
.claude/rules/*.md, the hooks and permissions of .claude/settings.json, and
the files these name as mandatory reading, at most 10,000 bytes, a cut file
and the ones left out named. Only regular text files inside the repository
are read, never a secret's name (settings.local.json, *.local.md, .env*,
keys, sops files). The call is never refused for it; the markers live
under <stateDir>/reads.

Register it in ~/.claude/settings.json:

  "PreToolUse": [{"matcher": "Bash|Read|Grep|Edit|Write|NotebookEdit|AskUserQuestion|SendMessage|TaskStop|mcp__.*", "hooks": [{"type": "command",
    "command": "~/.go/bin/beekeeper hook pretooluse"}]}]`,
		Args: cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			defer func() { _ = recover() }() // a broken hook must not block the tool call
			raw, err := io.ReadAll(os.Stdin)
			if err != nil {
				return nil
			}
			cfgErr := a.loadConfig()
			sandboxed := a.sandboxed(cfgErr)
			if !a.inScope(raw) {
				// the sandbox holds every session, the desk's or not
				if out := (guard.Hook{Sandbox: sandboxed}).DecideSandbox(raw); out != nil {
					_, _ = a.out.Write(out)
				}
				return nil
			}
			self, _ := os.Executable()
			h := guard.Hook{Self: self, Clusters: kindClusterNames, Leases: a.heldLeases, Guide: a.isGuide, CheckQuestion: checkQuestion, Role: a.roleTarget, Holds: a.heldRole, Peer: a.desktopPeer, Absent: a.absentPeer, Agent: a.agentPeer, Yours: a.yoursGrant,
				Project: os.Getenv("CLAUDE_PROJECT_DIR"), Reads: a.firstReads,
				Kubeconfig: kubeconfigList(), MachineKubeconfig: machineKubeconfig(),
				ModelServer: a.modelServer, ConfigErr: cfgErr, Sandbox: sandboxed,
				Started: a.startedWorker,
				Apps:    a.allowedApps, Shared: sharedUploads}
			if h.ConfigErr == nil {
				h.Shell, h.Production, h.ContextHint = a.cfg.Shell, a.cfg.Kube.Production, a.cfg.Kube.Context("<installation>")
				h.MaxLabs = func() int { return a.cfg.KindClusters(ramMiB()) }
				h.Outbound = outboundGuard(a.cfg.Outbound)
				h.Upstream = guard.Upstream{Orgs: a.cfg.GitHub.AllowedOrgs, Review: a.cfg.GitHub.UpstreamReview}
				h.Refused = a.recordRefusal
				h.Labs = a.heldLabs
				if !inSandbox() {
					h.FreshLabs = a.freshLabKubeconfigs
				}
				h.UnlockCommands = a.cfg.Secret.UnlockCommands
				h.SecretFiles = slices.DeleteFunc(append(slices.Clone(a.cfg.Secret.Files), a.cfg.Secret.TokenFile), func(f string) bool { return f == "" })
				h.GraphQL = a.graphqlLeft
				h.StopTask = a.stopTask
			}
			if out := h.Decide(raw); out != nil {
				_, _ = a.out.Write(out)
			}
			return nil
		},
	})
	c.AddCommand(&cobra.Command{
		Use:   "posttooluse",
		Short: "The PostToolUse hook: a tool result carrying a secret value is redacted before the model sees it",
		Long: `posttooluse reads a PostToolUse event on stdin and scans every string of
the tool's result against the fingerprint index (beekeeper scan) and the
outbound guard's token patterns (gitleaks' rules; a line marked
gitleaks:allow keeps its pattern matches). With a hit, its answer replaces
the result before the model sees it: the same result, each hit replaced by
"[redacted: <reference or rule>]". The session goes on. Each redaction is
a scan.redact event in beekeeper log, naming the tool, the references and
rules and their counts, never a value; an indexed reference gets a
rotation note for guide.person unless an open one names it. Claude Code
writes the redacted result to the session's transcript on disk too: the
value reaches neither the model nor the transcript.

Malformed input, an unreadable configuration or index, any error: no
answer, the result unchanged (an unreadable index still runs the
patterns). beekeeper install registers it in ~/.claude/settings.json:

  "PostToolUse": [{"matcher": "*", "hooks": [{"type": "command",
    "command": "~/.go/bin/beekeeper hook posttooluse", "timeout": 10}]}]`,
		Args: cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			defer func() { _ = recover() }() // a broken hook must not break the tool's result
			raw, err := io.ReadAll(os.Stdin)
			if err != nil || !a.inScope(raw) {
				return nil
			}
			if out := a.postToolUse(raw); out != nil {
				_, _ = a.out.Write(out)
			}
			return nil
		},
	})
	c.AddCommand(&cobra.Command{
		Use:   "sessionstart",
		Short: "The SessionStart hook: the agent shell's prelude",
		Long: `sessionstart writes the agent shell's prelude into the session's
environment file ($CLAUDE_ENV_FILE), which Claude Code sources before each
Bash command, before it parses the command: the aliases and shell functions
of agents.shell.unalias (default grep, find, ls, cp, mv, rm, among them the
harness's own grep and find shadows) are removed, so each name runs the tool
on PATH, and with agents.shell.globs literal (the default) an unmatched glob
stays as written instead of failing the command (zsh's "no matches found").
The person's interactive setup stays theirs; an agent's commands are written
for the plain tools. agents.shell.path goes first on PATH, except in the
agent sandbox, where gh reaches GitHub through the broker's egress proxy instead
of a gh link to devctl: there its directories leave PATH, an inherited one too.
Outside hooks.scope.dirs it writes nothing, except in the sandbox, where it
writes that PATH drop alone. It replaces only its own block, so other hooks' lines
stay, and prints nothing. beekeeper install registers it in
~/.claude/settings.json:

  "SessionStart": [{"matcher": "", "hooks": [{"type": "command",
    "command": "~/.go/bin/beekeeper hook sessionstart"}]}]`,
		Args: cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			raw, _ := io.ReadAll(os.Stdin)
			env := os.Getenv("CLAUDE_ENV_FILE")
			held := os.Getenv(sandbox.Env) != ""
			if env == "" || a.loadConfig() != nil {
				return nil // a broken configuration must not block a session's start
			}
			scoped := a.inScope(raw)
			if !scoped && !held {
				return nil
			}
			sh := a.cfg.Agents.Shell
			var drop []string
			unheld := ""
			if held {
				// held, gh goes through the egress proxy and a gh link to
				// devctl would read the keychain the sandbox closes, also from
				// an inherited PATH; the policy's environment without its
				// sandbox is the host's again
				drop, unheld = sh.Path, sandbox.UnsetShell(sandbox.EgressDir(os.Getenv("XDG_RUNTIME_DIR")))
			}
			unalias := append(slices.Clone(sh.Unalias), a.cfg.Secret.UnlockCommands...)
			globs, path := sh.Globs == config.GlobsLiteral, sh.Path
			if !scoped {
				// a held session outside the scope gets the sandbox's part
				// only: its gh would otherwise resolve to the link to devctl
				unalias, globs, path = nil, false, nil
			}
			if err := guard.WritePrelude(env, guard.Prelude(unalias, globs, path, drop, unheld)); err != nil {
				return err
			}
			if !held {
				a.recordSessionGH(raw, guard.Resolve("gh", guard.PathFirst(sh.Path, os.Getenv("PATH"))))
			}
			return nil
		},
	})
	c.AddCommand(&cobra.Command{
		Use:   "reportcheck",
		Short: "The scheduled reporter's PreToolUse hook: a post that fails the report check is refused",
		Long: `reportcheck reads a PreToolUse event on stdin and refuses a
slack_send_message whose message fails beekeeper reporter check, with what
to fix as the reason; every other call passes. beekeeper starts each
scheduled reporter session with it in its --settings; it is never
registered in the person's settings.`,
		Args: cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			raw, err := io.ReadAll(os.Stdin)
			if err != nil {
				return err
			}
			if out := a.reportCheck(raw); out != nil {
				_, err = a.out.Write(out)
			}
			return err
		},
	})
	c.AddCommand(&cobra.Command{
		Use:   "browsestop",
		Short: "A browse turn's Stop hook: the turn closes the tabs it opened before it ends",
		Long: `browsestop reads a Stop event on stdin. While tabs the turn opened (its
tab group's tabs, the tabs it created) are still open in its group, it
answers "block": the turn goes on once to close them, the group's first tab
last, then ends with its report. A turn that went on once already
(stop_hook_active) ends; browse names the tabs it left as a failure.
beekeeper browse passes it in each turn's --settings; it is never registered
in the person's settings.`,
		Args: cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			defer func() { _ = recover() }() // a broken hook must not hold the turn
			raw, err := io.ReadAll(os.Stdin)
			if err != nil {
				return nil
			}
			if out := guard.BrowseStop(raw); out != nil {
				_, _ = a.out.Write(out)
			}
			return nil
		},
	})
	c.AddCommand(a.capturePageHookCmd())
	c.AddCommand(&cobra.Command{
		Use:   "permissionrequest",
		Short: "The PermissionRequest hook: no card for beekeeper's own bypass starts after the desktop import",
		Long: `permissionrequest reads a PermissionRequest event on stdin: Claude Code is
about to show the person a permission card. It answers "allow" only when
the session is one beekeeper agents start started in bypassPermissions (its
session id is in beekeeper's record of starts) and the session now runs in
acceptEdits, the mode Claude Desktop's import gives it. Each allow is a
hook.allow event in beekeeper log.

A session the person took over on beekeeper ui (a flag in beekeeper's
take-over folder naming the running screen) has its request held for the
screen: the screen's allow or deny is the answer. No answer within 290 s,
the screen gone, or the take-over released: no answer, and the request
goes to the session's own window. Each is a takeover.answer or
takeover.back event. The take-over comes before the allow: a beekeeper
start the person took over asks the screen, and its unanswered request
gets the window's card, not the allow.

Every other request gets no answer at once and the person gets the normal
card: sessions beekeeper did not start, desktop sessions, and beekeeper's
starts the person set to default or plan. Telling a session not taken over
reads one file without a lock. Deny rules still win: Claude Code
refuses a denied call before it asks, so the hook never sees it. Malformed
input, an unreadable configuration or state, any error: no answer, never an
allow. A request in any mode but acceptEdits is decided without reading the
state.

Register it in ~/.claude/settings.json:

  "PermissionRequest": [{"matcher": "*", "hooks": [{"type": "command",
    "command": "~/.go/bin/beekeeper hook permissionrequest", "timeout": 300}]}]

The timeout is above the 290 s a held request waits, so the hook, not
Claude Code, ends the wait.`,
		Args: cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			defer func() { _ = recover() }() // a broken hook gives no answer: the person's card
			raw, err := io.ReadAll(os.Stdin)
			if err != nil || !a.inScope(raw) {
				return nil
			}
			_, _ = a.out.Write(a.permissionRequest(context.Background(), raw))
			return nil
		},
	})
	return c
}

// graphqlLeft is the GraphQL limit as beekeeper last read it, with the
// reading's age; "" when none was read. It reads the state without its
// lock, never GitHub: the hook opens no store of its own.
func (a *app) graphqlLeft() string {
	if a.loadConfig() != nil {
		return ""
	}
	store, err := state.Open(a.cfg.StateDir)
	if err != nil {
		return ""
	}
	st, err := store.Peek()
	if err != nil || st.Budget == nil || st.Budget.GraphQL == nil {
		return ""
	}
	if a.now.IsZero() {
		a.now = time.Now()
	}
	return graphqlText(a, graphqlOf(st.Budget.GraphQL)) + ", read " + dur(a.now.Sub(st.Budget.GraphQL.At)) + " ago"
}

// inScope reports whether the hook event in raw is in the hooks' scope:
// its working directory or the session's project ($CLAUDE_PROJECT_DIR)
// under one of hooks.scope.dirs, or a session beekeeper knows (state.Knows).
// No dirs configured, every session is; a configuration or state that does
// not load keeps the guards on.
func (a *app) inScope(raw []byte) bool {
	if a.loadConfig() != nil || len(a.cfg.Hooks.Scope.Dirs) == 0 {
		return true
	}
	var ev struct {
		CWD     string `json:"cwd"`
		Session string `json:"session_id"`
	}
	_ = json.Unmarshal(raw, &ev)
	if guard.Under(a.cfg.Hooks.Scope.Dirs, ev.CWD, os.Getenv("CLAUDE_PROJECT_DIR")) {
		return true
	}
	store, err := state.Open(a.cfg.StateDir)
	if err != nil {
		return true
	}
	st, err := store.Peek()
	if err != nil {
		return true
	}
	return st.Knows(state.Party{Session: ev.Session, HostSession: os.Getenv("CLAUDE_CODE_HOST_SESSION_ID")})
}

// verbHookRefused is the event of a command the upstream guard refused.
const verbHookRefused = "hook.refused"

// recordRefusal logs the upstream guard's refusal of a session's command as
// a hook.refused event, the session by its name where its CLI has one. It
// never changes the refusal: a state it cannot write records nothing.
func (a *app) recordRefusal(session, command, repo string) {
	store, err := state.Open(a.cfg.StateDir)
	if err != nil {
		return
	}
	who := state.Party{Session: session, HostSession: os.Getenv("CLAUDE_CODE_HOST_SESSION_ID"), Name: noSession}
	if t, err := plat.Machine.Processes(); err == nil {
		if n := claude.RecordName(a.cfg, t, session); n != "" {
			who.Name = n
		}
	}
	_ = store.Log(event(who, verbHookRefused, "upstream write to %s: %s", repo, command))
}

// takeoverGiveUp and takeoverPoll pace a held request; tests shorten them.
var (
	takeoverGiveUp = takeover.GiveUp
	takeoverPoll   = takeover.Poll
)

// permissionRequest decides one PermissionRequest event: a taken-over
// session's request is held for the screen, beekeeper's own bypass starts
// are allowed, every other one gets no answer (nil) at once. The take-over
// comes first, so a beekeeper start the person took over asks the screen,
// and its unanswered request goes to its window's card, never to the allow.
func (a *app) permissionRequest(ctx context.Context, raw []byte) []byte {
	if out, held := a.heldRequest(ctx, raw); held {
		return out
	}
	var start state.Start
	out, req := guard.Permission(raw, func(session string) bool {
		var ok bool
		start, ok = a.bypassStart(state.Party{Session: session})
		return ok
	})
	if out != nil && a.store != nil {
		_ = a.store.Log(event(start.Party, "hook.allow", "%s in %s", req.Tool, start.Dir))
	}
	return out
}

// heldRequest holds the request in raw for the screen when its session is
// taken over: held is true and out the screen's answer, nil when none came.
// A session not taken over is told by one file read without a lock.
func (a *app) heldRequest(ctx context.Context, raw []byte) (out []byte, held bool) {
	var req guard.Request
	if json.Unmarshal(raw, &req) != nil || req.Event != guard.PermissionEvent || req.Session == "" || a.loadConfig() != nil {
		return nil, false
	}
	dir := takeover.Dir(a.cfg.StateDir)
	f, ok := takeover.Taken(dir, req.Session, proc.Alive)
	if !ok {
		return nil, false
	}
	ctx, cancel := context.WithTimeout(ctx, takeoverGiveUp)
	defer cancel()
	r := takeover.Request{ID: uuid.NewString(), Session: req.Session, Tool: req.Tool, Input: req.Input, At: time.Now().UTC()}
	d, answered, err := takeover.Hold(ctx, dir, r, proc.Alive, takeoverPoll)
	answered = answered && err == nil
	if store, serr := state.Open(a.cfg.StateDir); serr == nil {
		who := state.Party{Session: req.Session, Name: f.By}
		if answered {
			_ = store.Log(event(who, "takeover.answer", "%s: %s %s on the screen", req.Session, req.Tool, d.Behavior))
		} else {
			_ = store.Log(event(who, "takeover.back", "%s: %s went to the session's window", req.Session, req.Tool))
		}
	}
	if !answered {
		return nil, true
	}
	return guard.PermissionDecision(d.Behavior, d.Message), true
}

// reportCheck decides a reporter's PreToolUse event: a post failing the
// report check in the report's zone is refused. The hook runs without a
// loaded configuration; one that loads sets reporter.tz, else the zone is
// the machine's.
func (a *app) reportCheck(raw []byte) []byte {
	tz := ""
	if a.loadConfig() == nil {
		tz = a.cfg.Reporter.TZ
	}
	return guard.ReportCheck(raw, func(msg string) []string {
		zone, err := reportZone(tz, a.zone)
		if err != nil {
			return []string{err.Error()}
		}
		return post.Report(msg, zone, time.Now())
	})
}

// bypassStart returns the record of p when beekeeper agents start started
// it in bypassPermissions. It reads the state without its lock, so that a
// permission request never waits on a slow update.
func (a *app) bypassStart(p state.Party) (state.Start, bool) {
	if a.loadConfig() != nil {
		return state.Start{}, false
	}
	store, err := state.Open(a.cfg.StateDir)
	if err != nil {
		return state.Start{}, false
	}
	st, err := store.Peek()
	if err != nil {
		return state.Start{}, false
	}
	a.store = store
	return st.BypassStartOf(p)
}

// startedWorker reports whether beekeeper agents start started session in
// bypassPermissions, the refusals' check: matched by its desktop host id
// too, so a worker whose CLI the desktop restarted under a new session id
// keeps them. The permission request's allow matches the session id only.
func (a *app) startedWorker(session string) bool {
	if session == "" {
		return false
	}
	_, ok := a.bypassStart(state.Party{Session: session, HostSession: os.Getenv("CLAUDE_CODE_HOST_SESSION_ID")})
	return ok
}

// firstReads reports whether this is the session's first write in repo. The
// configuration is read only for a write outside the session's project.
func (a *app) firstReads(session, repo string) bool {
	if a.loadConfig() != nil {
		return false
	}
	return guard.ReadsMarker{Dir: filepath.Join(a.cfg.StateDir, "reads")}.First(session, repo)
}

// loadConfig reads the configuration without opening the state.
func (a *app) loadConfig() error {
	path, err := config.Path(a.cfgPath)
	if err != nil {
		return err
	}
	if a.cfg, err = config.Load(path); err != nil {
		return err
	}
	if f := os.Getenv(stateFromEnv); f != "" {
		if err := a.stateFrom(f); err != nil {
			return err
		}
	}
	plat = platform.Current(platform.Options{DesktopApp: a.cfg.Claude.DesktopApp, QuietUnits: a.cfg.Units.Quiet})
	if !a.hook {
		unconfineUnheld(os.Stderr)
	}
	return nil
}

// ramMiB is the machine's RAM, 0 when it cannot be read.
func ramMiB() int {
	m, _ := plat.Machine.Mem()
	return m.TotalMiB
}

func kindClusterNames() []string {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cs, _ := machine.KindClusters(ctx)
	names := make([]string, 0, len(cs))
	for _, c := range cs {
		names = append(names, c.Name)
	}
	return names
}

// isGuide reports whether the session (its CLI id, or the desktop id in
// the environment) holds the guide role, and the person the guide asks. A
// broken configuration or state reads as not the guide.
// checkQuestion checks one of the guide's questions as note add checks a
// note for the person.
func checkQuestion(q guard.Question) []string {
	return noteDraft{Question: q.Text, StatusQuo: q.StatusQuo, Why: q.Why, Checked: q.Checked, Options: q.Options}.lacks()
}

func (a *app) isGuide(session string) (bool, string) {
	if a.loadConfig() != nil {
		return false, ""
	}
	person := a.cfg.Guide.Person
	store, err := state.Open(a.cfg.StateDir)
	if err != nil {
		return false, person
	}
	st, err := store.Read()
	if err != nil {
		return false, person
	}
	holder := st.GuideRole().Holder
	me := state.Party{Session: session, HostSession: os.Getenv("CLAUDE_CODE_HOST_SESSION_ID")}
	return holder != nil && session != "" && holder.Is(me), person
}

// heldLeases lists the held leases for the third-lab refusal, each holder
// named as `lease list` names it. The configuration is read only when a
// refusal needs it: a broken one must not block a tool call.
// modelServer is the host's model server as the hook guards it.
func (a *app) modelServer() guard.ModelServer {
	if a.loadConfig() != nil {
		return guard.ModelServer{}
	}
	return guard.ModelServer{URL: a.cfg.Ollama.URL, LemonadeURL: a.cfg.Lemonade.URL, LabTests: a.cfg.Ollama.LabTests}
}

// heldLabs lists the held leases of kind labs.
func (a *app) heldLabs() []lease.Holder {
	hs, _ := lease.Dir(a.cfg.LeaseDir).List()
	return slices.DeleteFunc(hs, func(h lease.Holder) bool { return a.cfg.LabCluster(h.Env) == "" })
}

func (a *app) heldLeases() []lease.Holder {
	if a.loadConfig() != nil {
		return nil
	}
	hs, _ := lease.Dir(a.cfg.LeaseDir).List()
	var sessions []*claude.Session
	if t, err := plat.Machine.Processes(); err == nil {
		sessions = claude.Discover(a.cfg, t, time.Now())
	}
	return a.namedHolders(sessions, hs)
}

// namedHolders sets each holder's name to the one `lease list` shows: the
// claim's, the live session's, or the user@host holder.
func (a *app) namedHolders(sessions []*claude.Session, hs []lease.Holder) []lease.Holder {
	out := make([]lease.Holder, len(hs))
	for i, h := range hs {
		h.Name = a.leaseView(sessions, h).Name
		out[i] = h
	}
	return out
}

func env(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

// machineKubeconfig is the machine kubeconfig, ~/.kube/config.
func machineKubeconfig() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".kube", "config")
}

// kubeconfigList is the kubeconfig list kubectl reads by default.
func kubeconfigList() string {
	return env("KUBECONFIG", machineKubeconfig())
}

// outboundGuard is the hook's outbound guard from its configuration.
func outboundGuard(o config.Outbound) guard.Outbound {
	g := guard.Outbound{Phrases: o.Phrases, Paths: o.Paths}
	for _, r := range o.StoreDeny {
		g.StoreDeny = append(g.StoreDeny, guard.StoreRule{Vault: r.Vault, Item: r.Item})
	}
	return g
}
