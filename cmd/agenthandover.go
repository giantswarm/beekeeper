package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/omp"
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
the old session's CLI and what it left running (a claude --bg session
through claude stop first, so its daemon does not resume it); once that CLI
has exited, archives the old session's desktop row through a steward, as
the doctor does, so only the follow-up's row carries the name (an archive
not done now the doctor owes and asks for again); and logs agents.handover. watch says HANDOVER DUE once an agent's
context reached agents.relayAt, at its first quiet moment, and HANDOVER HELD
instead while the agent's task is in its last step (agents.lastStepGrace).

The follow-up runs in the old session's folder and model (--dir, --model
override them). An agent whose CLI does not run (its headless turn ended)
or does not take the message by name is asked in one headless turn resumed
from its transcript, as agents wake does, without the desktop's reopen. With
no note, the follow-up's prompt rests on the roster: the task, the sessions
serve record (the issue and what it waited on) and the last events, and the
command says so. It exits non-zero when no follow-up was started.

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
			} else if home := a.cfg.Agents.Dir; home != "" {
				// The follow-up keeps the old session's folder when agents
				// may run there, else it runs in agents.dir.
				if _, err := a.agentDir(h.dir); h.dir == "" || err != nil {
					h.dir = homePath(home)
				}
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
	if strings.HasPrefix(p.HostSession, omp.HostPrefix) {
		return handover{}, refused("%q is an omp agent: a hand-over starts a Claude Code successor from a Claude transcript", p.Name)
	}
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
		h.context = transcriptContext(s)
	}
	if transcript == "" {
		// A session whose CLI does not run: its context is its transcript's.
		if transcript = transcriptOf(a.cfg, p.Session); transcript != "" {
			_, act := claude.ReadTranscript(transcript, a.now)
			h.context = act.Context
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
	switch task := h.task(); {
	case task == "":
		b.WriteString("Task: (none: the agent was idle; report back idle with beekeeper agents idle)\n")
	case ag.Task == "":
		fmt.Fprintf(&b, "Task: %s\n(The previous session had reported it idle at %s, waiting: finish what is left of it, then report idle.)\n",
			task, ag.IdleSince.Local().Format("Jan 2 15:04"))
	default:
		fmt.Fprintf(&b, "Task: %s\n", task)
	}
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

// task is the agent's task: the open one, or the last one it reported idle
// on, which a hand-over at its first quiet moment carries on.
func (h handover) task() string {
	if h.agent.Task != "" {
		return h.agent.Task
	}
	return h.agent.LastTask
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
	note, ok, err := a.askNote(ctx, h)
	if err != nil {
		return err
	}
	if ok {
		h.note, noted = note, "noted"
	} else {
		a.say("note: none, so the follow-up's prompt rests on the roster: %s", h.roster())
	}
	p := h.prompt()
	a.say("prompt: %d bytes: %s", len(p), h.summary())
	sa, err := a.startAgent(ctx, agentStart{name: ag.Name, brief: workerPrompt(p), task: h.task(), dir: h.dir, model: h.model, replaces: &ag.Party})
	if err != nil {
		return err
	}
	a.say("started %q: session %s, desktop local_%s, in %s, busy with %q", ag.Name, sa.id, sa.id, sa.dir, sa.task)
	a.say("%s", sa.turn)
	if sa.restored != "" {
		a.say("%s", sa.restored)
	}
	a.say("%s", titleLine(ag.Name, sa.title))
	a.say("%s", modelLine(sa.model))
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
	// A start or wake unit of the old session still in its turn, or a
	// reopen unit of it, would show it in the desktop and warm a CLI of it
	// again. The turns' units go first: their stop-posts start reopen units.
	stop := func(u string) {
		if err := plat.Launcher.Stop(ctx, u); err != nil {
			a.say("stopping %s of session %s: %v", u, ag.Session, err)
			return
		}
		a.say("stopped %s of session %s, so the desktop does not reopen it", u, ag.Session)
	}
	for _, u := range turningUnits(ctx, ag.Session) {
		stop(u)
	}
	for _, u := range reopenUnits(ctx, ag.Session) {
		stop(u)
	}
	a.say("%s", a.archiveHandedOver(ctx, me, ag.Party, sa.id))
	took := time.Since(began)
	if err := a.store.Log(event(me, "agents.handover", "%s: session %s at %s tokens to %s, %s, in %s",
		ag.Name, ag.Session, tokensText(h.context), sa.id, noted, dur(took))); err != nil {
		return err
	}
	a.say("handed over %q in %s: logged as agents.handover", ag.Name, dur(took))
	return nil
}

// archiveHandedOver archives the desktop session of the handed-over p once
// its CLI has exited, so only the follow-up's row carries its name; the
// follow-up's session is to. What it cannot archive now the doctor owes,
// and asks a steward for while no CLI of p runs a turn. It returns the
// step's line.
func (a *app) archiveHandedOver(ctx context.Context, by, p state.Party, to string) string {
	var host string
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		host = oweArchive(st, p, "handed over to session "+to, a.now)
		return nil, nil
	}); err != nil {
		return fmt.Sprintf("its desktop session stays: %v", err)
	}
	if host == "" {
		return "its desktop session stays: beekeeper started none"
	}
	sessions, _, err := a.sessions()
	if err != nil {
		return fmt.Sprintf("its desktop session %s stays: %v; the doctor owes it the archive", host, err)
	}
	if s, live := claude.Live(sessions, p); live {
		return fmt.Sprintf("its desktop session %s stays while its CLI %d runs; the doctor owes it the archive", host, s.PID)
	}
	st, err := a.store.Read()
	if err != nil {
		return fmt.Sprintf("its desktop session %s stays: %v; the doctor owes it the archive", host, err)
	}
	outcomes := a.archiveDesktops(ctx, st, []state.Party{p}, "beekeeper agents handover")
	line := outcomes[0].line
	_ = a.store.Update(func(st *state.State) ([]state.Event, error) {
		line = owe(st, outcomes, a.now)[0]
		return []state.Event{event(by, "agents.archive", "%s: %s", p.Name, line)}, nil
	})
	return line
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
				if hk.Type == hookTypeCommand && strings.Contains(hk.Command, "beekeeper") && strings.Contains(hk.Command, "hook permissionrequest") {
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

// roster says what the roster tells the follow-up without a note.
func (h handover) roster() string {
	r := h.record
	switch {
	case r == nil:
		return "its task and last events (no sessions serve record)"
	case r.Waits == "":
		return "serves " + r.Issue
	}
	return "serves " + r.Issue + ", waiting on " + r.Waits
}

// sendNote sends the note request to the running CLI named to.
var sendNote = func(ctx context.Context, dir, to, msg string) (peer.Result, error) {
	return peer.Sender{Dir: dir}.Send(ctx, to, msg)
}

// askNote asks the agent for its hand-over note and waits for it: by name
// when its CLI runs and takes the message, else in one headless turn resumed
// from its transcript, as agents wake does, so a worker whose turn has ended
// writes its note too. ok is false when no note came: the session could not
// be resumed, or the turn wrote none (its context exhausted).
func (a *app) askNote(ctx context.Context, h handover) (string, bool, error) {
	since := time.Now().UTC()
	wait := a.cfg.Agents.NoteWait.Duration
	if s := h.session; s != nil {
		// Its running CLI answers peer messages under its own name, which an
		// imported session's desktop derives: not always the roster's.
		res, err := sendNote(ctx, a.cfg.StateDir, s.Name, a.noteRequest(h))
		if err == nil {
			note, ok, err := a.awaitNote(ctx, h.agent.Party, since, wait)
			if err == nil {
				a.say("note: %s (the ask cost $%.2f)", noteOutcome(ok, since, wait), res.CostUSD)
			}
			return note, ok, err
		}
		a.say("note: its CLI %d did not take the message by name (%v): resuming the session headless for it", s.PID, err)
	} else {
		a.say("note: the CLI of %q does not run: resuming the session headless for it", h.agent.Name)
	}
	unit, err := a.resumeForNote(ctx, h)
	if err != nil {
		a.say("note: not asked, the session cannot be resumed: %v", err)
		return "", false, nil
	}
	note, ok, err := a.awaitNote(ctx, h.agent.Party, since, wait)
	where := "its desktop CLI, which started meanwhile"
	if unit != "" {
		a.endNoteTurn(ctx, unit)
		where = "headless turn " + unit
	}
	if err == nil {
		a.say("note: %s (%s)", noteOutcome(ok, since, wait), where)
	}
	return note, ok, err
}

// noteOutcome is the note step's result for its line.
func noteOutcome(ok bool, since time.Time, wait time.Duration) string {
	if ok {
		return "written after " + dur(time.Since(since))
	}
	return "none after " + dur(wait)
}

// resumeForNote starts one headless turn of the agent's session with the
// note request, in a wake unit without the desktop's reopen: the session
// ends with the hand-over. It returns the unit; "" when the session's desktop
// CLI started meanwhile and took the request instead.
func (a *app) resumeForNote(ctx context.Context, h handover) (string, error) {
	st, err := a.store.Read()
	if err != nil {
		return "", err
	}
	w, err := resolveWake(a.cfg, st, h.agent)
	if err != nil {
		return "", err
	}
	if u := wakeRunning(ctx, w.id); u != "" {
		return "", refused("its wake turn %s runs", u)
	}
	if pid, err := a.toDesktopCLI(ctx, w.id, a.noteRequest(h)); pid != 0 || err != nil {
		return "", err
	}
	bin, err := exec.LookPath("claude")
	if err != nil {
		return "", err
	}
	unit := wakeUnit(w.id)
	if err := launch(unit, w.dir, a.explicitConfig(), nil, wakeArgv(bin, w, a.noteRequest(h))); err != nil {
		return "", err
	}
	return unit, nil
}

// endNoteTurn gives the note turn endWait to end by itself, then stops its
// unit: the turn has nothing left to do once the note is written, or the
// wait for it is over.
func (a *app) endNoteTurn(ctx context.Context, unit string) {
	deadline := time.Now().Add(endWait)
	for time.Now().Before(deadline) && len(plat.Launcher.Running(ctx, false, unit+"*")) > 0 {
		time.Sleep(200 * time.Millisecond)
	}
	if len(plat.Launcher.Running(ctx, false, unit+"*")) > 0 {
		if err := plat.Launcher.Stop(ctx, unit); err != nil {
			a.say("note: stopping the headless turn %s: %v", unit, err)
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
	t, err := plat.Machine.Processes()
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

// dueAgent is a registered agent whose hand-over is due; parked when its
// CLI does not run. held is why it is held instead of due (its task's last
// step, "" when due), until when; was says how long a hand-over due after
// a hold was held.
type dueAgent struct {
	agent   state.Agent
	context int64
	parked  bool
	held    string
	until   time.Time
	was     string
}

// handoversDue are the registered agents whose context reached relayAt, at a
// quiet moment: no tool command of their own running and no gated merge of
// their own in flight. An agent with a task whose CLI no longer runs (its
// headless turn ended, waiting on a grant or a person) is due too, with its
// transcript's context: nothing else relieves it. It skips the agents said
// already, those that reported their work done (the doctor archives them)
// and those holding or relieved of a relayed role, which relay instead.
// An agent in its task's last step (lastStepOf) is held rather than due
// for cfg.LastStepGrace from the step's evidence, unless its context reached
// cfg.LastStepCeiling: its report is expected before a hand-over would
// pay. contextOf gets a nil session for an agent whose CLI does not run.
func handoversDue(st *state.State, sessions []*claude.Session, cfg config.Agents, now time.Time, said func(state.Party) bool,
	contextOf func(state.Agent, *claude.Session) int64, alive func(int) bool,
) []dueAgent {
	var out []dueAgent
	for _, ag := range st.Agents {
		if said(ag.Party) || ag.Done || keepsRole(st, ag.Party) || mergeInFlight(st, ag.Party, alive) {
			continue
		}
		s, live := claude.Live(sessions, ag.Party)
		switch {
		case live && len(s.Commands) > 0, !live && ag.Task == "":
			continue
		case !live:
			s = nil
		}
		c := contextOf(ag, s)
		if c < int64(cfg.RelayAt) {
			continue
		}
		d := dueAgent{agent: ag, context: c, parked: !live}
		if why, since, ok := lastStepOf(st, ag.Party); ok {
			until := since.Add(cfg.LastStepGrace.Duration)
			switch {
			case now.Before(until) && c < int64(cfg.LastStepCeiling):
				d.held, d.until = why, until
			case now.Before(until):
				d.was = fmt.Sprintf("its last step (%s) holds no hand-over past %s", why, tokensText(int64(cfg.LastStepCeiling)))
			default:
				d.was = fmt.Sprintf("held %s for its last step (%s)", dur(cfg.LastStepGrace.Duration), why)
			}
		}
		out = append(out, d)
	}
	return out
}

// lastStepWords are the words of a sessions serve --waits that say the
// task is in its last step: the report is being written, or the merge
// landed and the proof, the release or the rollout is what remains.
var lastStepWords = regexp.MustCompile(`(?i)\b(report\w*|merged|releas\w*|roll(ed|ing|s)?[ -]?out|proof|proven)\b`)

// lastStepOf finds the evidence that p's task is in its last step and when
// it was given: its sessions serve record waits on the report or says the
// merge landed, or the gate saw a merge of its own land (settling, its
// release and rollout pending). The latest evidence counts; false when
// there is none.
func lastStepOf(st *state.State, p state.Party) (why string, since time.Time, ok bool) {
	for _, r := range st.Records {
		if r.Session.Is(p) && r.Ended.IsZero() && lastStepWords.MatchString(r.Waits) && !r.At.Before(since) {
			why, since, ok = fmt.Sprintf("it waits on %q", r.Waits), r.At, true
		}
	}
	for _, m := range st.Merges {
		if m.By.Is(p) && m.Phase == state.Settling && m.Exit == 0 && !m.Finished.Before(since) {
			why, since, ok = m.Key()+" merged", m.Finished, true
		}
	}
	return why, since, ok
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

// handoversDue says HANDOVER DUE once for each agent handoversDue finds,
// and HANDOVER HELD once for an agent it holds in its last step; the
// watch's mark keeps both said across the watch's restarts.
func (w *watcher) handoversDue(st *state.State, sessions []*claude.Session) {
	key := func(p state.Party) string { return "handover " + p.Session }
	said := func(p state.Party) bool { return w.reported[key(p)] }
	contextOf := func(ag state.Agent, s *claude.Session) int64 {
		if s == nil {
			s = &claude.Session{Transcript: transcriptOf(w.cfg, ag.Session)}
		}
		return transcriptContext(s)
	}
	for _, d := range handoversDue(st, sessions, w.cfg.Agents, w.now, said, contextOf, proc.Alive) {
		if d.held != "" {
			if k := "handover-held " + d.agent.Session; !w.reported[k] {
				w.reported[k], w.dirty = true, true
				w.emitNow("handover", "HANDOVER HELD %q at %s: last step, report expected (%s); due at %s or at %s",
					d.agent.Name, tokensText(d.context), d.held, d.until.Local().Format("15:04"), tokensText(int64(w.cfg.Agents.LastStepCeiling)))
			}
			continue
		}
		w.reported[key(d.agent.Party)], w.dirty = true, true
		parked, was := "", ""
		if d.parked {
			parked = " (its CLI does not run: the hand-over resumes it headless for its note)"
		}
		if d.was != "" {
			was = ", " + d.was
		}
		w.emitNow("handover", "HANDOVER DUE %q at %s%s%s: beekeeper agents handover %q", d.agent.Name, tokensText(d.context), parked, was, d.agent.Name)
	}
}
