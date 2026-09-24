package cmd

import (
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/lease"
	"github.com/giantswarm/beekeeper/internal/state"
)

// The states of a held lease.
const (
	holderLive   = "live"   // the holder's session runs
	holderGone   = "gone"   // it does not: the lease is stale
	holderPerson = "person" // held by a person or script
)

// leaseView is a held lease with whether its holder still runs.
type leaseView struct {
	lease.Holder
	State string `json:"state"`
}

func (a *app) leaseView(sessions []*claude.Session, h lease.Holder) leaseView {
	v := leaseView{Holder: h, State: holderPerson}
	if h.Session != "" || h.HostSession != "" {
		v.State = holderGone
		if s, ok := claude.Live(sessions, h.Party()); ok {
			v.State = holderLive
			if v.Name == "" {
				v.Name = s.Name
			}
		}
	}
	if v.Name == "" {
		v.Name = h.Holder
	}
	return v
}

func (a *app) leaseCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "lease",
		Short: "Hold a shared resource: a kind lab, an installation, the browser",
		Long: `Hold one of the machine's shared resources, one session at a time: the
environments listed under resources in the configuration (kind labs,
shared installations) and the browser.

While a supervisor runs, a free lease is not permission: a session claims
only what the supervisor granted it (` + "`beekeeper lease grant`" + `), in the order
the grants were given. Without a supervisor a free lease is claimed directly.
A claim that is refused exits 3: gate the action on it (claim && act), never
run the action after a failed claim.

Without a subcommand, lists the leases.`,
		Args: cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error { return a.leaseList() },
	}
	c.AddCommand(a.leaseClaimCmd(), a.leaseReleaseCmd(), a.leaseStatusCmd(), a.leaseListCmd(),
		a.leaseGrantCmd(), a.leaseRevokeCmd())
	return c
}

func (a *app) checkResource(res string) error {
	if !a.cfg.IsLeasable(res) {
		return &exitError{code: ExitUsage, msg: fmt.Sprintf("unknown resource %q (configured: %s)", res, strings.Join(a.cfg.Leasable(), ", "))}
	}
	return nil
}

// heldMap reports which resources are held right now.
func heldMap(holders []lease.Holder) map[string]bool {
	m := map[string]bool{}
	for _, h := range holders {
		m[h.Env] = true
	}
	return m
}

// liveSupervisor returns the recorded supervisor when its session runs.
func liveSupervisor(st *state.State, sessions []*claude.Session) *state.Supervisor {
	if st.Supervisor == nil {
		return nil
	}
	if _, ok := claude.Live(sessions, st.Supervisor.Party); !ok {
		return nil
	}
	return st.Supervisor
}

func (a *app) leaseClaimCmd() *cobra.Command {
	var purpose string
	c := &cobra.Command{
		Use:   "claim <resource>",
		Short: "Claim a resource (exit 3 when it is held or not granted to you)",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			res := args[0]
			if err := a.checkResource(res); err != nil {
				return err
			}
			if strings.TrimSpace(purpose) == "" {
				return &exitError{code: ExitUsage, msg: "--purpose is required: say what the resource is for"}
			}
			me, err := a.caller()
			if err != nil {
				return err
			}
			sessions, _, err := a.sessions()
			if err != nil {
				return err
			}
			dir := lease.Dir(a.cfg.LeaseDir)
			var msg string
			err = a.store.Update(func(st *state.State) ([]state.Event, error) {
				holders, err := dir.List()
				if err != nil {
					return nil, err
				}
				held := heldMap(holders)
				lease.Prune(st, held, a.now, a.cfg.GrantTTL.Duration)
				if cur, _ := dir.Get(res); cur != nil {
					if cur.Party().Is(me) {
						msg = fmt.Sprintf("%s is already yours (since %s)", res, clock(a.now, cur.SinceTime()))
						return nil, nil
					}
					return nil, a.heldBy(a.leaseView(sessions, *cur))
				}
				idx, err := lease.Check(st, lease.Gate{
					Resource:   res,
					Caller:     me,
					Supervisor: liveSupervisor(st, sessions),
					Held:       false,
					Now:        a.now,
					TTL:        a.cfg.GrantTTL.Duration,
				})
				if r := (*lease.Refusal)(nil); errors.As(err, &r) {
					return nil, refused("%s: %s", res, r.Reason)
				} else if err != nil {
					return nil, err
				}
				host, _ := os.Hostname()
				cur, err := dir.Claim(res, lease.Holder{
					Env:         res,
					Holder:      os.Getenv("USER") + "@" + strings.SplitN(host, ".", 2)[0],
					Session:     me.Session,
					HostSession: me.HostSession,
					Name:        me.Name,
					Purpose:     purpose,
					Since:       a.now.UTC().Format("2006-01-02T15:04:05Z"),
				})
				if err != nil {
					return nil, err
				}
				if cur != nil {
					return nil, a.heldBy(a.leaseView(sessions, *cur))
				}
				if idx >= 0 {
					st.Grants = slices.Delete(st.Grants, idx, idx+1)
				}
				msg = "claimed " + res
				return []state.Event{event(me, "lease.claim", "%s: %s", res, purpose)}, nil
			})
			if err != nil {
				return err
			}
			_, err = fmt.Fprintln(a.out, msg)
			return err
		},
	}
	c.Flags().StringVarP(&purpose, "purpose", "p", "", "what the resource is for (required)")
	return c
}

func (a *app) heldBy(v leaseView) error {
	note := ""
	if v.State == holderGone {
		note = " (its session no longer runs: the supervisor or the person frees it with `beekeeper lease release --force`)"
	}
	return refused("%s is held by %q since %s: %s%s", v.Env, v.Name, clock(a.now, v.SinceTime()), v.Purpose, note)
}

func (a *app) leaseReleaseCmd() *cobra.Command {
	var force bool
	c := &cobra.Command{
		Use:   "release <resource>",
		Short: "Release a resource you hold (--force frees another holder's)",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			res := args[0]
			if err := a.checkResource(res); err != nil {
				return err
			}
			me, err := a.caller()
			if err != nil {
				return err
			}
			dir := lease.Dir(a.cfg.LeaseDir)
			var msg string
			err = a.store.Update(func(st *state.State) ([]state.Event, error) {
				cur, err := dir.Get(res)
				if err != nil {
					return nil, err
				}
				if cur == nil {
					msg = res + " was free"
					return nil, nil
				}
				mine := cur.Party().Is(me)
				if !mine && !force {
					return nil, refused("%s is held by %q, not by you: only --force frees another holder's lease", res, cur.Name)
				}
				if err := dir.Release(res); err != nil {
					return nil, err
				}
				if st.Released == nil {
					st.Released = map[string]time.Time{}
				}
				st.Released[res] = a.now.UTC()
				msg = "released " + res
				if !mine {
					msg += fmt.Sprintf(" (held by %q: %s)", cur.Name, cur.Purpose)
					return []state.Event{event(me, "lease.force-release", "%s held by %s: %s", res, cur.Name, cur.Purpose)}, nil
				}
				return []state.Event{event(me, "lease.release", "%s", res)}, nil
			})
			if err != nil {
				return err
			}
			_, err = fmt.Fprintln(a.out, msg)
			return err
		},
	}
	c.Flags().BoolVar(&force, "force", false, "free a lease another session or person holds (its holder is gone)")
	return c
}

func (a *app) leaseStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status <resource>",
		Short: "Say whether a resource is free (exit 0) or held (exit 3)",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			res := args[0]
			if err := a.checkResource(res); err != nil {
				return err
			}
			cur, err := lease.Dir(a.cfg.LeaseDir).Get(res)
			if err != nil {
				return err
			}
			if cur == nil {
				_, err := fmt.Fprintln(a.out, "free")
				return err
			}
			sessions, _, err := a.sessions()
			if err != nil {
				return err
			}
			v := a.leaseView(sessions, *cur)
			if a.json {
				_ = a.printJSON(v)
			}
			return a.heldBy(v)
		},
	}
}

func (a *app) leaseListCmd() *cobra.Command {
	return listCmd("List the held leases, the free resources and the grant queues", a.leaseList)
}

type leaseList struct {
	Held   []leaseView              `json:"held"`
	Free   []string                 `json:"free"`
	Queues map[string][]state.Grant `json:"queues,omitempty"`
}

func (a *app) leases() (*leaseList, error) {
	holders, err := lease.Dir(a.cfg.LeaseDir).List()
	if err != nil {
		return nil, err
	}
	sessions, _, err := a.sessions()
	if err != nil {
		return nil, err
	}
	st, err := a.store.Read()
	if err != nil {
		return nil, err
	}
	held := heldMap(holders)
	l := &leaseList{Queues: map[string][]state.Grant{}}
	for _, h := range holders {
		l.Held = append(l.Held, a.leaseView(sessions, h))
	}
	for _, r := range a.cfg.Leasable() {
		if !held[r] {
			l.Free = append(l.Free, r)
		}
		if q := lease.Pending(st, r, held[r], a.now, a.cfg.GrantTTL.Duration); len(q) > 0 {
			l.Queues[r] = q
		}
	}
	return l, nil
}

func (a *app) leaseList() error {
	l, err := a.leases()
	if err != nil {
		return err
	}
	if a.json {
		return a.printJSON(l)
	}
	a.printLeases(l)
	return nil
}

func (a *app) printLeases(l *leaseList) {
	if len(l.Held) == 0 {
		_, _ = fmt.Fprintln(a.out, "no resource is held")
	} else {
		w := a.table()
		_, _ = fmt.Fprintln(w, "RESOURCE\tHOLDER\tSINCE\tSTATE\tPURPOSE")
		for _, h := range l.Held {
			_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", h.Env, truncate(h.Name, 40), clock(a.now, h.SinceTime()), h.State, truncate(h.Purpose, 60))
		}
		_ = w.Flush()
	}
	if len(l.Free) > 0 {
		_, _ = fmt.Fprintln(a.out, "free:", strings.Join(l.Free, ", "))
	}
	for _, r := range a.cfg.Leasable() {
		q := l.Queues[r]
		if len(q) == 0 {
			continue
		}
		names := make([]string, len(q))
		for i, g := range q {
			names[i] = fmt.Sprintf("%q (granted %s)", g.To.Name, clock(a.now, g.At))
		}
		_, _ = fmt.Fprintf(a.out, "granted %s: %s\n", r, strings.Join(names, ", then "))
	}
}

func (a *app) leaseGrantCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "grant <resource> <session>",
		Short: "Grant a resource to a session: its `yours <resource>`, queued behind earlier grants",
		Long: `Record the supervisor's grant of a resource to a session: the session may
claim it once it is free and every earlier grant for it is claimed or
expired. A grant waits while the resource is held; once it is free the grant
expires after grantTTL (default 30m). The session is a name, a unique part of
one, a session id or a PID.`,
		Args: cobra.ExactArgs(2),
		RunE: func(_ *cobra.Command, args []string) error {
			res := args[0]
			if err := a.checkResource(res); err != nil {
				return err
			}
			me, err := a.caller()
			if err != nil {
				return err
			}
			sessions, _, err := a.sessions()
			if err != nil {
				return err
			}
			to, err := claude.Resolve(sessions, args[1])
			if err != nil {
				return err
			}
			dir := lease.Dir(a.cfg.LeaseDir)
			var msg string
			err = a.store.Update(func(st *state.State) ([]state.Event, error) {
				holders, err := dir.List()
				if err != nil {
					return nil, err
				}
				held := heldMap(holders)
				lease.Prune(st, held, a.now, a.cfg.GrantTTL.Duration)
				q := lease.Pending(st, res, held[res], a.now, a.cfg.GrantTTL.Duration)
				if i := slices.IndexFunc(q, func(g state.Grant) bool { return g.To.Is(to.Party()) }); i >= 0 {
					msg = fmt.Sprintf("%s is already granted to %q (number %d in its queue)", res, to.Name, i+1)
					return nil, nil
				}
				st.Grants = append(st.Grants, state.Grant{Resource: res, To: to.Party(), By: me, At: a.now.UTC()})
				switch {
				case held[res]:
					cur, _ := dir.Get(res)
					msg = fmt.Sprintf("granted %s to %q, number %d in its queue: %q holds it (%s)", res, to.Name, len(q)+1, cur.Name, cur.Purpose)
				case len(q) > 0:
					msg = fmt.Sprintf("granted %s to %q, number %d in its queue after %q", res, to.Name, len(q)+1, q[len(q)-1].To.Name)
				default:
					msg = fmt.Sprintf("granted %s to %q: it claims with `beekeeper lease claim %s --purpose ...` within %s", res, to.Name, res, dur(a.cfg.GrantTTL.Duration))
				}
				return []state.Event{event(me, "lease.grant", "%s to %s", res, to.Name)}, nil
			})
			if err != nil {
				return err
			}
			_, err = fmt.Fprintln(a.out, msg)
			return err
		},
	}
}

func (a *app) leaseRevokeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "revoke <resource> <session>",
		Short: "Withdraw a grant not claimed yet",
		Args:  cobra.ExactArgs(2),
		RunE: func(_ *cobra.Command, args []string) error {
			res, who := args[0], strings.ToLower(args[1])
			if err := a.checkResource(res); err != nil {
				return err
			}
			me, err := a.caller()
			if err != nil {
				return err
			}
			removed := 0
			err = a.store.Update(func(st *state.State) ([]state.Event, error) {
				st.Grants = slices.DeleteFunc(st.Grants, func(g state.Grant) bool {
					hit := g.Resource == res && (strings.Contains(strings.ToLower(g.To.Name), who) || g.To.Session == args[1])
					if hit {
						removed++
					}
					return hit
				})
				if removed == 0 {
					return nil, nil
				}
				return []state.Event{event(me, "lease.revoke", "%s from %s", res, args[1])}, nil
			})
			if err != nil {
				return err
			}
			if removed == 0 {
				return refused("no grant of %s matches %q", res, args[1])
			}
			_, err = fmt.Fprintf(a.out, "revoked %d grant(s) of %s\n", removed, res)
			return err
		},
	}
}
