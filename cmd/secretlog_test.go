package cmd

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gofrs/flock"

	"github.com/giantswarm/beekeeper/internal/sandbox"
	"github.com/giantswarm/beekeeper/internal/state"
)

// secretEvents are the store's secret.* events, oldest first.
func secretEvents(t *testing.T, a *app) []state.Event {
	t.Helper()
	evs, err := a.store.Events(0, func(e state.Event) bool { return strings.HasPrefix(e.Verb, "secret.") })
	if err != nil {
		t.Fatal(err)
	}
	return evs
}

// A call's entry waits for a busy state lock, longer than Log would, instead
// of being dropped: the call ends logged, with the operation's duration.
func TestSecretCallWaitsForABusyLog(t *testing.T) {
	a, _, repo := secretApp(t)
	src := filepath.Join(repo, "db.sops.yaml")
	l := flock.New(filepath.Join(a.cfg.StateDir, "state.lock"))
	if err := l.Lock(); err != nil {
		t.Fatal(err)
	}
	const held = 1500 * time.Millisecond
	go func() { time.Sleep(held); _ = l.Unlock() }()
	t0 := time.Now()
	if _, err := runSecret(a, fingerprintOp, src); err != nil || time.Since(t0) < held {
		t.Fatalf("fingerprint under a held lock = %v after %s", err, time.Since(t0))
	}
	evs := secretEvents(t, a)
	if len(evs) != 1 || evs[0].Verb != "secret.fingerprint" || evs[0].By.Name != a.as ||
		!strings.HasPrefix(evs[0].Detail, src+": ") || !strings.Contains(evs[0].Detail, " keys (") || !strings.HasSuffix(evs[0].Detail, "s)") {
		t.Errorf("the logged call = %+v", evs)
	}
}

// unloggedStore is a store whose log cannot be written.
type unloggedStore struct{ state.Store }

func (unloggedStore) Record(...state.Event) error { return errors.New("disk full") }

// A call whose entry cannot be written fails with the reason and the entry
// it lost, never ending unlogged; a call that failed itself keeps its own
// exit code, its message carrying both.
func TestSecretCallFailsWhenItsEntryIsNotWritten(t *testing.T) {
	a, _, repo := secretApp(t)
	a.store = unloggedStore{a.store}
	src := filepath.Join(repo, "db.sops.yaml")
	_, err := runSecret(a, fingerprintOp, src)
	if Code(err) != ExitError || !strings.Contains(err.Error(), "the call is not logged: disk full; the entry: "+a.as+" secret.fingerprint "+src+": ") || !strings.Contains(err.Error(), " keys (") {
		t.Errorf("fingerprint with an unwritable log: exit %d, %v", Code(err), err)
	}
	noSecret(t, "the error", err.Error())
	a.out = &bytes.Buffer{}
	_, err = runSecret(a, copyOp, dbRef, "--", "cat")
	if Code(err) != ExitRefused || !strings.Contains(err.Error(), "the call is not logged: disk full; the entry: "+a.as+" secret.copy "+dbRef+" to cat: failed: ") {
		t.Errorf("a refused copy with an unwritable log: exit %d, %v", Code(err), err)
	}
	noSecret(t, "the error", err.Error())
}

// The broker logs a brokered secret call that ran no process of its own to
// its end: one refused before it ran, one ended on the broker's deadline.
// A call that ran to its end logged itself.
func TestBrokerLogsASecretCallThatRanNoProcess(t *testing.T) {
	a, _, _ := secretApp(t)
	pid := os.Getpid()
	req := sandbox.Request{Op: sandbox.OpSecret, Args: []string{copyOp, dbRef, "x.sops.yaml#k", "--name=x"}}
	refusal := errors.New("the sandbox broker runs beekeeper secret compare only")
	h := a.secretCallLogged(time.Minute, func(context.Context, int, sandbox.Request) (sandbox.Reply, error) {
		return sandbox.Reply{}, refusal
	})
	if _, err := h(context.Background(), pid, req); !errors.Is(err, refusal) {
		t.Errorf("a refused call = %v", err)
	}
	// the process the deadline ends dies by SIGTERM: exit code -1
	h = a.secretCallLogged(20*time.Millisecond, func(ctx context.Context, _ int, _ sandbox.Request) (sandbox.Reply, error) {
		<-ctx.Done()
		return sandbox.Reply{Code: -1, Err: "killed\n"}, nil
	})
	r, err := h(context.Background(), pid, req)
	if err != nil || r.Code != -1 || r.Err != "killed\nbeekeeper: the call ended on the broker's deadline of 20ms\n" {
		t.Errorf("a call ended on the deadline = %+v, %v", r, err)
	}
	h = a.secretCallLogged(time.Minute, func(context.Context, int, sandbox.Request) (sandbox.Reply, error) {
		return sandbox.Reply{Out: "ok", Code: 1}, nil
	})
	if r, err := h(context.Background(), pid, req); err != nil || r.Out != "ok" || r.Err != "" {
		t.Errorf("a call that ran = %+v, %v", r, err)
	}
	evs := secretEvents(t, a)
	args := dbRef + " x.sops.yaml#k --name=x: failed: "
	if len(evs) != 2 || evs[0].Verb != "secret.copy" || evs[1].Verb != "secret.copy" ||
		!strings.HasPrefix(evs[0].Detail, args+refusal.Error()+" (") ||
		!strings.HasPrefix(evs[1].Detail, args+"ended on the broker's deadline of 20ms (") {
		t.Errorf("the broker's log = %+v", evs)
	}
	for _, e := range evs {
		if e.By.Name == "" {
			t.Errorf("an event without the requester: %+v", e)
		}
	}
	// a log the broker cannot write reaches the requester
	a.store = unloggedStore{a.store}
	if r, err := h(context.Background(), pid, req); err != nil || r.Out != "ok" {
		t.Errorf("a call that ran with an unwritable log = %+v, %v", r, err)
	}
	h = a.secretCallLogged(time.Minute, func(context.Context, int, sandbox.Request) (sandbox.Reply, error) {
		return sandbox.Reply{Code: -1}, nil
	})
	if r, err := h(context.Background(), pid, req); err != nil || !strings.HasPrefix(r.Err, "beekeeper: the call is not logged: disk full; the entry: ") || !strings.Contains(r.Err, " secret.copy "+args+"ended by a signal (") {
		t.Errorf("a signalled call with an unwritable log = %+v, %v", r, err)
	}
}
