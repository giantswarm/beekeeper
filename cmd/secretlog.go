package cmd

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/giantswarm/beekeeper/internal/sandbox"
	"github.com/giantswarm/beekeeper/internal/state"
)

// The secret calls' audit log: every call is a secret.<operation> event with
// the calling session, the references, the outcome and the call's duration,
// never a value. A call logs itself at its end; one that never ran to its end
// in the broker (refused, waited on the locked vault in vain, ended on the
// broker's deadline) is logged by the broker.

// secretLog records one operation with the calling session and how long
// the operation took, waiting for the state lock as long as it takes. It
// answers result, the operation's own error; when the entry cannot be
// written the call fails with the reason and the entry it lost, so that no
// call ends unlogged.
func (a *app) secretLog(result error, op, format string, args ...any) error {
	return a.opLog(result, "secret."+op, format, args...)
}

// opLog is secretLog for any verb: an operation on credentials outside
// beekeeper secret (app create) is logged the same way.
func (a *app) opLog(result error, verb, format string, args ...any) error {
	who, err := a.caller()
	if err != nil {
		who = state.Party{Name: noSession}
	}
	e := event(who, verb, format, args...)
	e.Detail += fmt.Sprintf(" (%s)", time.Since(a.now).Round(time.Millisecond))
	if err := a.store.Record(e); err != nil {
		return errors.Join(result, &exitError{code: ExitError, msg: notLogged(e, err)})
	}
	return result
}

// notLogged says why e was not written, with the entry itself, so that the
// stderr of the call keeps what the log lost.
func notLogged(e state.Event, err error) string {
	return fmt.Sprintf("the call is not logged: %v; the entry: %s %s %s", err, e.By.Name, e.Verb, e.Detail)
}

// outcome is ok, or the error that ended an operation.
func outcome(err error, ok string) string {
	if err != nil {
		return "failed: " + err.Error()
	}
	return ok
}

// secretCallLogged bounds a brokered secret call's process by timeout and
// logs the call when no process of its own ran to its end, where it logs
// itself: one refused before it ran, one ended on the deadline or by a
// signal (exit code -1).
func (a *app) secretCallLogged(timeout time.Duration, h sandbox.Handler) sandbox.Handler {
	return func(ctx context.Context, pid int, req sandbox.Request) (sandbox.Reply, error) {
		ctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		start := time.Now()
		r, err := h(ctx, pid, req)
		switch {
		case err != nil:
			return r, errors.Join(err, a.brokerSecretLog(pid, req, start, err.Error()))
		case r.Code < 0 && ctx.Err() != nil:
			r.Err += fmt.Sprintf("beekeeper: the call ended on the broker's deadline of %s\n", timeout)
			return withBrokerLog(r, a.brokerSecretLog(pid, req, start, fmt.Sprintf("ended on the broker's deadline of %s", timeout))), nil
		case r.Code < 0:
			return withBrokerLog(r, a.brokerSecretLog(pid, req, start, "ended by a signal")), nil
		}
		return r, nil
	}
}

// brokerSecretLog records a brokered secret call that logged itself not,
// why it failed and after how long since start, as the requesting session:
// the operation and its arguments, which are references and flags, never a
// value. It answers the write's failure for the reply to carry.
func (a *app) brokerSecretLog(pid int, req sandbox.Request, start time.Time, why string) error {
	op, rest := "call", req.Args
	if len(req.Args) > 0 && slices.Contains(brokeredSecretOps, req.Args[0]) {
		op, rest = req.Args[0], req.Args[1:]
	}
	who := requesterParty(pid)
	who.Person, who.Team, who.Host = a.cfg.Identity.Person, a.cfg.Identity.Team, a.cfg.Identity.Host
	e := event(who, "secret."+op, "%s: failed: %s (%s)", strings.Join(rest, " "), why, time.Since(start).Round(time.Millisecond))
	if err := a.store.Record(e); err != nil {
		return errors.New(notLogged(e, err))
	}
	return nil
}

// withBrokerLog adds a failed broker log write to the reply's stderr.
func withBrokerLog(r sandbox.Reply, err error) sandbox.Reply {
	if err != nil {
		r.Err += "beekeeper: " + err.Error() + "\n"
	}
	return r
}

// requesterParty is the session behind a request, as its environment names
// it (callerEnv): its ids and its name, else its pid for a name.
func requesterParty(pid int) state.Party {
	vars := map[string]string{}
	if _, env, err := sandbox.Origin("/proc", pid, callerEnv); err == nil {
		for _, kv := range env {
			k, v, _ := strings.Cut(kv, "=")
			vars[k] = v
		}
	}
	p := envParty(func(k string) string { return vars[k] })
	if p.Name == "" {
		p.Name = "pid " + strconv.Itoa(pid)
	}
	return p
}

// requester names the session behind a request: its name, else its pid.
func requester(pid int) string { return requesterParty(pid).Name }
