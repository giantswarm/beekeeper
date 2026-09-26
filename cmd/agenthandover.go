package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/peer"
	"github.com/giantswarm/beekeeper/internal/proc"
	"github.com/giantswarm/beekeeper/internal/state"
)

const (
	// verbNote is the event an agent's hand-over note is logged as.
	verbNote = "agents.note"
	// maxNote keeps a note one event line.
	maxNote = 16 << 10
	// handoverEvents is how many of the agent's last events its prompt
	// lists.
	handoverEvents = 15
	// endWait bounds the wait for the old session's processes to exit on
	// SIGTERM before they are killed.
	endWait = 10 * time.Second
	// briefOpen and briefClose enclose the brief in a hand-over prompt, so
	// the next hand-over passes on the brief, not the prompt around it.
	briefOpen, briefClose = "<brief>", "</brief>"
)

func (a *app) agentNoteCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "note <text>",
		Short: "Record the calling agent's hand-over note: what is in flight and what is next",
		Long: `note logs the calling session's hand-over note to the event log. beekeeper
agents handover asks the agent for it and puts the latest one into its
follow-up session's prompt.`,
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			me, err := a.caller()
			if err != nil {
				return err
			}
			text := strings.TrimSpace(args[0])
			switch {
			case text == "":
				return usageErr("the note is empty")
			case len(text) > maxNote:
				return usageErr("the note has %d bytes, more than %d", len(text), maxNote)
			}
			if err := a.store.Log(event(me, verbNote, "%s", text)); err != nil {
				return err
			}
			_, err = fmt.Fprintf(a.out, "noted for the hand-over of %s\n", me.Name)
			return err
		},
	}
}

func (a *app) agentHandoverCmd() *cobra.Command {
	var prompt bool
	var model, dir string
	c := &cobra.Command{
		Use:   "handover <agent>",
		Short: "Hand a registered agent over to a fresh session near its context limit",
		Long: `handover moves a registered agent to a fresh session, one line per step:
it asks the agent (a peer message) to record what is in flight with
beekeeper agents note, waiting agents.noteWait (3m) at most; builds the
follow-up's prompt; starts the follow-up as agents start does, under the
agent's name, taking over its roster entry, task and session record; stops
the old session's CLI and what it left running, so the desktop shows it
stopped (a claude --bg session through claude stop first, so its daemon
does not resume it); and logs agents.handover. watch says HANDOVER DUE once an agent's
context reached agents.relayAt, at its first quiet moment.

The follow-up runs in the old session's folder and model (--dir, --model
override them). A stopped agent is handed over without the note and the
stop.

--prompt prints the follow-up's prompt and changes nothing: the roster task,
the brief it was started with, its session record, its gated merges, its
last events and its note. It carries no standing rules and no live values.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			h, err := a.readHandover(args[0])
			if err != nil {
				return err
			}
			if prompt {
				_, err := fmt.Fprintln(a.out, h.prompt())
				return err
			}
			if dir != "" {
				h.dir = dir
			}
			if model != "" {
				h.model = model
			}
			return a.handOver(cmd.Context(), h)
		},
	}
	c.Flags().BoolVar(&prompt, "prompt", false, "print the follow-up's prompt and change nothing")
	c.Flags().StringVar(&model, "model", "", "the follow-up's model (default: the old session's)")
	c.Flags().StringVar(&dir, "dir", "", "the follow-up's working directory (default: the old session's)")
	return c
}

// handover is what a hand-over passes on from an agent's session.
type handover struct {
	agent state.Agent
	// session is the agent's running session, nil when its CLI stopped.
	session *claude.Session
	context int64
	record  *state.Record
	merges  []state.Merge
	events  []state.Event
	note    string
	brief   string
	// dir and model are the follow-up's.
	dir, model string
}

func (a *app) readHandover(q string) (handover, error) {
	st, err := a.store.Read()
	if err != nil {
		return handover{}, err
	}
	i, err := findAgent(st, q)
	if err != nil {
		return handover{}, err
	}
	h := handover{agent: st.Agents[i]}
	p := h.agent.Party
	for _, rl := range roles {
		if r := rl.get(st); r.Holder != nil && r.Holder.Is(p) {
			return handover{}, refused("%q holds the %s role: it moves by relay (%s)", p.Name, rl.name, rl.handover)
		}
	}
	sessions, _, err := a.sessions()
	if err != nil {
		return handover{}, err
	}
	transcript := ""
	if s, ok := claude.Live(sessions, p); ok {
		h.session, h.dir, h.model, transcript = s, s.Cwd, s.Model, s.Transcript
		h.context = transcriptContext(s, a.now)
	}
	if transcript == "" && p.Session != "" {
		if m, _ := filepath.Glob(filepath.Join(a.cfg.Claude.ProjectsDir, "*", p.Session+".jsonl")); len(m) > 0 {
			transcript = m[0]
		}
	}
	if s, ok := st.BypassStart(p.Session); ok {
		if h.dir == "" {
			h.dir = s.Dir
		}
		if transcript != "" {
			if t, found, err := claude.First(transcript); err == nil && found {
				h.brief = briefOf(t.Text)
			}
		}
	}
	for _, r := range st.Records {
		if r.Session.Is(p) {
			h.record = &r
		}
	}
	for _, m := range st.Merges {
		if m.By.Is(p) {
			h.merges = append(h.merges, m)
		}
	}
	h.events, err = a.store.Events(handoverEvents, func(e state.Event) bool {
		return e.By.Is(p) && e.Verb != verbNote && !isRun(e) && !strings.HasPrefix(e.Verb, "hook.")
	})
	if err != nil {
		return handover{}, err
	}
	h.note, _, err = a.lastNote(p, time.Time{})
	return h, err
}

// lastNote is p's latest hand-over note logged at or after since.
func (a *app) lastNote(p state.Party, since time.Time) (string, bool, error) {
	notes, err := a.store.Events(1, func(e state.Event) bool {
		return e.Verb == verbNote && e.By.Is(p) && !e.At.Before(since)
	})
	if err != nil || len(notes) == 0 {
		return "", false, err
	}
	return notes[0].Detail, true, nil
}

// briefOf is the brief of a session's first prompt: the brief a hand-over
// prompt encloses, or the whole prompt.
func briefOf(first string) string {
	if i := strings.Index(first, briefOpen); i >= 0 {
		if j := strings.LastIndex(first, briefClose); j > i {
			return strings.TrimSpace(first[i+len(briefOpen) : j])
		}
	}
	return strings.TrimSpace(first)
}

// prompt is the follow-up session's whole prompt.
func (h handover) prompt() string {
	var b strings.Builder
	ag := h.agent
	fmt.Fprintf(&b, "You are %q, taking over from its previous session %s", ag.Name, ag.Session)
	if h.context > 0 {
		fmt.Fprintf(&b, ", which beekeeper ended at %s tokens of context", tokensText(h.context))
	}
	b.WriteString(". Carry the task on to its finish from this prompt alone: nobody adds to it. " +
		"Continue where the note leaves off and do not redo finished steps; check the live state " +
		"(files, pull requests, CI) before acting on anything below, which was true when the previous session ended.\n\n")
	task := ag.Task
	if task == "" {
		task = "(none: the agent was idle; report back idle with beekeeper agents idle)"
	}
	fmt.Fprintf(&b, "Task: %s\n", task)
	if r := h.record; r != nil {
		fmt.Fprintf(&b, "Serves: %s", r.Issue)
		if r.Waits != "" {
			fmt.Fprintf(&b, ", waiting on %s", r.Waits)
		}
		b.WriteString("\n")
	}
	b.WriteString("\nHand-over note from the previous session:\n")
	if h.note != "" {
		b.WriteString(h.note + "\n")
	} else {
		b.WriteString("(none: the brief and the events below are what is known)\n")
	}
	if len(h.merges) > 0 {
		b.WriteString("\nIts gated merges:\n")
		for _, m := range h.merges {
			fmt.Fprintf(&b, "- %s in lane %s: %s\n", m.Key(), m.Lane, m.Phase)
		}
	}
	if len(h.events) > 0 {
		b.WriteString("\nIts last events:\n")
		for _, e := range h.events {
			fmt.Fprintf(&b, "- %s %s %s\n", e.At.Local().Format("Jan 2 15:04"), e.Verb, e.Detail)
		}
	}
	if h.brief != "" {
		brief := h.brief
		if room := max(maxBrief-b.Len()-(4<<10), 0); len(brief) > room {
			brief = brief[:room] + "\n(cut: the brief was longer than one command-line argument)"
		}
		fmt.Fprintf(&b, "\nThe brief it was started with:\n%s\n%s\n%s\n", briefOpen, brief, briefClose)
	}
	return strings.TrimSpace(b.String())
}

// summary names the parts a prompt carries, for the step's one line.
func (h handover) summary() string {
	parts := []string{"task"}
	add := func(ok bool, s string) {
		if ok {
			parts = append(parts, s)
		}
	}
	add(h.brief != "", "brief")
	add(h.record != nil, "record")
	add(len(h.merges) > 0, fmt.Sprintf("%d merges", len(h.merges)))
	add(len(h.events) > 0, fmt.Sprintf("%d events", len(h.events)))
	add(h.note != "", "note")
	return strings.Join(parts, ", ")
}

// handOver runs the hand-over's steps, one line each.
func (a *app) handOver(ctx context.Context, h handover) error {
	began := time.Now()
	me, err := a.caller()
	if err != nil {
		return err
	}
	ag := h.agent
	if me.Session != "" && me.Is(ag.Party) {
		return refused("%q cannot hand itself over: its session ends with the hand-over; another session (the supervisor) runs it", ag.Name)
	}
	if h.dir == "" {
		return usageErr("the folder of %q is unknown: pass --dir", ag.Name)
	}
	userSettings := filepath.Join(filepath.Dir(a.cfg.Claude.ProjectsDir), "settings.json")
	if files, ok := permissionHook(userSettings, h.dir); !ok {
		return refused("%q is not handed over: no PermissionRequest hook runs `beekeeper hook permissionrequest` in %s, so its follow-up would stop at its first card; "+
			"install the hook there (README: Agents started without a click) or pass --dir a folder whose settings have it", ag.Name, strings.Join(files, ", "))
	}
	noted := "no note"
	if h.session == nil {
		a.say("note: not asked, the CLI of %q does not run", ag.Name)
	} else {
		since := time.Now().UTC()
		// Its running CLI answers peer messages under its own name, which an
		// imported session's desktop derives: not always the roster's.
		res, err := peer.Sender{Dir: a.cfg.StateDir}.Send(ctx, h.session.Name, a.noteRequest(h))
		if err != nil {
			return fmt.Errorf("asking %q for its note: %w (nothing changed)", ag.Name, err)
		}
		note, ok, err := a.awaitNote(ctx, ag.Party, since, a.cfg.Agents.NoteWait.Duration)
		if err != nil {
			return err
		}
		if ok {
			h.note, noted = note, "noted"
			a.say("note: written after %s (the ask cost $%.2f)", dur(time.Since(since)), res.CostUSD)
		} else {
			a.say("note: none after %s, handing over without it (the ask cost $%.2f)", a.cfg.Agents.NoteWait.Duration, res.CostUSD)
		}
	}
	p := h.prompt()
	a.say("prompt: %d bytes: %s", len(p), h.summary())
	sa, err := a.startAgent(ctx, agentStart{name: ag.Name, brief: p, task: ag.Task, dir: h.dir, model: h.model, replaces: &ag.Party})
	if err != nil {
		return err
	}
	a.say("started %q: session %s, desktop local_%s, in %s, busy with %q", ag.Name, sa.id, sa.id, sa.dir, sa.task)
	a.say("%s", titleLine(ag.Name, sa.title))
	a.say("%s", modelLine(sa.model))
	a.say("%s", twinLine(sa.twin))
	if sa.kept != "" {
		a.say("the desktop still shows %s", sa.kept)
	}
	if h.session != nil {
		n, err := endSession(ctx, h.session)
		if err != nil {
			return fmt.Errorf("ending session %s: %w", ag.Session, err)
		}
		how := ""
		if h.session.Background {
			how = "claude stop, so its daemon does not resume it; "
		}
		a.say("ended session %s: %sits CLI %d and %d processes it left stopped", ag.Session, how, h.session.PID, n-1)
	}
	took := time.Since(began)
	if err := a.store.Log(event(me, "agents.handover", "%s: session %s at %s tokens to %s, %s, in %s",
		ag.Name, ag.Session, tokensText(h.context), sa.id, noted, dur(took))); err != nil {
		return err
	}
	a.say("handed over %q in %s: logged as agents.handover", ag.Name, dur(took))
	return nil
}

// hookSettings is the part of a Claude Code settings file permissionHook
// reads.
type hookSettings struct {
	Hooks struct {
		PermissionRequest []struct {
			Matcher string `json:"matcher"`
			Hooks   []struct {
				Type    string `json:"type"`
				Command string `json:"command"`
			} `json:"hooks"`
		} `json:"PermissionRequest"`
	} `json:"hooks"`
}

// permissionHook reports whether a session started in dir gets beekeeper's
// PermissionRequest hook for every tool: from the user settings, or from
// the project settings of dir or of the checkout dir is in. files are the
// settings files it read.
func permissionHook(userSettings, dir string) (files []string, ok bool) {
	files = []string{userSettings}
	for _, d := range projectDirs(dir) {
		files = append(files, filepath.Join(d, ".claude", "settings.json"), filepath.Join(d, ".claude", "settings.local.json"))
	}
	for _, f := range files {
		raw, err := os.ReadFile(f) //nolint:gosec // the Claude Code settings a started session reads
		if err != nil {
			continue
		}
		var hs hookSettings
		if json.Unmarshal(raw, &hs) != nil {
			continue
		}
		for _, m := range hs.Hooks.PermissionRequest {
			if m.Matcher != "" && m.Matcher != "*" {
				continue
			}
			for _, hk := range m.Hooks {
				if hk.Type == "command" && strings.Contains(hk.Command, "beekeeper") && strings.Contains(hk.Command, "hook permissionrequest") {
					return files, true
				}
			}
		}
	}
	return files, false
}

// projectDirs are dir and, when dir lies in a git checkout, its top.
func projectDirs(dir string) []string {
	out := []string{dir}
	for d := dir; ; d = filepath.Dir(d) {
		if _, err := os.Stat(filepath.Join(d, ".git")); err == nil {
			if d != dir {
				out = append(out, d)
			}
			return out
		}
		if filepath.Dir(d) == d {
			return out
		}
	}
}

// say prints one line.
func (a *app) say(format string, args ...any) { _, _ = fmt.Fprintf(a.out, format+"\n", args...) }

// noteRequest is the peer message that asks the agent for its note, naming
// the configuration file the hand-over runs with.
func (a *app) noteRequest(h handover) string {
	bin := "beekeeper"
	if c := a.explicitConfig(); c != "" {
		bin += " --config " + c
	}
	return fmt.Sprintf("beekeeper hands you over to a fresh session: your context is at %s tokens. "+
		"Record what is in flight and what is next in one command, then end your turn without doing anything else: "+
		"%s agents note \"<what is in flight, what is next>\"", tokensText(h.context), bin)
}

// awaitNote waits up to wait for p's note logged at or after since.
func (a *app) awaitNote(ctx context.Context, p state.Party, since time.Time, wait time.Duration) (string, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	for {
		note, ok, err := a.lastNote(p, since)
		if err != nil || ok {
			return note, ok, err
		}
		select {
		case <-ctx.Done():
			return "", false, nil
		case <-tick.C:
		}
	}
}

// bgJob is a session claude agents lists: its job id, session id and kind.
type bgJob struct {
	ID      string `json:"id"`
	Session string `json:"sessionId"`
	Kind    string `json:"kind"`
}

// claudeStop stops a background session through its daemon, which would
// otherwise resume it once its CLI dies. The daemon names its sessions by a
// job id of their own, which claude agents maps to the session id.
var claudeStop = func(ctx context.Context, id string) error {
	out, err := exec.CommandContext(ctx, "claude", "agents", "--json").Output()
	if err != nil {
		return fmt.Errorf("claude agents: %w", err)
	}
	var jobs []bgJob
	if err := json.Unmarshal(out, &jobs); err != nil {
		return fmt.Errorf("claude agents: %w", err)
	}
	i := slices.IndexFunc(jobs, func(j bgJob) bool { return j.Session == id && j.Kind == "background" && j.ID != "" })
	if i < 0 {
		return fmt.Errorf("claude agents lists no background job of session %s", id)
	}
	if out, err := exec.CommandContext(ctx, "claude", "stop", jobs[i].ID).CombinedOutput(); err != nil { //nolint:gosec // the job claude agents named
		return fmt.Errorf("claude stop %s: %w: %s", jobs[i].ID, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// endSession stops a session's CLI and every process under it (its unit's
// KillMode=process leaves them): a background session through `claude stop`
// first, so its daemon does not resume it, then SIGTERM, then SIGKILL for
// what still runs after endWait. It returns how many processes it stopped.
func endSession(ctx context.Context, s *claude.Session) (int, error) {
	pid := s.PID
	t, err := proc.Read()
	if err != nil {
		return 0, err
	}
	pids := []int{pid}
	for _, d := range t.Descendants(pid) {
		pids = append(pids, d.PID)
	}
	if s.Background {
		if err := claudeStop(ctx, s.ID); err != nil {
			return 0, err
		}
	}
	signal := func(sig syscall.Signal) {
		for _, p := range pids {
			if pr, err := os.FindProcess(p); err == nil {
				_ = pr.Signal(sig)
			}
		}
	}
	signal(syscall.SIGTERM)
	deadline := time.Now().Add(endWait)
	for time.Now().Before(deadline) && slices.ContainsFunc(pids, proc.Alive) {
		time.Sleep(200 * time.Millisecond)
	}
	signal(syscall.SIGKILL)
	time.Sleep(200 * time.Millisecond)
	if proc.Alive(pid) {
		return 0, errors.New("the CLI still runs after SIGKILL")
	}
	return len(pids), nil
}

// dueAgent is a registered agent whose hand-over is due.
type dueAgent struct {
	agent   state.Agent
	context int64
}

// handoversDue are the registered agents whose running session's context
// reached relayAt, at a quiet moment: no tool command of their own running
// and no gated merge of their own in flight. It skips the agents said
// already and those holding a relayed role, which relay instead.
func handoversDue(st *state.State, sessions []*claude.Session, relayAt config.Tokens, said func(state.Party) bool,
	contextOf func(*claude.Session) int64, alive func(int) bool,
) []dueAgent {
	var out []dueAgent
	for _, ag := range st.Agents {
		if said(ag.Party) || holdsRole(st, ag.Party) {
			continue
		}
		s, ok := claude.Live(sessions, ag.Party)
		if !ok || len(s.Commands) > 0 || mergeInFlight(st, ag.Party, alive) {
			continue
		}
		if c := contextOf(s); c >= int64(relayAt) {
			out = append(out, dueAgent{agent: ag, context: c})
		}
	}
	return out
}

func holdsRole(st *state.State, p state.Party) bool {
	for _, rl := range roles {
		if r := rl.get(st); r.Holder != nil && r.Holder.Is(p) {
			return true
		}
	}
	return false
}

func mergeInFlight(st *state.State, p state.Party, alive func(int) bool) bool {
	return slices.ContainsFunc(st.Merges, func(m state.Merge) bool {
		return m.By.Is(p) && m.Phase == state.Running && alive(m.PID)
	})
}

// handoversDue says HANDOVER DUE once for each agent handoversDue finds;
// the watch's mark keeps it said across the watch's restarts.
func (w *watcher) handoversDue(st *state.State, sessions []*claude.Session) {
	key := func(p state.Party) string { return "handover " + p.Session }
	said := func(p state.Party) bool { return w.reported[key(p)] }
	contextOf := func(s *claude.Session) int64 { return transcriptContext(s, w.now) }
	for _, d := range handoversDue(st, sessions, w.cfg.Agents.RelayAt, said, contextOf, proc.Alive) {
		w.reported[key(d.agent.Party)], w.dirty = true, true
		w.emitNow("handover", "HANDOVER DUE %q at %s: beekeeper agents handover %q", d.agent.Name, tokensText(d.context), d.agent.Name)
	}
}
