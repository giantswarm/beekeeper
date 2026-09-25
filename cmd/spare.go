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
	"sync/atomic"
	"time"

	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/peer"
	"github.com/giantswarm/beekeeper/internal/proc"
	"github.com/giantswarm/beekeeper/internal/state"
)

// keepAwakeMessage is the shortest message that makes the spare run one
// turn and nothing else: a turn is what resets the desktop's idle clock.
const keepAwakeMessage = `beekeeper keep-awake: reply "ok", nothing else`

// takeOverMessage tells a session to take the supervisor's role: the
// handover prompt itself is too long to relay through the sender's turn.
func takeOverMessage(why string) string {
	return "beekeeper: " + why + ". Run `beekeeper handover --prompt` and follow it."
}

// desktopApp is the Claude desktop app's executable: it starts the app, or
// hands a claude:// link to the running one.
const desktopApp = "claude-desktop"

// keepAwakeCheck is how long after a keep-awake the spare must have run its
// turn.
const keepAwakeCheck = 3 * time.Minute

// spareWatch is what the standby watch remembers of the spare and of the
// messages it sent; a restarted watch starts afresh.
type spareWatch struct {
	send func(ctx context.Context, to, msg string) error
	// open hands a claude:// link to the desktop app (openDesktop).
	open func(ctx context.Context, url string, running bool) error
	busy atomic.Bool
	// sent is the last keep-awake per spare, checked once for its turn.
	sent    map[string]time.Time
	checked map[string]bool
	// asleep is the spare said to have no CLI; handed and reopened are the
	// gone supervisor terms handed over and reopened.
	asleep, handed, reopened string
	// liveAt is when this watch last saw the supervisor's CLI of the term
	// liveTerm run.
	liveTerm string
	liveAt   time.Time
}

func (a *app) supervisorSpareCmd() *cobra.Command {
	var clear bool
	return withClear(&cobra.Command{
		Use:   "spare <session> | --clear",
		Short: "Name the session that takes over after a crash; the standby watch keeps it awake",
		Long: `Record the relay spare: a session kept ready to take the role. The standby
watch (beekeeper-notify) sends it a keep-awake whenever it sat idle for
supervisor.keepAwake (25m), under the desktop app's 30-minute idle
disconnect, so a message from the command line still reaches it. When the
supervisor's CLI stays gone past supervisor.restartGrace, the standby watch
sends the spare the hand-over from the command line and its
` + "`beekeeper supervisor start`" + ` takes the role. A relay to the spare is unchanged:
` + "`beekeeper supervisor relay <spare>`" + ` and the handover prompt sent with the
desktop's SendMessage to its local_ id. Its start clears the spare. The
session is a name, a unique part of one, a session id or a PID.`,
		Args: func(_ *cobra.Command, args []string) error {
			if clear != (len(args) == 0) {
				return usageErr("name the spare, or --clear without one")
			}
			return nil
		},
		RunE: func(_ *cobra.Command, args []string) error {
			me, err := a.caller()
			if err != nil {
				return err
			}
			sessions, _, err := a.sessions()
			if err != nil {
				return err
			}
			var to *state.Party
			if !clear {
				s, err := claude.Resolve(sessions, args[0])
				if err != nil {
					return usageErr("%v", err)
				}
				p := s.Party()
				to = &p
			}
			var msg string
			err = a.store.Update(func(st *state.State) ([]state.Event, error) {
				if err := supervisorRole.mustHold(st.SupervisorRole(), me); err != nil {
					return nil, err
				}
				if to != nil && to.Is(me) {
					return nil, usageErr("you supervise: name another session")
				}
				st.Spare = to
				if to == nil {
					msg = "no spare is recorded"
					return []state.Event{event(me, "supervisor.spare", "cleared")}, nil
				}
				msg = fmt.Sprintf("%q is the spare: the standby watch keeps it awake and hands it the role after a crash", to.Name)
				return []state.Event{event(me, "supervisor.spare", "%s", to.Name)}, nil
			})
			if err != nil {
				return err
			}
			_, err = fmt.Fprintln(a.out, msg)
			return err
		},
	}, &clear, "forget the spare")
}

func withClear(c *cobra.Command, clear *bool, usage string) *cobra.Command {
	c.Flags().BoolVar(clear, "clear", false, usage)
	return c
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

// keepAwakeDue reports whether the spare, last active at active and last
// sent a keep-awake at sent, is due another at now.
func keepAwakeDue(active, sent, now time.Time, every time.Duration) bool {
	if sent.After(active) {
		active = sent
	}
	return now.Sub(active) >= every
}

// tendSpare keeps the spare awake (standby watch): silent unless it fails.
func (w *watcher) tendSpare(ctx context.Context, st *state.State, sessions []*claude.Session) {
	sp := st.Spare
	if sp == nil {
		return
	}
	s, live := claude.Live(sessions, *sp)
	if !live {
		if w.spare.asleep != sp.Name {
			w.spare.asleep = sp.Name
			w.emitNow("spare", "SPARE ASLEEP: %q has no running CLI: no keep-awake or hand-over reaches it until a message from the desktop starts it", sp.Name)
		}
		return
	}
	w.spare.asleep = ""
	key := s.Key()
	sent := w.spare.sent[key]
	if !sent.IsZero() && !w.spare.checked[key] && w.now.Sub(sent) >= keepAwakeCheck {
		w.spare.checked[key] = true
		if s.LastActive.Before(sent) {
			w.emitNow("spare", "KEEP-AWAKE FAILED: %q ran no turn on the keep-awake sent at %s (held for its user's approval?)", s.Name, clock(w.now, sent))
		}
	}
	if !keepAwakeDue(s.LastActive, sent, w.now, w.cfg.Supervisor.KeepAwake.Duration) {
		return
	}
	if w.sendAsync(ctx, s.Name, keepAwakeMessage, func(err error) {
		if err != nil {
			w.emitNow("spare", "KEEP-AWAKE FAILED: %q: %v", s.Name, err)
		}
	}) {
		w.spare.sent[key], w.spare.checked[key] = w.now, false
	}
}

// handOver sends the spare the hand-over after the supervisor's CLI stayed
// gone past the grace, once per gone term, and returns what the GONE line
// and its notification add: whom it hands over to, or why it cannot.
func (w *watcher) handOver(ctx context.Context, st *state.State, sessions []*claude.Session, term string) string {
	sp := st.Spare
	if sp == nil {
		return ""
	}
	s, live := claude.Live(sessions, *sp)
	if !live {
		return fmt.Sprintf("; its spare %q has no running CLI", sp.Name)
	}
	if w.spare.handed != term && w.sendAsync(ctx, s.Name, takeOverMessage(fmt.Sprintf("the supervisor %q is gone and you are its spare", st.Supervisor.Name)), func(err error) {
		if err != nil {
			w.emitNow("spare", "HANDOVER FAILED: %q: %v", s.Name, err)
			return
		}
		w.emitNow("spare", "HANDOVER: sent %q the hand-over from the command line", s.Name)
	}) {
		w.spare.handed = term
	}
	return fmt.Sprintf("; handing over to its spare %q", sp.Name)
}

// reopenAfterAppStart opens the gone supervisor's row once when the desktop
// app started after its CLI stopped and no spare can take over: an app
// restart, and a reboot, leave every CLI cold, so the focus starts it.
func (w *watcher) reopenAfterAppStart(ctx context.Context, st *state.State, gone time.Time, term string) {
	sup := st.Supervisor
	if w.table == nil || sup.HostSession == "" || w.spare.reopened == term {
		return
	}
	liveAt := w.spare.liveAt
	if w.spare.liveTerm != term {
		liveAt = time.Time{}
	}
	if !reopenDue(desktopStart(w.table), gone, liveAt) {
		return
	}
	w.spare.reopened = term
	if err := w.spare.open(ctx, continueURL(sup.HostSession), true); err != nil {
		w.emitNow("spare", "REOPEN FAILED: %q: %v", sup.Name, err)
		return
	}
	w.emitNow("spare", "REOPENED: %q after the desktop app started", sup.Name)
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

// resumeRestarted records a supervisor CLI back under a new PID (standby
// watch, while the supervisor's own watch is gone with its old CLI) and
// tells it to resume the role: a restarted CLI has lost its watch.
func (w *watcher) resumeRestarted(ctx context.Context, st *state.State, sessions []*claude.Session) {
	if s, live := claude.Live(sessions, st.Supervisor.Party); !live || (st.SupervisorCLI.Of(st.Supervisor) && st.SupervisorCLI.PID == s.PID) {
		return
	}
	var restarted []state.Event
	var sup *state.Supervisor
	err := w.store.Update(func(st *state.State) ([]state.Event, error) {
		_, evs := observeCLI(st, sessions, w.now)
		restarted, sup = slices.DeleteFunc(evs, func(e state.Event) bool { return e.Verb != "supervisor.restart" }), st.Supervisor
		return evs, nil
	})
	if err != nil || len(restarted) == 0 || sup == nil {
		return
	}
	s, live := claude.Live(sessions, sup.Party)
	if !live {
		return
	}
	_ = w.sendAsync(ctx, s.Name, takeOverMessage("your CLI restarted ("+restarted[0].Detail+") and its watch is gone"), func(err error) {
		if err != nil {
			w.emitNow("spare", "RESUME FAILED: %q: %v", s.Name, err)
			return
		}
		w.emitNow("spare", "RESUME: sent %q the hand-over from the command line (%s)", s.Name, restarted[0].Detail)
	})
}

// sendAsync sends one message at a time from the command line, outside the
// poll: a send is a headless turn of 5 to 15 s. It reports whether the send
// started: one in flight drops it, and the next poll tries again.
func (w *watcher) sendAsync(ctx context.Context, to, msg string, done func(error)) bool {
	if !w.spare.busy.CompareAndSwap(false, true) {
		return false
	}
	go func() {
		defer w.spare.busy.Store(false)
		done(w.spare.send(ctx, to, msg))
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
