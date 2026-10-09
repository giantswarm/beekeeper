package cmd

import (
	"bytes"
	"context"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/giantswarm/beekeeper/internal/guard"
	"github.com/giantswarm/beekeeper/internal/sandbox"
	"github.com/giantswarm/beekeeper/internal/secret"
	"github.com/giantswarm/beekeeper/internal/state"
)

// The broker's answers to sandbox.OpVault.
const (
	vaultUnlocked = "unlocked"
	vaultLocked   = "locked"
)

// keepVault starts the broker's vault keeper with secret.session and signs
// in at once through secret.signinCommand. The broker makes itself
// undumpable, so that no process of the user reads its memory or
// environment. The keeper holds the session for secret.sessionLifetime,
// keeps op from letting it idle out until then (keepVaultAlive) and writes
// its state for the watch. Without secret.session it keeps nothing.
func (a *app) keepVault(ctx context.Context) (*vaultBroker, error) {
	if !a.cfg.Secret.Session {
		return &vaultBroker{k: secret.NewKeeper(0, nil)}, nil
	}
	path, err := secret.SocketPath()
	if err != nil {
		return nil, err
	}
	statePath, err := secret.StatePath()
	if err != nil {
		return nil, err
	}
	writeState := func(st secret.VaultState) {
		if err := secret.WriteState(statePath, st); err != nil {
			fmt.Fprintf(os.Stderr, "vault state: %v\n", err)
		}
	}
	k := secret.NewKeeper(a.cfg.Secret.SessionLifetime.Duration, func(st secret.VaultState) {
		writeState(st)
		if st.Unlocked {
			fmt.Fprintf(os.Stderr, "vault keeper: unlocked until %s\n", st.Until.Local().Format(time.DateTime))
			return
		}
		fmt.Fprintln(os.Stderr, "vault keeper: locked")
	})
	// a restarted broker holds no session: its state says so
	writeState(k.State())
	if err := secret.Protect(); err != nil {
		return nil, fmt.Errorf("the vault keeper: %w", err)
	}
	go func() {
		if err := secret.ServeVault(ctx, path, k); err != nil {
			fmt.Fprintf(os.Stderr, "vault keeper: %v\n", err)
		}
	}()
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	v := &vaultBroker{k: k, wait: a.cfg.Secret.UnlockWait.Duration, store: secret.CredentialStore, uptime: secret.Uptime, backoff: vaultSigninBackoff,
		retrying: func(status string) {
			writeState(secret.VaultState{Retrying: status})
			a.vaultSigninLog("retrying: %s", status)
		},
		failed: func(cause secret.SigninCause, tries int, err error) {
			next := "the broker signs in again at the next call on the vault"
			if cause.Rejected() {
				next = "a sign-in note asks " + a.cfg.Guide.Person
			}
			writeState(secret.VaultState{Error: fmt.Sprintf("%s after %s: %v; %s", cause, triesOf(tries), err, next)})
			a.vaultSigninFailed(exe, cause, tries, err)
		}}
	if cmd := a.cfg.Secret.SigninCommand; len(cmd) > 0 {
		v.signin = func(ctx context.Context) (string, string, error) { return runSignin(ctx, cmd) }
	}
	v.ask(ctx)
	go keepVaultAlive(ctx, v, func(ctx context.Context, env string) error {
		// op whoami answers from the local session without counting as
		// activity: a listing reaches 1Password and resets the idle timeout
		if err := opVault(ctx, env, "vault", "list", "--format", "json"); err != nil && secret.Expired(err.Error()) {
			return err
		}
		return nil
	})
	return v, nil
}

// drop forgets a session op no longer takes, says so in the journal and on
// the watch (VAULT SESSION DROPPED), and signs in again at once.
func (v *vaultBroker) drop(ctx context.Context, reason string) {
	fmt.Fprintf(os.Stderr, "vault keeper: op no longer takes the session (%s): signing in again\n", reason)
	v.k.Drop(reason, time.Now())
	v.ask(ctx)
}

// vaultBroker signs the broker in to the vault, one sign-in at a time:
// every call that waits on the vault shares it.
type vaultBroker struct {
	k *secret.Keeper
	// signin answers a session op signin would print, without the person
	// (secret.signinCommand); nil leaves it to beekeeper secret unlock.
	signin func(context.Context) (name, token string, err error)
	// store answers the state of the person's credential store
	// (secret.CredentialStore); nil never waits on it.
	store func(context.Context) (secret.StoreState, error)
	// uptime is how long the machine is up (secret.Uptime); nil: long enough.
	uptime func() (time.Duration, error)
	// backoff is the wait before each retry, the last one repeated.
	backoff []time.Duration
	// storeEvery is how often a store the sign-in waits for is looked at
	// (vaultStoreEvery).
	storeEvery time.Duration
	// retrying hears what the running sign-in waits on or retries after.
	retrying func(status string)
	// failed hears why a sign-in failed for good, after its tries.
	failed func(cause secret.SigninCause, tries int, err error)
	wait   time.Duration
	mu     sync.Mutex
	asking bool
}

// A failed sign-in is tried again after vaultSigninBackoff's waits, the
// last repeated, within the ask's window (secret.unlockWait); one try has
// vaultTryTimeout. Within vaultBootGrace of the boot the keeper waits for
// the credential store to come up, looking every vaultStoreEvery; a
// locked store is waited for at any time.
var vaultSigninBackoff = []time.Duration{30 * time.Second, time.Minute, 2 * time.Minute, 5 * time.Minute}

const (
	vaultTryTimeout = 2 * time.Minute
	vaultBootGrace  = 10 * time.Minute
	vaultStoreEvery = 15 * time.Second
)

// ask starts a sign-in unless the vault is unlocked or a sign-in runs; it
// ends after secret.unlockWait.
func (v *vaultBroker) ask(ctx context.Context) {
	if v.signin == nil || v.k.State().Unlocked {
		return
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.asking {
		return
	}
	v.asking = true
	go func() {
		sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), v.wait)
		defer cancel()
		fmt.Fprintln(os.Stderr, "vault keeper: signing in")
		if !v.signinUntil(sctx) {
			// a sign-in that succeeded ended itself in its unlock
			v.mu.Lock()
			v.asking = false
			v.mu.Unlock()
		}
	}()
}

// signinUntil signs in, trying again after v.backoff's waits while ctx
// lasts, and gives up once the next try would not fit: every failure is
// one journal line with its cause, and the retry one status for the
// watch. A store the sign-in cannot succeed without is waited for first.
// It answers whether the sign-in unlocked the keeper.
func (v *vaultBroker) signinUntil(ctx context.Context) bool {
	var tries int
	for {
		store, err := v.waitStore(ctx)
		if err == nil {
			tries++
			tctx, cancel := context.WithTimeout(ctx, vaultTryTimeout)
			var name, token string
			name, token, err = v.signin(tctx)
			cancel()
			if err == nil {
				err = v.unlock(name, token)
			}
			if err == nil {
				return true
			}
		}
		cause := secret.ClassifySignin(err, store)
		next := time.Now().Add(v.backoffFor(tries))
		if deadline, ok := ctx.Deadline(); ctx.Err() != nil || (ok && next.After(deadline)) {
			v.giveUp(cause, tries, err)
			return false
		}
		at := next.Local().Format(time.TimeOnly)
		fmt.Fprintf(os.Stderr, "vault keeper: the sign-in failed (%s): %v; try %d at %s\n", cause, err, tries+1, at)
		v.status(fmt.Sprintf("%s (%s); try %d at %s", cause, lastOf(err.Error()), tries+1, at))
		select {
		case <-ctx.Done():
			v.giveUp(cause, tries, err)
			return false
		case <-time.After(time.Until(next)):
		}
	}
}

// unlock gives the keeper the signed-in session and ends the sign-in in
// one step: a drop right after it finds no sign-in running and asks anew,
// where one in between would find the old sign-in still running and lose its
// own.
func (v *vaultBroker) unlock(name, token string) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if err := v.k.Unlock(name, token, time.Now()); err != nil {
		return err
	}
	v.asking = false
	return nil
}

// backoffFor is the wait before the try after tries failed ones.
func (v *vaultBroker) backoffFor(tries int) time.Duration {
	b := v.backoff
	if len(b) == 0 {
		b = vaultSigninBackoff
	}
	return b[min(max(tries, 1), len(b))-1]
}

// waitStore waits for the person's credential store where the sign-in
// cannot succeed without it: one that is locked, and one not up yet
// within vaultBootGrace of the boot (later an absent one may be no Secret
// Service at all, and the command runs). It answers the store's state as
// last read and, when ctx ended first, the wait as the error.
func (v *vaultBroker) waitStore(ctx context.Context) (secret.StoreState, error) {
	if v.store == nil {
		return secret.StoreUnknown, nil
	}
	every := v.storeEvery
	if every == 0 {
		every = vaultStoreEvery
	}
	start := time.Now()
	var last secret.StoreState
	for {
		st, _ := v.store(ctx)
		if st == secret.StoreReady || st == secret.StoreUnknown || (st == secret.StoreAbsent && !v.booting()) {
			return st, nil
		}
		if st != last {
			last = st
			fmt.Fprintf(os.Stderr, "vault keeper: waiting for the credential store: %s; looking every %s\n", st, every)
			v.status(fmt.Sprintf("waiting for the credential store: %s; looking every %s", st, every))
		}
		select {
		case <-ctx.Done():
			return st, fmt.Errorf("%s: waited %s for it", st, time.Since(start).Round(time.Second))
		case <-time.After(every):
		}
	}
}

// booting reports whether the machine is within vaultBootGrace of its boot,
// when the person's credential store may not be up yet.
func (v *vaultBroker) booting() bool {
	if v.uptime == nil {
		return false
	}
	up, err := v.uptime()
	return err == nil && up < vaultBootGrace
}

// status tells the watch what the running sign-in waits on or retries after.
func (v *vaultBroker) status(s string) {
	if v.retrying != nil {
		v.retrying(s)
	}
}

// giveUp reports a sign-in that failed for good: the journal, and v.failed
// for the state and the person.
func (v *vaultBroker) giveUp(cause secret.SigninCause, tries int, err error) {
	fmt.Fprintf(os.Stderr, "vault keeper: the sign-in failed (%s) after %s: %v\n", cause, triesOf(tries), err)
	if v.failed != nil {
		v.failed(cause, tries, err)
	}
}

// triesOf is n tries, in words.
func triesOf(n int) string {
	if n == 1 {
		return "1 try"
	}
	return fmt.Sprintf("%d tries", n)
}

// vaultParty is the keeper in the event log.
var vaultParty = state.Party{Name: "the vault keeper"}

// vaultSigninLog is one line of the event log about the keeper's sign-in.
func (a *app) vaultSigninLog(format string, args ...any) {
	if err := a.store.Record(event(vaultParty, "vault.signin", format, args...)); err != nil {
		fmt.Fprintf(os.Stderr, "vault keeper: %v\n", err)
	}
}

// vaultSigninFailed logs a sign-in that failed for good and, for a password
// 1Password rejected while the credential store answered, leaves one
// sign-in note for the guide's person, which closes by itself once the
// broker holds a session (beekeeper secret status). A failure that passes
// by itself leaves no note: the broker signs in again at the next call.
func (a *app) vaultSigninFailed(exe string, cause secret.SigninCause, tries int, err error) {
	if uerr := a.store.Update(func(st *state.State) ([]state.Event, error) {
		evs := []state.Event{event(vaultParty, "vault.signin", "failed (%s) after %s: %s", cause, triesOf(tries), lastOf(err.Error()))}
		probe := guard.ShellQuote(exe) + " secret status"
		if !cause.Rejected() || slices.ContainsFunc(st.Notes, func(n state.Note) bool { return n.Kind == noteLogin && n.Until == probe }) {
			return evs, nil
		}
		now := time.Now().UTC()
		st.NextNote++
		n := state.Note{ID: st.NextNote, For: a.cfg.Guide.Person, By: vaultParty, At: now, Due: now, Kind: noteLogin, Until: probe,
			Text: fmt.Sprintf("Sign in to the vault: 1Password rejected the password the broker's sign-in command reads, in %s (%s). The entry the command reads is wrong or rotated: fix it, then `systemctl --user restart beekeeper-sandbox`, or wait for the next call on the vault, when the broker signs in again.",
				triesOf(tries), lastOf(err.Error())),
			Default: "every call on the vault waits secret.unlockWait and exits 78: the agents' secret steps stay blocked"}
		st.Notes = append(st.Notes, n)
		return append(evs, event(vaultParty, "note.add", "#%d %s", n.ID, n.Text)), nil
	}); uerr != nil {
		fmt.Fprintf(os.Stderr, "vault keeper: %v\n", uerr)
	}
}

// runSignin runs secret.signinCommand and reads the session it prints, as op
// signin prints it, into memory; its other output goes to the journal.
func runSignin(ctx context.Context, argv []string) (string, string, error) {
	c := exec.CommandContext(ctx, argv[0], argv[1:]...) //nolint:gosec // the configured sign-in
	var out bytes.Buffer
	var stderr strings.Builder
	c.Env = withoutVaultEnv(os.Environ())
	c.Stdout, c.Stderr = &out, &stderr
	err := c.Run()
	defer out.Reset()
	if err != nil {
		return "", "", fmt.Errorf("%s: %w: %s", strings.Join(argv, " "), err, lastOf(stderr.String()))
	}
	return secret.ParseSignin(out.Bytes())
}

// The keeper touches op's session every vaultTouchEvery, well inside op's
// 30-minute idle timeout, looking every vaultTendEvery.
const (
	vaultTouchEvery = 10 * time.Minute
	vaultTendEvery  = time.Minute
)

// keepVaultAlive touches the session within its lifetime, so that op does
// not let it idle out; the keeper ends it at the end of its lifetime.
func keepVaultAlive(ctx context.Context, v *vaultBroker, touch func(context.Context, string) error) {
	t := time.NewTicker(vaultTendEvery)
	defer t.Stop()
	var touched time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			touched = tendVault(ctx, v, now, touched, touch)
		}
	}
}

// tendVault is one look of keepVaultAlive at now: it touches a session
// untouched for vaultTouchEvery and drops it, signing in again, when op no
// longer takes it. It answers when the session was last touched.
func tendVault(ctx context.Context, v *vaultBroker, now, touched time.Time, touch func(context.Context, string) error) time.Time {
	st := v.k.State()
	if st.Since.After(touched) {
		touched = st.Since
	}
	if !st.Unlocked || now.Sub(touched) < vaultTouchEvery {
		return touched
	}
	if err := touch(ctx, v.k.Env()); err != nil {
		v.drop(ctx, err.Error())
	}
	return now
}

// opVault runs one of the broker's own op calls with the session; its
// output goes nowhere, its error carries op's message.
func opVault(ctx context.Context, env string, args ...string) error {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	op := exec.CommandContext(ctx, "op", args...) //nolint:gosec // the broker's own op call
	var stderr strings.Builder
	op.Env = append(withoutVaultEnv(os.Environ()), env)
	op.Stderr = &stderr
	if err := op.Run(); err != nil {
		return fmt.Errorf("op %s: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// brokeredVault runs a brokered secret call with the vault session when it
// takes the vault (secretNeedsVault, its files relative to the requester's
// directory, as the client decides). While the keeper holds none, it signs in (one
// sign-in shared by every waiting call) and waits until secret.unlockWait
// passes (exit ExitVault, the call logged as the requester's, since no
// process of its own ran), listed in the broker's waits for the watch's
// VAULT LOCKED line and beekeeper status. A session op no longer takes is
// dropped, signed in again and the call retried once.
func (a *app) brokeredVault(v *vaultBroker, call func(env []string) sandbox.Handler) sandbox.Handler {
	waits := &vaultWaits{}
	return func(ctx context.Context, pid int, req sandbox.Request) (sandbox.Reply, error) {
		if !a.cfg.Secret.Session {
			return call(nil)(ctx, pid, req)
		}
		start := time.Now()
		needs := a.secretArgsNeedVault(req.Args)
		if !needs && (len(a.cfg.Secret.AgeIdentities) > 0 || a.cfg.Secret.Vault != "") {
			// the files of a call are the requester's, read only for an age identity
			cwd, _, err := sandbox.Origin("/proc", pid, nil)
			if err != nil {
				return sandbox.Reply{}, fmt.Errorf("the requester: %w", err)
			}
			needs = a.ageOps().AgeNeedsVault(cwd, req.Args)
		}
		if !needs {
			return call(nil)(ctx, pid, req)
		}
		for try := 0; ; try++ {
			if !v.k.State().Unlocked {
				v.ask(ctx)
				done := waits.add(secret.VaultWait{Who: requester(pid), Ref: vaultRef(req.Args), Since: time.Now()})
				wctx, cancel := context.WithTimeout(ctx, a.cfg.Secret.UnlockWait.Duration)
				err := v.k.Wait(wctx)
				cancel()
				done()
				if err != nil {
					wait := a.cfg.Secret.UnlockWait.Duration
					r := sandbox.Reply{Code: ExitVault, Err: fmt.Sprintf("beekeeper: %v: the broker's sign-in did not unlock it within %s (journalctl --user -u beekeeper-sandbox says why)\n", err, wait)}
					return withBrokerLog(r, a.brokerSecretLog(pid, req, start, fmt.Sprintf("the vault stayed locked for %s", wait))), nil
				}
			}
			r, err := call([]string{v.k.Env()})(ctx, pid, req)
			if err != nil || r.Code != ExitVault || !secret.Expired(r.Err) || try > 0 {
				return r, err
			}
			v.drop(ctx, lastOf(r.Err))
		}
	}
}

// vaultWaits are the calls waiting on the person's approval, written to
// secret.WaitsPath on every change.
type vaultWaits struct {
	mu   sync.Mutex
	next int
	ws   map[int]secret.VaultWait
}

// add lists w until the returned func is called.
func (v *vaultWaits) add(w secret.VaultWait) func() {
	v.mu.Lock()
	if v.ws == nil {
		v.ws = map[int]secret.VaultWait{}
	}
	id := v.next
	v.next++
	v.ws[id] = w
	v.writeLocked()
	v.mu.Unlock()
	return func() {
		v.mu.Lock()
		defer v.mu.Unlock()
		delete(v.ws, id)
		v.writeLocked()
	}
}

func (v *vaultWaits) writeLocked() {
	path, err := secret.WaitsPath()
	if err != nil {
		return
	}
	ws := slices.SortedFunc(maps.Values(v.ws), func(a, b secret.VaultWait) int { return a.Since.Compare(b.Since) })
	if err := secret.WriteWaits(path, ws); err != nil {
		fmt.Fprintf(os.Stderr, "vault waits: %v\n", err)
	}
}

// vaultRef is the first op:// reference of a call's arguments.
func vaultRef(args []string) string {
	for _, a := range args {
		if secret.NeedsVault([]string{a}) {
			return a
		}
	}
	return "the vault"
}

// brokeredVaultState answers whether the keeper holds the vault session.
func brokeredVaultState(v *vaultBroker) sandbox.Handler {
	return func(context.Context, int, sandbox.Request) (sandbox.Reply, error) {
		if v.k.State().Unlocked {
			return sandbox.Reply{Out: vaultUnlocked}, nil
		}
		return sandbox.Reply{Out: vaultLocked}, nil
	}
}
