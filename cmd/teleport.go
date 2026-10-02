package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/guard"
	"github.com/giantswarm/beekeeper/internal/lease"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/teleport"
)

// teleportFile is the keeper's record in the state directory.
const teleportFile = "teleport.json"

// teleportPurpose is the purpose of the browser lease a renewal holds.
const teleportPurpose = "Teleport login renewal"

// teleportRenewHint is how a person or session renews the login by hand.
const teleportRenewHint = "renew it: beekeeper teleport renew, holding the browser lease"

// The watch's condition keys of the Teleport login, one per kind of trouble.
const (
	teleportWarn       = "teleport-warn"
	teleportExpired    = "teleport-expired"
	teleportFailed     = "teleport-failed"
	teleportUnreadable = "teleport-unreadable"
)

var teleportKeys = []string{teleportWarn, teleportExpired, teleportFailed, teleportUnreadable}

// teleportView is the Teleport login as teleport, watch and snapshot show
// it: metadata and the keeper's record, never key material.
type teleportView struct {
	Cluster    string    `json:"cluster,omitempty"`
	Username   string    `json:"username,omitempty"`
	ValidUntil time.Time `json:"validUntil,omitzero"`
	// Error is why no profile was read: "not logged in", or tsh's error.
	Error  string          `json:"error,omitempty"`
	Keeper teleport.Record `json:"keeper"`
	// Key is the watch's condition key, empty while the login is fine.
	Key string `json:"condition,omitempty"`
}

// newTeleportView judges the profile p (read with perr) and the keeper's
// record at now: expired, failed to renew, or with less than warn left is a
// condition.
func newTeleportView(p teleport.Profile, perr error, rec teleport.Record, now time.Time, warn time.Duration) teleportView {
	v := teleportView{Cluster: p.Cluster, Username: p.Username, ValidUntil: p.ValidUntil, Keeper: rec}
	switch {
	case rec.Failed(p, perr):
		v.Key = teleportFailed
	case errors.Is(perr, teleport.ErrNotLoggedIn):
		v.Key = teleportExpired
	case perr != nil:
		v.Key = teleportUnreadable
	case p.Left(now) <= 0:
		v.Key = teleportExpired
	case p.Left(now) < warn:
		v.Key = teleportWarn
	}
	if perr != nil {
		v.Error = perr.Error()
	}
	return v
}

// line is the login in one line: the watch's condition line while there is
// one, else its expiry.
func (v teleportView) line(now time.Time) string {
	until := "expires " + clock(now, v.ValidUntil)
	if v.ValidUntil.IsZero() {
		until = "has no active profile"
	}
	switch v.Key {
	case teleportFailed:
		return fmt.Sprintf("TELEPORT RENEWAL FAILED at %s: %s; the login %s: %s", clock(now, v.Keeper.FailedAt), v.Keeper.Reason, until, teleportRenewHint)
	case teleportUnreadable:
		return "TELEPORT LOGIN unreadable: " + v.Error
	case teleportExpired:
		if v.ValidUntil.IsZero() {
			return "TELEPORT LOGIN none: " + v.Error + "; installation reads fail: " + teleportRenewHint
		}
		return fmt.Sprintf("TELEPORT LOGIN EXPIRED at %s: installation reads fail: %s", clock(now, v.ValidUntil), teleportRenewHint)
	case teleportWarn:
		l := fmt.Sprintf("TELEPORT LOGIN %s (%s left)", until, dur(v.ValidUntil.Sub(now)))
		if v.Keeper.Waiting != "" && now.Sub(v.Keeper.WaitingAt) < time.Hour {
			return l + ": the keeper's renewal waits (" + v.Keeper.Waiting + ")"
		}
		return l + ": " + teleportRenewHint
	}
	l := fmt.Sprintf("teleport: %s@%s valid until %s (%s left)", v.Username, v.Cluster, clock(now, v.ValidUntil), dur(v.ValidUntil.Sub(now)))
	if !v.Keeper.RenewedAt.IsZero() {
		l += ", renewed " + clock(now, v.Keeper.RenewedAt)
	}
	return l
}

// readTeleport reads the login and the keeper's record; nil without a
// configured keeper.
func (a *app) readTeleport(ctx context.Context) *teleportView {
	t := a.cfg.Teleport
	if !t.Enabled() {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	p, perr := teleport.Status(ctx, t.Tsh, t.Home)
	rec, err := a.teleportRecord()
	if err != nil && perr == nil {
		perr = err
	}
	v := newTeleportView(p, perr, rec, time.Now(), t.WarnBefore.Duration)
	return &v
}

func (a *app) teleportRecord() (teleport.Record, error) {
	var rec teleport.Record
	_, err := a.store.ReadFile(teleportFile, &rec)
	return rec, err
}

// teleportCause names an expired or missing Teleport login, for a refusal
// whose installation read failed: "" when the login is fine or not kept.
func (a *app) teleportCause(ctx context.Context) string {
	v := a.readTeleport(ctx)
	if v == nil || v.Key != teleportExpired {
		return ""
	}
	return v.line(a.now)
}

func (a *app) teleportCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "teleport",
		Short: "The Teleport login: its expiry and its keeper (exit 3 when it needs a renewal)",
		Long: `Print the Teleport login the kube contexts reach the installations through:
user, cluster and expiry from tsh status (metadata, never key material), and
what its keeper last did. Exit 3 while it needs a person: expired, under
teleport.warnBefore, or its keeper's renewal failed; that makes it the probe
of the sign-in note a failed renewal leaves.

The keeper (beekeeper install writes beekeeper-teleport.timer when
teleport.proxy is set) runs ` + "`teleport renew --keeper`" + ` every teleport.every.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			v := a.readTeleport(cmd.Context())
			if v == nil {
				return refused("no Teleport login is kept: teleport.proxy is not configured")
			}
			if a.json {
				if err := a.printJSON(v); err != nil {
					return err
				}
			} else if _, err := fmt.Fprintln(a.out, v.line(a.now)); err != nil {
				return err
			}
			if v.Key != "" {
				return &exitError{code: ExitRefused}
			}
			return nil
		},
	}
	c.AddCommand(a.teleportRenewCmd())
	return c
}

func (a *app) teleportRenewCmd() *cobra.Command {
	var keeper bool
	c := &cobra.Command{
		Use:   "renew",
		Short: "Renew the Teleport login now, holding the browser lease",
		Long: `Renew the Teleport login: tsh login into an empty staging home (a valid
profile makes tsh login print its status only), which opens the login URL in
the default browser, whose SSO session completes it; once the new profile is
valid, the staging home and the profile directory (teleport.home) swap in one
step, so the old certificate serves until the new one is in place, and the
profile before is kept in the state directory. tsh's output carries the
one-time login URL: it goes to teleport/login.log in the state directory and
is never printed. A login that does not complete within teleport.loginTimeout
leaves the profile as it was.

The renewal holds the browser lease: a session claims it first (beekeeper
lease claim browser); the keeper, which has no session, claims it itself
when it is free and granted to nobody, and waits for its next run otherwise.

--keeper is how the keeper's unit runs it: it renews only once less than
teleport.renewBefore is left, and not again after its renewal of the same
login failed; a failure leaves a sign-in note for the guide's person. A
login whose SSO callback exchange with the proxy timed out after the
browser's sign-in is retried once first, both attempts in the log.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			t := a.cfg.Teleport
			if !t.Enabled() {
				return usageErr("teleport.proxy is not configured")
			}
			me, err := a.caller()
			if err != nil {
				return err
			}
			rec, err := a.teleportRecord()
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			before, perr := teleport.Status(ctx, t.Tsh, t.Home)
			if keeper {
				if due, why := teleport.Due(before, perr, rec, a.now, t.RenewBefore.Duration); !due {
					_, err := fmt.Fprintln(a.out, "teleport: "+why)
					return err
				}
			}
			release, err := a.holdBrowser(me)
			if err != nil {
				if !keeper {
					return err
				}
				rec.Waiting, rec.WaitingAt = err.Error(), a.now.UTC()
				if werr := a.store.WriteFile(teleportFile, rec); werr != nil {
					return werr
				}
				_, err = fmt.Fprintln(a.out, "teleport: the renewal waits: "+err.Error())
				return err
			}
			defer release()
			r := teleport.Renewal{Tsh: t.Tsh, Home: t.Home, Dir: filepath.Join(a.cfg.StateDir, "teleport"),
				Proxy: t.Proxy, Auth: t.Auth, Timeout: t.LoginTimeout.Duration,
				Retry: func(first error) { a.teleportRetry(me, first) }}
			after, err := r.Renew(ctx, a.now)
			if err != nil {
				return a.teleportFailed(me, keeper, before, perr, err)
			}
			if err := a.store.WriteFile(teleportFile, teleport.Record{RenewedAt: a.now.UTC(), ValidUntil: after.ValidUntil}); err != nil {
				return err
			}
			from := "no active profile"
			if perr == nil {
				from = before.ValidUntil.Local().Format(time.RFC3339)
			}
			to := after.ValidUntil.Local().Format(time.RFC3339)
			if err := a.store.Update(func(*state.State) ([]state.Event, error) {
				return []state.Event{event(me, "teleport.renew", "valid until %s → %s", from, to)}, nil
			}); err != nil {
				return err
			}
			_, err = fmt.Fprintf(a.out, "renewed the Teleport login: valid until %s → %s\n", from, to)
			return err
		},
	}
	c.Flags().BoolVar(&keeper, "keeper", false, "as the keeper's unit: only under teleport.renewBefore, never again after a failed renewal of the same login")
	return c
}

// teleportRetry logs the first login's failure that the renewal retries
// once.
func (a *app) teleportRetry(me state.Party, first error) {
	_ = a.store.Update(func(*state.State) ([]state.Event, error) {
		return []state.Event{event(me, "teleport.retry", "%s", first)}, nil
	})
	_, _ = fmt.Fprintf(a.out, "teleport: the login failed (%s): retrying once\n", first)
}

// teleportFailed records the failed renewal of the profile before (read
// with perr) and, for the keeper, leaves a sign-in note for the guide's
// person that closes once the login is renewed.
func (a *app) teleportFailed(me state.Party, keeper bool, before teleport.Profile, perr, failure error) error {
	rec := teleport.Record{FailedAt: a.now.UTC(), Reason: failure.Error()}
	until := "has no active profile"
	if perr == nil {
		rec.FailedFor = before.ValidUntil
		until = "expires " + clock(a.now, before.ValidUntil)
	}
	if err := a.store.WriteFile(teleportFile, rec); err != nil {
		return err
	}
	probe, err := os.Executable()
	if err != nil {
		return err
	}
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		evs := []state.Event{event(me, "teleport.fail", "%s", failure)}
		if !keeper {
			return evs, nil
		}
		st.NextNote++
		n := state.Note{ID: st.NextNote, For: a.cfg.Guide.Person, By: me, At: a.now.UTC(), Due: a.now.UTC(), Kind: noteLogin,
			Until: guard.ShellQuote(probe) + " teleport",
			Text: fmt.Sprintf("Sign in to Teleport: the keeper's renewal failed (%s) and the login %s; run `beekeeper teleport renew` while holding the browser lease, completing the SSO sign-in in the browser it opens.",
				failure, until),
			Default: "the login expires and every installation read (watch, kubectl, the merge gate) fails until someone signs in"}
		st.Notes = append(st.Notes, n)
		return append(evs, event(me, "note.add", "#%d %s", n.ID, n.Text)), nil
	}); err != nil {
		return err
	}
	return failure
}

// holdBrowser makes sure the caller holds the browser lease for a renewal
// and returns what gives it back. A session must hold it already: its
// claims go through the supervisor's grants. A caller without a session
// (the keeper) claims it when it is free and granted to nobody.
func (a *app) holdBrowser(me state.Party) (func(), error) {
	dir := lease.Dir(a.cfg.LeaseDir)
	claimed := false
	err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		cur, err := dir.Get(config.Browser)
		switch {
		case err != nil:
			return nil, err
		case cur != nil && cur.Party().Is(me):
			return nil, nil
		case cur != nil:
			return nil, refused("the browser lease is held by %q (%s)", cur.Name, cur.Purpose)
		case me.Session != "":
			return nil, refused("claim the browser lease first: beekeeper lease claim browser -p %q", teleportPurpose)
		}
		if q := lease.Pending(st, config.Browser, false, a.now, a.cfg.GrantTTL.Duration); len(q) > 0 {
			return nil, refused("the browser lease is granted to %q", q[0].To.Name)
		}
		host, _ := os.Hostname()
		if cur, err := dir.Claim(config.Browser, lease.Holder{Env: config.Browser, Holder: os.Getenv("USER") + "@" + strings.SplitN(host, ".", 2)[0],
			Name: me.Name, Purpose: teleportPurpose, Since: a.now.UTC().Format("2006-01-02T15:04:05Z")}); err != nil {
			return nil, err
		} else if cur != nil {
			return nil, refused("the browser lease is held by %q (%s)", cur.Name, cur.Purpose)
		}
		claimed = true
		return []state.Event{event(me, verbLeaseClaim, "%s: %s", config.Browser, teleportPurpose)}, nil
	})
	if err != nil || !claimed {
		return func() {}, err
	}
	return func() {
		_ = a.store.Update(func(st *state.State) ([]state.Event, error) {
			if err := dir.Release(config.Browser); err != nil {
				return nil, err
			}
			if st.Released == nil {
				st.Released = map[string]time.Time{}
			}
			st.Released[config.Browser] = time.Now().UTC()
			return []state.Event{event(me, "lease.release", "%s", config.Browser)}, nil
		})
	}, nil
}
