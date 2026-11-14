package cmd

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/peer"
	"github.com/giantswarm/beekeeper/internal/state"
)

func continueURL(host string) string { return "claude://code/continue?session=" + host }

// startGrace is how long a relay may name a session no start of
// beekeeper's recorded: the start records it before it launches the session.
const startGrace = 2 * time.Minute

// relayPending reports whether r's open relay still stands for a
// successor: one whose start beekeeper recorded, or a moment ago. A relay a
// start never followed (the watch that opened it stopped) is left to the
// next successor, which replaces it.
func relayPending(st *state.State, r state.Role, now time.Time) bool {
	rel := r.Relay
	if !rel.Open(now) {
		return false
	}
	return !rel.Taken.IsZero() || now.Sub(rel.At) < startGrace || rel.To.Session == "" ||
		slices.ContainsFunc(st.Starts, func(s state.Start) bool { return s.Session == rel.To.Session })
}

// succeedFromWatch starts rl's next run for the standby watch, in the gone
// holder's folder.
func (a *app) succeedFromWatch(ctx context.Context, rl role, from state.Party) (state.Party, error) {
	to, _, err := a.startSuccessor(ctx, rl, from, WatchParty, "")
	return to, err
}

// unitsReopening reports whether a unit of session id runs its headless
// turn or its reopen: a start's or wake's unit active, starting or stopping
// (its stop-post starts the reopen's unit), or a reopen unit.
func unitsReopening(ctx context.Context, id string) bool {
	return len(turningUnits(ctx, id)) > 0 || len(reopenUnits(ctx, id)) > 0
}

// unitsTurning reports whether a start or wake unit of session id is
// active or starting: its headless turn runs.
func unitsTurning(ctx context.Context, id string) bool { return len(sessionUnits(ctx, id, false)) > 0 }

// turningUnits are the start and wake units of session id that are active,
// starting or stopping (deactivating: the turn's end on the stop signal,
// then the stop-post that starts the reopen's unit).
func turningUnits(ctx context.Context, id string) []string { return sessionUnits(ctx, id, true) }

// sessionUnits are the start and wake units of session id that are active
// or starting, and with stopping those in their stop too.
func sessionUnits(ctx context.Context, id string, stopping bool) []string {
	if len(id) < 8 {
		return nil
	}
	return plat.Launcher.Running(ctx, stopping, "beekeeper-agent-"+id[:8]+".service", wakePrefix(id)+"*")
}

// Successors that do not come up are retried a bounded number of times:
// the next one starts successorBackoff after the first failed, twice that
// after the second, none after maxFailedSuccessors.
const (
	successorBackoff    = 5 * time.Minute
	maxFailedSuccessors = 3
)

// started reports whether p's session is one of beekeeper's starts.
func started(st *state.State, p state.Party) bool {
	return p.Session != "" && slices.ContainsFunc(st.Starts, func(s state.Start) bool { return s.Execution == nil && s.Session == p.Session })
}

// reviveFromWatch resumes rl's holder headless with msg for the standby
// watch.
//
// The desktop runs the turn: a steward sends msg through the desktop's own
// session messaging, which starts the holder's desktop CLI at once, whatever
// the window's focus, the person's typing or the desktop's cap of CLIs. A
// holder runs no headless turn: its row in the desktop is where the person
// sees the role, and a headless turn that keeps a watch never hands it to a
// desktop CLI. A holder with no row yet (its import waits on the person's
// typing or missed at the cap) is imported first, as importRow does: the
// role does not wait for a reopen.
func (a *app) reviveFromWatch(ctx context.Context, _ role, holder state.Party, msg string) error {
	if holder.Session == "" {
		return fmt.Errorf("%s has no session id to reach", holder.Name)
	}
	st, err := a.store.Read()
	if err != nil {
		return err
	}
	if err := a.refuseStopped(st, "reviving "+holder.Name); err != nil {
		return err
	}
	if a.backgroundAgent(ctx, st, holder) {
		return a.reviveBackground(ctx, holder, msg)
	}
	host := desktopID(holder)
	id := strings.TrimPrefix(host, "local_")
	if !a.hasRow(id) {
		if _, err := a.importRowFromWatch(ctx, id, holder.Name); err != nil {
			return fmt.Errorf("%s has no row in the desktop and its import missed: %w", holder.Name, err)
		}
	}
	running := func() bool {
		sessions, _, err := a.sessions()
		if err != nil {
			return false
		}
		_, ok := claude.Live(sessions, holder)
		return ok
	}
	_, err = a.sendThroughDesktop(ctx, host, msg, running)
	return err
}

// sendThroughDesktop has a steward send msg to the desktop session host,
// which runs no CLI, through the desktop's session messaging: the desktop
// starts host's CLI at once with msg as its turn, whatever the window's
// focus, the person's typing or its cap. running reports host's CLI up. The
// send starts a CLI, so it waits for a headless turn of the session to end
// first, the desktop stays under its cap, and the steward asked keeps its
// own CLI.
func (a *app) sendThroughDesktop(ctx context.Context, host, msg string, running func() bool) (steward, error) {
	if err := awaitNoHeadless(ctx, a.cliSession(host), sendWait); err != nil {
		return steward{}, err
	}
	find := func(ctx context.Context, tried []string) (steward, error) {
		s, err := a.findSteward(host, append(tried, host))
		if err != nil {
			return s, err
		}
		return s, a.makeRoom(ctx, host, s.host)
	}
	return delegate(ctx, find, func(steward) string { return sendRequest(host, msg) }, running, a.peerSend, sendWait)
}

// cliSession is the CLI session id the desktop session host runs under: its
// record's, else host's own id.
func (a *app) cliSession(host string) string {
	if r, ok := claude.ReadRecord(a.cfg, host); ok && r.CLISessionID != "" {
		return r.CLISessionID
	}
	return strings.TrimPrefix(host, "local_")
}

// sendWait bounds how long a steward's send takes to start a session's
// desktop CLI.
const sendWait = 2 * time.Minute

// sendTool is the desktop's session messaging tool.
const sendTool = "mcp__ccd_session_mgmt__send_message"

// sendRequest is the message that has a steward send msg to the desktop
// session host, a session beekeeper started that runs no CLI.
func sendRequest(host, msg string) string {
	return fmt.Sprintf(stewardPreamble+"the session %s, which beekeeper started, runs no CLI, and only the desktop's session messaging starts one. "+
		"Call %s once with session_id %q and the message below, word for word, then end the turn without another tool call and without a reply.\n\n%s",
		host, sendTool, host, msg)
}

// watchNoteFailedSuccessor files the one note for the person on rl's first
// successor that did not come up, and returns its number (0: none filed).
func watchNoteFailedSuccessor(w *Watcher, rl role, holder state.Party) int {
	person := watchApp(w.Host).cfg.Guide.Person
	if person == "" {
		return 0
	}
	d := noteDraft{
		Question: fmt.Sprintf("%q, the %s's successor beekeeper started, did not come up: no desktop CLI after its first turn, and its headless resume ended. Take the role over from a running desktop session (`beekeeper %s start --take-over`)?",
			holder.Name, rl.Name, rl.Name),
		StatusQuo: fmt.Sprintf("%s holds the role in name only: %s", holder.Name, rl.Gone),
		Why:       "the desktop did not run the successor's CLI (`journalctl --user -u beekeeper-notify` and the desktop's main.log say why)",
		Options:   []string{"take over: the running session holds the role and arms its watch", "leave it: the standby watch retries"},
	}
	var id int
	_ = watchApp(w.Host).store.Update(func(st *state.State) ([]state.Event, error) {
		st.NextNote++
		id = st.NextNote
		n := state.Note{ID: id, For: person, Text: d.text(), By: WatchParty, At: watchApp(w.Host).now.UTC(), Kind: noteDecision,
			Default: fmt.Sprintf("the standby watch starts at most %d successors, %s apart and doubling", maxFailedSuccessors, dur(successorBackoff))}
		st.Notes = append(st.Notes, n)
		return []state.Event{event(WatchParty, "note.add", "#%d %s", n.ID, n.Text)}, nil
	})
	return id
}

// peerSend sends from the command line with the peer package.
func (a *app) peerSend(ctx context.Context, to, msg string) error {
	target, err := a.instructionTarget(to)
	if err != nil {
		return err
	}
	if target != nil {
		_, err := a.relayInstruction(ctx, target.ID, instructionAddress(target), msg)
		return err
	}
	_, err = peer.Sender{Dir: a.cfg.StateDir}.Send(ctx, to, msg)
	if errors.Is(err, peer.ErrUnreachable) {
		return fmt.Errorf("%w: its CLI stopped", err)
	}
	return err
}
