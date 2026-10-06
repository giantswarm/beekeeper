package cmd

import (
	"bytes"
	"context"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/giantswarm/beekeeper/internal/omp"
	"github.com/giantswarm/beekeeper/internal/sandbox"
	"github.com/giantswarm/beekeeper/internal/secret"
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
	v := &vaultBroker{k: k, wait: a.cfg.Secret.UnlockWait.Duration, failed: func(err error) {
		writeState(secret.VaultState{Error: err.Error()})
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
	// failed hears why a sign-in failed.
	failed func(error)
	wait   time.Duration
	mu     sync.Mutex
	asking bool
}

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
		defer func() {
			v.mu.Lock()
			v.asking = false
			v.mu.Unlock()
		}()
		sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), v.wait)
		defer cancel()
		fmt.Fprintln(os.Stderr, "vault keeper: signing in")
		name, token, err := v.signin(sctx)
		if err == nil {
			err = v.k.Unlock(name, token, time.Now())
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "vault keeper: the sign-in failed: %v\n", err)
			if v.failed != nil {
				v.failed(err)
			}
		}
	}()
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
// names an op:// reference. While the keeper holds none, it signs in (one
// sign-in shared by every waiting call) and waits until secret.unlockWait
// passes (exit ExitVault), listed in the broker's waits for the watch's
// VAULT LOCKED line and beekeeper status. A session op no longer takes is
// dropped, signed in again and the call retried once.
func (a *app) brokeredVault(v *vaultBroker, call func(env []string) sandbox.Handler) sandbox.Handler {
	waits := &vaultWaits{}
	return func(ctx context.Context, pid int, req sandbox.Request) (sandbox.Reply, error) {
		if !a.cfg.Secret.Session || !secret.NeedsVault(req.Args) {
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
					return sandbox.Reply{Code: ExitVault, Err: fmt.Sprintf("beekeeper: %v: the broker's sign-in did not unlock it within %s (journalctl --user -u beekeeper-sandbox says why)\n",
						err, a.cfg.Secret.UnlockWait.Duration)}, nil
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

// requester names the session behind a request: its name, else its pid.
func requester(pid int) string {
	_, env, err := sandbox.Origin("/proc", pid, []string{"CLAUDE_CODE_SESSION_NAME", omp.EnvName})
	if err == nil {
		for _, kv := range env {
			if _, v, _ := strings.Cut(kv, "="); v != "" {
				return v
			}
		}
	}
	return "pid " + strconv.Itoa(pid)
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
