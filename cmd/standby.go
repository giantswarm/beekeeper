package cmd

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/peer"
	"github.com/giantswarm/beekeeper/internal/proc"
	"github.com/giantswarm/beekeeper/internal/state"
)

// resumeMessage tells a role's holder to take its role up again in a new
// CLI: the handover prompt itself is too long to relay through the
// sender's turn.
func (rl role) resumeMessage(why string) string {
	return "beekeeper: " + why + ". Run `" + rl.handover + "` and follow it."
}

// desktopApp is the Claude desktop app's executable: it starts the app, or
// hands a claude:// link to the running one.
const desktopApp = "claude-desktop"

// reopenGrace is how long the standby watch gives a supervisor whose row it
// reopened after the desktop app started to come back before it starts a
// successor.
const reopenGrace = 3 * time.Minute

// standbyWatch is what the standby watch remembers of the messages it sent,
// the successors it started and the rows it reopened; a restarted watch
// starts afresh, and the relay a successor's start opens is in the state.
type standbyWatch struct {
	send func(ctx context.Context, to, msg string) error
	// open hands a claude:// link to the desktop app (openDesktop).
	open func(ctx context.Context, url string, running bool) error
	// succeed starts rl's next run after its holder from is gone
	// (app.startSuccessor).
	succeed func(ctx context.Context, rl role, from state.Party) (state.Party, error)
	// turning reports whether a unit of beekeeper's start or wake of
	// session id runs a turn or the reopen after it (unitsTurning); nil:
	// none does.
	turning func(ctx context.Context, id string) bool
	busy    atomic.Bool
	// guideGap is the term of the gone guide this watch said.
	guideGap string
	// starting is the role whose successor is being started, one at a time;
	// inflight waits for it (a watch --once).
	starting atomic.Bool
	inflight sync.WaitGroup
	// reopened is the gone supervisor term reopened, at reopenedAt.
	reopened   string
	reopenedAt time.Time
	// liveAt is when this watch last saw the supervisor's CLI of the term
	// liveTerm run.
	liveTerm string
	liveAt   time.Time
}

func (a *app) supervisorReopenCmd() *cobra.Command {
	var print bool
	c := &cobra.Command{
		Use:   "reopen",
		Short: "Open the recorded supervisor's desktop session, starting the app if needed (the login unit)",
		Long: `Open claude://code/continue?session=local_… for the recorded supervisor: it
starts the desktop app on the supervisor's row, or switches the running app
to it. Right after the app starts no CLI runs yet, so the focus starts the
supervisor's CLI; the standby watch then sees it back and tells it to
resume the role. The beekeeper-supervisor-open user unit runs this at
login. It does nothing when no supervisor is recorded or it has no desktop
session.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			st, err := a.store.Read()
			if err != nil {
				return err
			}
			if st.Supervisor == nil || st.Supervisor.HostSession == "" {
				_, err := fmt.Fprintln(a.out, "no supervisor with a desktop session is recorded: nothing to open")
				return err
			}
			url := continueURL(st.Supervisor.HostSession)
			if print {
				_, err := fmt.Fprintln(a.out, url)
				return err
			}
			t, err := proc.Read()
			if err != nil {
				return err
			}
			if err := openDesktop(cmd.Context(), url, !desktopStart(t).IsZero()); err != nil {
				return err
			}
			_, err = fmt.Fprintf(a.out, "opened %q: %s\n", st.Supervisor.Name, url)
			return err
		},
	}
	c.Flags().BoolVar(&print, "print", false, "print the link instead of opening it")
	return c
}

func continueURL(host string) string { return "claude://code/continue?session=" + host }

// openDesktop hands url to the running app, or starts the app on it in a
// scope of its own under app.slice, where the desktop starts it too: the
// app outlives the unit or watch that started it.
func openDesktop(ctx context.Context, url string, running bool) error {
	if running {
		return exec.CommandContext(ctx, desktopApp, url).Run() //nolint:gosec // a claude:// link built from the state's local_ id
	}
	c := exec.Command("systemd-run", "--user", "--scope", "--quiet", "--slice=app.slice", //nolint:gosec // as above
		"--unit=app-com.anthropic.Claude-beekeeper-"+strconv.FormatInt(time.Now().Unix(), 10), desktopApp, url)
	if err := c.Start(); err != nil {
		return err
	}
	return c.Process.Release()
}

// desktopStart is when the desktop app's main process started, zero when
// it does not run. Electron rewrites its command line into one string, so
// the arguments are its fields.
func desktopStart(t *proc.Table) time.Time {
	for _, p := range t.ByPID {
		args := strings.Fields(p.Cmdline())
		if len(args) > 0 && filepath.Base(args[0]) == desktopApp &&
			!slices.ContainsFunc(args, func(a string) bool { return strings.HasPrefix(a, "--type=") }) {
			return p.Start
		}
	}
	return time.Time{}
}

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
	to, _, err := a.startSuccessor(ctx, rl, from, watchParty, "")
	return to, err
}

// firstTurn reports whether p's session is between beekeeper's start or
// wake and its desktop CLI: its headless turn runs, or the reopen after it.
// Its CLI is gone meanwhile without the session being gone.
func (w *watcher) firstTurn(ctx context.Context, p state.Party) bool {
	return p.Session != "" && w.stand.turning != nil && w.stand.turning(ctx, p.Session)
}

// unitsTurning reports whether a start or wake unit of session id is
// active, starting or running its reopen (deactivating).
func unitsTurning(ctx context.Context, id string) bool {
	if len(id) < 8 {
		return false
	}
	out, _ := exec.CommandContext(ctx, "systemctl", "--user", "list-units", "--plain", "--no-legend", //nolint:gosec // the units beekeeper named
		"--state=active,activating,deactivating", "beekeeper-agent-"+id[:8]+".service", wakePrefix(id)+"*").Output()
	return strings.TrimSpace(string(out)) != ""
}

// guideGone says once when the guide's CLI stayed gone past its grace with
// no relay open, and starts its successor (standby watch).
func (w *watcher) guideGone(ctx context.Context, st *state.State, sessions []*claude.Session) {
	r := guideRole.get(st)
	sv := readHolder(r, sessions, w.now, w.cfg.Guide.RestartGrace.Duration)
	if !sv.down() || relayPending(st, r, w.now) || w.firstTurn(ctx, r.Holder.Party) {
		if sv.live {
			w.stand.guideGap = ""
		}
		return
	}
	key := r.Holder.Name + "@" + r.Holder.Since.UTC().Format(time.RFC3339)
	successor := w.succeedGone(ctx, guideRole, r.Holder.Party)
	if w.stand.guideGap != key {
		w.stand.guideGap = key
		w.emitNow("guide", "GUIDE GONE: %q (guiding since %s) is gone since %s%s", r.Holder.Name, clock(w.now, r.Holder.Since), clock(w.now, sv.gone), successor)
	}
}

// succeedGone starts rl's next run once its holder's CLI stayed gone past
// the grace (standby watch): the successor's start takes the role through
// the relay beekeeper opens to it, so a repeated poll and a restarted watch
// see the relay and start none. It returns what the GONE line adds.
func (w *watcher) succeedGone(ctx context.Context, rl role, holder state.Party) string {
	if w.stand.succeed == nil {
		return ""
	}
	if !w.stand.starting.CompareAndSwap(false, true) {
		return "; its successor is starting"
	}
	w.stand.inflight.Add(1)
	go func() {
		defer w.stand.inflight.Done()
		defer w.stand.starting.Store(false)
		to, err := w.stand.succeed(ctx, rl, holder)
		if err != nil {
			w.emitNow(rl.name+"-successor", "%sSUCCESSOR FAILED: %q is gone and its successor did not start: %v", rl.tag, holder.Name, err)
			return
		}
		w.emitNow(rl.name+"-successor", "%sSUCCESSOR: started %q, whose `beekeeper %s start` takes the role from the gone %q", rl.tag, to.Name, rl.name, holder.Name)
	}()
	return "; starting its successor"
}

// reopenAfterAppStart opens the gone supervisor's row once when the desktop
// app started after its CLI stopped: an app restart, and a reboot, leave
// every CLI cold, so the focus starts it. It reports whether the row was
// reopened within reopenGrace, while the supervisor may still come back.
func (w *watcher) reopenAfterAppStart(ctx context.Context, st *state.State, gone time.Time, term string) bool {
	sup := st.Supervisor
	if w.stand.reopened == term {
		return w.now.Sub(w.stand.reopenedAt) < reopenGrace
	}
	if w.table == nil || sup.HostSession == "" {
		return false
	}
	liveAt := w.stand.liveAt
	if w.stand.liveTerm != term {
		liveAt = time.Time{}
	}
	if !reopenDue(desktopStart(w.table), gone, liveAt) {
		return false
	}
	w.stand.reopened, w.stand.reopenedAt = term, w.now
	if err := w.stand.open(ctx, continueURL(sup.HostSession), true); err != nil {
		w.emitNow("reopen", "REOPEN FAILED: %q: %v", sup.Name, err)
		return false
	}
	w.emitNow("reopen", "REOPENED: %q after the desktop app started", sup.Name)
	return true
}

// reopenDue says whether the desktop app, started at start (zero: it does
// not run), started after the supervisor's CLI stopped: after beekeeper
// first saw the CLI gone at gone, or with this watch never having seen the
// CLI run under it (liveAt, zero when never). After a reboot the app starts
// at login before the standby watch's first poll, which first sees gone a
// CLI that stopped with the machine.
func reopenDue(start, gone, liveAt time.Time) bool {
	return !start.IsZero() && (!start.Before(gone) || liveAt.Before(start))
}

// resumeRestarted records a CLI of rl's holder back under a new PID
// (standby watch) and tells it to resume the role: a restarted CLI has lost
// its watch. A fresh successor's desktop CLI, which follows the headless
// first turn that took the role, is such a restart.
func (w *watcher) resumeRestarted(ctx context.Context, rl role, st *state.State, sessions []*claude.Session) {
	r := rl.get(st)
	if r.Holder == nil {
		return
	}
	if s, live := claude.Live(sessions, r.Holder.Party); !live || (r.CLI.Of(r.Holder) && r.CLI.PID == s.PID) {
		return
	}
	var restarted []state.Event
	var sup *state.Supervisor
	err := w.store.Update(func(st *state.State) ([]state.Event, error) {
		_, evs := rl.observeCLI(st, sessions, w.now)
		restarted, sup = slices.DeleteFunc(slices.Clone(evs), func(e state.Event) bool { return e.Verb != rl.name+".restart" }), rl.get(st).Holder
		return evs, nil
	})
	if err != nil || len(restarted) == 0 || sup == nil {
		return
	}
	s, live := claude.Live(sessions, sup.Party)
	if !live {
		return
	}
	_ = w.sendAsync(ctx, s.Name, rl.resumeMessage("your CLI restarted ("+restarted[0].Detail+") and its watch is gone"), func(err error) {
		if err != nil {
			w.emitNow(rl.name+"-resume", "%sRESUME FAILED: %q: %v", rl.tag, s.Name, err)
			return
		}
		w.emitNow(rl.name+"-resume", "%sRESUME: sent %q the hand-over from the command line (%s)", rl.tag, s.Name, restarted[0].Detail)
	})
}

// sendAsync sends one message at a time from the command line, outside the
// poll: a send is a headless turn of 5 to 15 s. It reports whether the send
// started: one in flight drops it, and the next poll tries again.
func (w *watcher) sendAsync(ctx context.Context, to, msg string, done func(error)) bool {
	if !w.stand.busy.CompareAndSwap(false, true) {
		return false
	}
	go func() {
		defer w.stand.busy.Store(false)
		done(w.stand.send(ctx, to, msg))
	}()
	return true
}

// peerSend sends from the command line with the peer package.
func (a *app) peerSend(ctx context.Context, to, msg string) error {
	_, err := peer.Sender{Dir: a.cfg.StateDir}.Send(ctx, to, msg)
	if errors.Is(err, peer.ErrUnreachable) {
		return fmt.Errorf("%w: its CLI stopped", err)
	}
	return err
}
