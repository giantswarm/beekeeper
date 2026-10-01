package cmd

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/guard"
	"github.com/giantswarm/beekeeper/internal/lease"
	"github.com/giantswarm/beekeeper/internal/machine"
	"github.com/giantswarm/beekeeper/internal/platform"
	"github.com/giantswarm/beekeeper/internal/post"
	"github.com/giantswarm/beekeeper/internal/proc"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/takeover"
)

func (a *app) runCmd() *cobra.Command {
	var maxFlag, swapFlag, waitFlag string
	c := &cobra.Command{
		Use:   "run [--max SIZE] [--wait DURATION] -- command [args...]",
		Short: "Run a build, test or lint command in a build slot under a memory cap",
		Long: `run takes one of the machine's build slots and runs the command in a
transient systemd user scope under memcap.slice with MemoryMax=SIZE,
MemorySwapMax=0 and OOMPolicy=continue. When the command's process tree
reaches the cap the kernel kills its biggest process and nothing outside the
scope notices; run then names the victim in one line starting with
"` + guard.LogPrefix + `" and exits 137 (or the tool's own failure code).
Bound the command's parallelism rather than raising --max.

When every slot is held, or MemAvailable is below the cap, run waits up to
--wait, one line when the wait starts and one when it ends. A wait that runs
out exits 75 and lists the holders: run the command in the background
instead (the PreToolUse hook then sets a 60m wait), never poll.

The slots are memcap's flock files, shared with the memcap wrapper. The
command's arguments reach it verbatim: systemd-run's own ${VAR} expansion is
off. Without a user systemd (containers, CI) the command runs uncapped.

Every capped run leaves a run.start and a run.end event in beekeeper log,
naming the scope, the session and the command, so that a cap kill found
later (snapshot, watch) names them after the run has ended. Logging never
fails or delays the run: an event the log cannot take within a second is
dropped.

Environment: MEMCAP_MAX (memcap.max, default 14% of RAM), MEMCAP_SWAP (0), MEMCAP_WAIT (8m),
MEMCAP_SLOTS and MEMCAP_STATE (the directory holding slots/) override the
configuration; the flags override the environment. MEMCAP_TEST=1 marks a
test's run: its scope is memcap-test-…, and snapshot and watch report a
kill in it as a test kill, not a build's.`,
		Args: cobra.MinimumNArgs(1),
		PersistentPreRunE: func(*cobra.Command, []string) error {
			return a.loadConfig()
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			o := guard.Options{Max: env("MEMCAP_MAX", a.cfg.MemcapMax(ramMiB())), Swap: env("MEMCAP_SWAP", "0"),
				SlotDir: a.cfg.Memcap.SlotDir, Slots: a.cfg.Memcap.Slots, Stderr: os.Stderr, Record: a.runRecorder(),
				Test: os.Getenv("MEMCAP_TEST") == "1"}
			if s := os.Getenv("MEMCAP_STATE"); s != "" {
				o.SlotDir = filepath.Join(s, "slots")
			}
			if s := os.Getenv("MEMCAP_SLOTS"); s != "" {
				n, err := strconv.Atoi(s)
				if err != nil || n < 1 {
					return usageErr("MEMCAP_SLOTS=%q: want a positive number", s)
				}
				o.Slots = n
			}
			f := cmd.Flags()
			if f.Changed("max") {
				o.Max = maxFlag
			}
			if f.Changed("swap") {
				o.Swap = swapFlag
			}
			wait := env("MEMCAP_WAIT", "8m")
			if f.Changed("wait") {
				wait = waitFlag
			}
			var err error
			if o.Wait, err = guard.ParseWait(wait); err != nil {
				return usageErr("%v", err)
			}
			for _, s := range []string{o.Max, o.Swap} {
				if _, err := guard.ParseSize(s); err != nil {
					return usageErr("%v", err)
				}
			}
			if rc := guard.Run(o, args); rc != 0 {
				return &exitError{code: rc}
			}
			return nil
		},
	}
	c.Flags().SetInterspersed(false)
	c.Flags().StringVar(&maxFlag, "max", "", "the command's memory cap, a systemd size (default $MEMCAP_MAX or memcap.max)")
	c.Flags().StringVar(&swapFlag, "swap", "", "the command's swap cap (default $MEMCAP_SWAP or 0)")
	c.Flags().StringVar(&waitFlag, "wait", "", "how long to wait for a slot and memory (default $MEMCAP_WAIT or 8m)")
	return c
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
		Args:  cobra.NoArgs,
		PersistentPreRunE: func(*cobra.Command, []string) error {
			return nil
		},
		RunE: func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	c.AddCommand(&cobra.Command{
		Use:   "pretooluse",
		Short: "The PreToolUse hook: builds into a slot, the kind lab limit, the merge gate, Secret reads, outbound secrets, questions via the guide, desktop sends by name, a repository's instructions on the first write",
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
background one "gate --wait 30m --"), the timeout raised the same way; see
beekeeper lanes. That is behind prefix commands (flock <lock>, nohup, setsid,
stdbuf, ionice, chrt, nice, timeout, env, VAR=value) and in any segment of a
pipeline or list, devctl by name or path, wrapping only the devctl
invocation. A merge inside a sh, bash or zsh -c string the rewrite cannot
reach is refused, naming the command with the gate written in. Other devctl
commands pass untouched.
A SendMessage to a desktop session id (local_…) whose session has a
running CLI goes to that CLI by its name instead: the desktop would start a
second CLI of the session beside a headless turn (an agents start's first
turn, an agents wake), and every send by local_ id counts against the
desktop's cap on messages between sessions. A name two running CLIs carry is
refused, naming them. A send to a session with no running CLI passes: the
desktop starts it.
A SendMessage to "the supervisor" or "the guide" (any case, "the" optional)
goes to the session holding that role now: its running CLI by name, else
its desktop session. A brief names the role, so a relay never makes it
stale; with nobody holding the role the send is refused.
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
git repository other than its own project ($CLAUDE_PROJECT_DIR) carries
that repository's instructions as additional context, once per session
(and subagent) and repository: CLAUDE.md and AGENTS.md with their @imports,
.claude/rules/*.md, the hooks and permissions of .claude/settings.json, and
the files these name as mandatory reading, at most 10,000 bytes, a cut file
and the ones left out named. Only regular text files inside the repository
are read, never a secret's name (settings.local.json, *.local.md, .env*,
keys, sops files). The call is never refused for it; the markers live
under <stateDir>/reads.

Register it in ~/.claude/settings.json:

  "PreToolUse": [{"matcher": "Bash|Edit|Write|NotebookEdit|AskUserQuestion|SendMessage|mcp__.*", "hooks": [{"type": "command",
    "command": "~/.go/bin/beekeeper hook pretooluse"}]}]`,
		Args: cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			defer func() { _ = recover() }() // a broken hook must not block the tool call
			raw, err := io.ReadAll(os.Stdin)
			if err != nil {
				return nil
			}
			self, _ := os.Executable()
			h := guard.Hook{Self: self, Clusters: kindClusterNames, Leases: a.heldLeases, Guide: a.isGuide, CheckQuestion: checkQuestion, Role: a.roleTarget, Peer: a.desktopPeer,
				Project: os.Getenv("CLAUDE_PROJECT_DIR"), Reads: a.firstReads,
				Kubeconfig: kubeconfigList(), MachineKubeconfig: machineKubeconfig(),
				ModelServer: a.modelServer, ConfigErr: a.loadConfig()}
			if h.ConfigErr == nil {
				h.Shell, h.Production, h.ContextHint = a.cfg.Shell, a.cfg.Kube.Production, a.cfg.Kube.Context("<installation>")
				h.MaxLabs = func() int { return a.cfg.KindClusters(ramMiB()) }
				h.Outbound = outboundGuard(a.cfg.Outbound)
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
			if err != nil {
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
for the plain tools. It replaces only its own block, so other hooks' lines
stay, and prints nothing. beekeeper install registers it in
~/.claude/settings.json:

  "SessionStart": [{"matcher": "", "hooks": [{"type": "command",
    "command": "~/.go/bin/beekeeper hook sessionstart"}]}]`,
		Args: cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			_, _ = io.Copy(io.Discard, os.Stdin)
			env := os.Getenv("CLAUDE_ENV_FILE")
			if env == "" || a.loadConfig() != nil {
				return nil // a broken configuration must not block a session's start
			}
			sh := a.cfg.Agents.Shell
			return guard.WritePrelude(env, guard.Prelude(sh.Unalias, sh.Globs == config.GlobsLiteral))
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
takeover.back event.

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
			if err != nil {
				return nil
			}
			_, _ = a.out.Write(a.permissionRequest(context.Background(), raw))
			return nil
		},
	})
	return c
}

// takeoverGiveUp and takeoverPoll pace a held request; tests shorten them.
var (
	takeoverGiveUp = takeover.GiveUp
	takeoverPoll   = takeover.Poll
)

// permissionRequest decides one PermissionRequest event: beekeeper's own
// bypass starts are allowed, a taken-over session's request is held for the
// screen, every other one gets no answer (nil) at once.
func (a *app) permissionRequest(ctx context.Context, raw []byte) []byte {
	var start state.Start
	out, req := guard.Permission(raw, func(session string) bool {
		var ok bool
		start, ok = a.bypassStart(session)
		return ok
	})
	if out != nil {
		if a.store != nil {
			_ = a.store.Log(event(start.Party, "hook.allow", "%s in %s", req.Tool, start.Dir))
		}
		return out
	}
	if req.Event != guard.PermissionEvent || req.Session == "" || a.loadConfig() != nil {
		return nil
	}
	dir := takeover.Dir(a.cfg.StateDir)
	f, ok := takeover.Taken(dir, req.Session, proc.Alive)
	if !ok {
		return nil
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
		return nil
	}
	return guard.PermissionDecision(d.Behavior, d.Message)
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

// bypassStart returns the record of session when beekeeper agents start
// started it in bypassPermissions. It reads the state without its lock, so
// that a permission request never waits on a slow update.
func (a *app) bypassStart(session string) (state.Start, bool) {
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
	return st.BypassStart(session)
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
	plat = platform.Current(platform.Options{DesktopApp: a.cfg.Claude.DesktopApp})
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
