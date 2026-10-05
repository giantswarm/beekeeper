package cmd

import (
	"context"
	"fmt"
	"maps"
	"os"
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

// keepVault starts the broker's vault keeper with secret.session: the
// person's beekeeper secret unlock hands it the session over its socket, and
// the broker makes itself undumpable, so that no process of the user reads
// its memory or environment. Without secret.session it keeps nothing.
func (a *app) keepVault(ctx context.Context) (*secret.Keeper, error) {
	k := secret.NewKeeper()
	if !a.cfg.Secret.Session {
		return k, nil
	}
	path, err := secret.SocketPath()
	if err != nil {
		return nil, err
	}
	if err := secret.Protect(); err != nil {
		return nil, fmt.Errorf("the vault keeper: %w", err)
	}
	go func() {
		if err := secret.ServeVault(ctx, path, k); err != nil {
			fmt.Fprintf(os.Stderr, "vault keeper: %v\n", err)
		}
	}()
	return k, nil
}

// brokeredVault runs a brokered secret call with the vault session when it
// names an op:// reference. While the keeper holds none, the call waits for
// the person's unlock until secret.unlockWait passes (exit ExitVault),
// listed in the broker's waits for the watch's VAULT LOCKED line and
// beekeeper status; nothing asks the person. A session op no longer takes is
// forgotten and waited for once more.
func (a *app) brokeredVault(k *secret.Keeper, call func(env []string) sandbox.Handler) sandbox.Handler {
	waits := &vaultWaits{}
	return func(ctx context.Context, pid int, req sandbox.Request) (sandbox.Reply, error) {
		if !a.cfg.Secret.Session || !secret.NeedsVault(req.Args) {
			return call(nil)(ctx, pid, req)
		}
		for try := 0; ; try++ {
			if k.Env() == "" {
				done := waits.add(secret.VaultWait{Who: requester(pid), Ref: vaultRef(req.Args), Since: time.Now()})
				wctx, cancel := context.WithTimeout(ctx, a.cfg.Secret.UnlockWait.Duration)
				err := k.Wait(wctx)
				cancel()
				done()
				if err != nil {
					return sandbox.Reply{Code: ExitVault, Err: fmt.Sprintf("beekeeper: %v: the person did not unlock it within %s (beekeeper secret unlock in their own terminal)\n",
						err, a.cfg.Secret.UnlockWait.Duration)}, nil
				}
			}
			r, err := call([]string{k.Env()})(ctx, pid, req)
			if err != nil || r.Code != ExitVault || !secret.Expired(r.Err) || try > 0 {
				return r, err
			}
			k.Lock()
		}
	}
}

// vaultWaits are the calls waiting on the person's unlock, written to
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
func brokeredVaultState(k *secret.Keeper) sandbox.Handler {
	return func(context.Context, int, sandbox.Request) (sandbox.Reply, error) {
		if ok, _ := k.Status(); ok {
			return sandbox.Reply{Out: vaultUnlocked}, nil
		}
		return sandbox.Reply{Out: vaultLocked}, nil
	}
}
