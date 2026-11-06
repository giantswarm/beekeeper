package cmd

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/execution"
	"github.com/giantswarm/beekeeper/internal/guard"
	"github.com/giantswarm/beekeeper/internal/omp"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/transcripts"
)

const (
	transcriptListCommand   = "list"
	transcriptStatusEnded   = "ended"
	transcriptStatusRunning = "running"
)

const (
	transcriptsListOp    = "transcripts.list"
	transcriptsSearchOp  = "transcripts.search"
	transcriptsReadOp    = "transcripts.read"
	transcriptsRequestOp = "transcripts.request"

	defaultTranscriptGrantTTL = 24 * time.Hour
)

type remoteTranscriptListReq struct {
	Repo    string `json:"repo,omitempty"`
	Harness string `json:"harness,omitempty"`
	Host    string `json:"host,omitempty"`
}

type remoteTranscriptSearchReq struct {
	Query   string `json:"query"`
	Repo    string `json:"repo,omitempty"`
	Harness string `json:"harness,omitempty"`
	Host    string `json:"host,omitempty"`
	Ref     string `json:"ref,omitempty"`
	Session string `json:"session,omitempty"`
	Since   string `json:"since,omitempty"`
	Tools   bool   `json:"tools,omitempty"`
	Results bool   `json:"results,omitempty"`
	Limit   int    `json:"limit,omitempty"`
}

type remoteTranscriptReadReq struct {
	Target  string `json:"target"`
	Last    int    `json:"last,omitempty"`
	Tools   bool   `json:"tools,omitempty"`
	Results bool   `json:"results,omitempty"`
}

type remoteTranscriptReadResp struct {
	Meta  transcripts.Meta   `json:"meta"`
	Turns []transcripts.Turn `json:"turns"`
	Loss  transcripts.Loss   `json:"loss,omitzero"`
}

// transcriptGrantReach says what a grant reaches, which a reader cannot tell
// from a refused read.
const transcriptGrantReach = "it covers the stored transcript (transcripts read, transcripts search), not the files that session wrote: " +
	"a file under a harness store needs hooks.transcriptGrants, and a file elsewhere follows its own rules"

type remoteTranscriptAccessReq struct {
	Repo    string `json:"repo,omitempty"`
	Session string `json:"session,omitempty"`
	All     bool   `json:"all,omitempty"`
	Reason  string `json:"reason"`
}

func (a *app) transcriptStore() transcripts.Store {
	ix, err := guard.LoadIndex(a.scanDir())
	if err != nil {
		ix = &guard.Index{}
	}
	return transcripts.Store{
		Root: filepath.Join(a.cfg.StateDir, "transcripts"),
		Redact: func(s string) string {
			out, _ := ix.Redact(s)
			return out
		},
	}
}

func (a *app) transcriptsCmd() *cobra.Command {
	var repo, harness, host string
	c := &cobra.Command{
		Use:   "transcripts",
		Short: "Store, search, read and gate session transcripts across harnesses and hosts",
		Long: `Store, search and read session transcripts across harnesses (claude, agy,
codex, omp) and execution hosts (local coordinator and SSH execution hosts).

Every transcript in the coordinator store (<stateDir>/transcripts) keeps its
session name, roster name, role, repository, issue or pull request references,
harness, host and timestamps, surviving the session ending or leaving the
roster. Secret values from the scan index and credential tokens are masked
before storage. Dialog answers (such as AskUserQuestion) are preserved as
searchable user turns.

Access rules:
  - The guide (and the operator outside a worker session) reads and searches
    all transcripts.
  - A worker reads and searches by default its own transcript and the
    transcripts of workers on the same repository.
  - When a task needs another session's or another repository's transcript, the
    worker requests access with 'beekeeper transcripts request' and the guide
    grants an expiring lease with 'beekeeper transcripts grant'.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.runTranscriptsList(cmd.Context(), repo, harness, host)
		},
	}
	c.Flags().StringVar(&repo, "repo", "", "filter by repository (owner/repo)")
	c.Flags().StringVar(&harness, "harness", "", "filter by harness (claude, agy, codex, omp)")
	c.Flags().StringVar(&host, "host", "", "filter by host")

	list := &cobra.Command{
		Use:   transcriptListCommand,
		Short: "List stored transcripts accessible to the caller",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.runTranscriptsList(cmd.Context(), repo, harness, host)
		},
	}
	list.Flags().StringVar(&repo, "repo", "", "filter by repository (owner/repo)")
	list.Flags().StringVar(&harness, "harness", "", "filter by harness (claude, agy, codex, omp)")
	list.Flags().StringVar(&host, "host", "", "filter by host")

	c.AddCommand(
		list,
		a.transcriptsSearchCmd(),
		a.transcriptsReadCmd(),
		a.transcriptsRequestCmd(),
		a.transcriptsGrantCmd(),
		a.transcriptsRevokeCmd(),
		a.transcriptsRequestsCmd(),
		a.transcriptsSyncCmd(),
	)
	return c
}

func (a *app) transcriptsSearchCmd() *cobra.Command {
	var req remoteTranscriptSearchReq
	c := &cobra.Command{
		Use:   "search <query>",
		Short: "Search accessible transcripts across running and ended sessions",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			req.Query = strings.TrimSpace(strings.Join(args, " "))
			if req.Query == "" {
				return usageErr("search query is required")
			}
			var hits []transcripts.SearchHit
			if a.managedExecutionID() != "" {
				if err := a.callCoordinatorTranscripts(cmd.Context(), transcriptsSearchOp, req, &hits); err != nil {
					return err
				}
			} else {
				var err error
				hits, err = a.searchTranscriptsLocal(cmd.Context(), req)
				if err != nil {
					return err
				}
			}
			if a.json {
				return a.printJSON(hits)
			}
			if len(hits) == 0 {
				_, err := fmt.Fprintln(a.out, "no matching transcript turns")
				return err
			}
			for _, h := range hits {
				refText := h.Meta.Ref
				if refText == "" {
					refText = h.Meta.Repo
				}
				header := fmt.Sprintf("%s [%s/%s", h.Meta.DisplayName(), h.Meta.Harness, h.Meta.Host)
				if refText != "" {
					header += " " + refText
				}
				header += fmt.Sprintf("] turn #%d %s %s", h.TurnIndex, clock(a.now, h.Turn.At), h.Turn.Role)
				_, _ = fmt.Fprintf(a.out, "--- %s\n%s\n", header, h.Snippet)
			}
			return nil
		},
	}
	c.Flags().StringVar(&req.Repo, "repo", "", "filter by repository (owner/repo)")
	c.Flags().StringVar(&req.Harness, "harness", "", "filter by harness (claude, agy, codex, omp)")
	c.Flags().StringVar(&req.Host, "host", "", "filter by host")
	c.Flags().StringVar(&req.Ref, "ref", "", "filter by issue or pull request (owner/repo#n or URL)")
	c.Flags().StringVar(&req.Session, "session", "", "filter by session name or id")
	c.Flags().StringVar(&req.Since, "since", "", "only turns since duration or timestamp (e.g. 24h)")
	c.Flags().BoolVar(&req.Tools, "tools", false, "search tool calls as well as messages")
	c.Flags().BoolVar(&req.Results, "results", false, "search tool results as well as messages and tool calls")
	c.Flags().IntVarP(&req.Limit, "limit", "n", 30, "maximum number of matching turns")
	return c
}

func (a *app) transcriptsReadCmd() *cobra.Command {
	var req remoteTranscriptReadReq
	c := &cobra.Command{
		Use:   "read <session-or-ref>",
		Short: "Read a session's stored transcript by name, id, execution id or issue/PR ref",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			req.Target = strings.TrimSpace(args[0])
			var resp remoteTranscriptReadResp
			if a.managedExecutionID() != "" {
				if err := a.callCoordinatorTranscripts(cmd.Context(), transcriptsReadOp, req, &resp); err != nil {
					return err
				}
			} else {
				var err error
				resp, err = a.readTranscriptLocal(cmd.Context(), req)
				if err != nil {
					return err
				}
			}
			if a.json {
				return a.printJSON(resp)
			}
			m := resp.Meta
			status := transcriptStatusEnded
			if m.Running {
				status = transcriptStatusRunning
			}
			_, _ = fmt.Fprintf(a.out, "# %s (%s, %s on %s, %s)\n", m.DisplayName(), cmp.Or(m.ID, m.Execution, m.Key), m.Harness, m.Host, status)
			if m.Repo != "" || m.Ref != "" {
				_, _ = fmt.Fprintf(a.out, "# repo: %s  ref: %s\n", cmp.Or(m.Repo, "-"), cmp.Or(m.Ref, "-"))
			}
			if lost := resp.Loss.String(); lost != "" {
				_, _ = fmt.Fprintf(a.out, "# lost for good: %s\n", lost)
			}
			for _, t := range resp.Turns {
				_, _ = fmt.Fprintf(a.out, "--- %s %s\n%s\n", clock(a.now, t.At), t.Role, t.Text)
				if req.Results && t.Result != "" {
					_, _ = fmt.Fprintf(a.out, "[result]\n%s\n", t.Result)
				}
			}
			return nil
		},
	}
	c.Flags().IntVarP(&req.Last, "turns", "n", 20, "number of turns to read (0 for all)")
	c.Flags().BoolVar(&req.Tools, "tools", false, "include tool call turns")
	c.Flags().BoolVar(&req.Results, "results", false, "include tool call turns and their redacted results")
	return c
}

func (a *app) transcriptsRequestCmd() *cobra.Command {
	var req remoteTranscriptAccessReq
	c := &cobra.Command{
		Use:   "request [--repo <owner/repo> | --session <session> | --all] --reason <why>",
		Short: "Request wider transcript access from the guide",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			req.Repo = strings.TrimSpace(req.Repo)
			req.Session = strings.TrimSpace(req.Session)
			req.Reason = strings.TrimSpace(req.Reason)
			if req.Reason == "" {
				return usageErr("--reason is required when requesting wider transcript access")
			}
			if req.Repo == "" && req.Session == "" && !req.All {
				return usageErr("specify --repo <owner/repo>, --session <session> or --all")
			}
			if a.managedExecutionID() != "" {
				return a.remoteControl(cmd.Context(), transcriptsRequestOp, req)
			}
			me, err := a.caller()
			if err != nil {
				return err
			}
			var created state.TranscriptRequest
			err = a.store.Update(func(st *state.State) ([]state.Event, error) {
				var evs []state.Event
				created, evs, err = a.recordTranscriptRequest(st, me, req)
				return evs, err
			})
			if err != nil {
				return err
			}
			if a.json {
				return a.printJSON(created)
			}
			_, err = fmt.Fprintf(a.out, "transcript access request #%d recorded for the guide (%s)\n", created.ID, transcriptScopeText(created.Repo, created.Session, created.All))
			return err
		},
	}
	c.Flags().StringVar(&req.Repo, "repo", "", "repository to request access to (owner/repo)")
	c.Flags().StringVar(&req.Session, "session", "", "session name, id or ref to request access to")
	c.Flags().BoolVar(&req.All, "all", false, "request access to all transcripts")
	c.Flags().StringVar(&req.Reason, "reason", "", "why the task needs wider transcript access")
	return c
}

func (a *app) transcriptsGrantCmd() *cobra.Command {
	var repo, session, reason string
	var all bool
	var ttl time.Duration
	c := &cobra.Command{
		Use:   "grant <worker> [--repo <owner/repo> | --session <session> | --all] [--ttl <duration>] [--reason <why>]",
		Short: "Grant a worker expiring access to another session's or repository's transcripts",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			worker := strings.TrimSpace(args[0])
			repo = strings.TrimSpace(repo)
			session = strings.TrimSpace(session)
			reason = strings.TrimSpace(reason)
			if ttl <= 0 {
				return usageErr("--ttl must be positive")
			}
			var granted state.TranscriptGrant
			err := a.store.Update(func(st *state.State) ([]state.Event, error) {
				caller, me := a.resolveTranscriptCaller(st)
				if !caller.IsGuide {
					return nil, refused("only the guide may grant wider transcript access; workers request access with `beekeeper transcripts request`")
				}
				to := state.Party{Name: worker}
				if idx, findErr := findAgent(st, worker); findErr == nil {
					to = st.Agents[idx].Party
					to.Name = st.Agents[idx].Name
				}
				// If no explicit scope flags were given, fulfill the worker's latest open request.
				if repo == "" && session == "" && !all {
					for i := len(st.TranscriptRequests) - 1; i >= 0; i-- {
						r := st.TranscriptRequests[i]
						if strings.EqualFold(r.By.Name, worker) || strings.EqualFold(r.By.Name, to.Name) || r.By.Is(to) {
							repo, session, all = r.Repo, r.Session, r.All
							if reason == "" {
								reason = r.Reason
							}
							break
						}
					}
				}
				if repo == "" && session == "" && !all {
					return nil, usageErr("specify --repo <owner/repo>, --session <session> or --all (or have an open request from %q)", worker)
				}
				st.NextTranscriptGrant++
				granted = state.TranscriptGrant{
					ID:      st.NextTranscriptGrant,
					To:      to,
					Repo:    repo,
					Session: session,
					All:     all,
					Reason:  reason,
					By:      me,
					At:      a.now.UTC(),
					Expires: a.now.Add(ttl).UTC(),
				}
				st.TranscriptGrants = slices.DeleteFunc(st.TranscriptGrants, func(g state.TranscriptGrant) bool { return !g.Active(a.now) })
				st.TranscriptGrants = append(st.TranscriptGrants, granted)
				st.TranscriptRequests = slices.DeleteFunc(st.TranscriptRequests, func(r state.TranscriptRequest) bool {
					if !strings.EqualFold(r.By.Name, worker) && !strings.EqualFold(r.By.Name, to.Name) && !r.By.Is(to) {
						return false
					}
					return all || (repo != "" && strings.EqualFold(r.Repo, repo)) || (session != "" && strings.EqualFold(r.Session, session))
				})
				scope := transcriptScopeText(granted.Repo, granted.Session, granted.All)
				return []state.Event{event(me, "transcripts.grant", "to %s (%s) until %s", to.Name, scope, granted.Expires.Format(time.RFC3339))}, nil
			})
			if err != nil {
				return err
			}
			if a.json {
				return a.printJSON(granted)
			}
			_, err = fmt.Fprintf(a.out, "granted transcript access #%d to %q (%s) until %s: %s\n",
				granted.ID, granted.To.Name, transcriptScopeText(granted.Repo, granted.Session, granted.All), clockZone(a.now, granted.Expires), transcriptGrantReach)
			if err != nil {
				return err
			}
			if metas, err := a.transcriptStore().List(); err == nil && !slices.ContainsFunc(metas, func(m transcripts.Meta) bool { return transcripts.Covers(granted, m) }) {
				_, err = fmt.Fprintf(a.out, "no stored transcript matches it yet: `beekeeper transcripts list` names what the store holds\n")
				return err
			}
			return nil
		},
	}
	c.Flags().StringVar(&repo, "repo", "", "repository to grant access to (owner/repo)")
	c.Flags().StringVar(&session, "session", "", "session name, id or ref to grant access to")
	c.Flags().BoolVar(&all, "all", false, "grant access to all transcripts")
	c.Flags().DurationVar(&ttl, "ttl", defaultTranscriptGrantTTL, "how long the grant lasts")
	c.Flags().StringVar(&reason, "reason", "", "reason for the grant")
	return c
}

func (a *app) transcriptsRevokeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "revoke <worker>",
		Short: "Revoke a worker's active transcript access grants",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			worker := strings.TrimSpace(args[0])
			var removed int
			err := a.store.Update(func(st *state.State) ([]state.Event, error) {
				caller, me := a.resolveTranscriptCaller(st)
				if !caller.IsGuide {
					return nil, refused("only the guide may revoke transcript access grants")
				}
				before := len(st.TranscriptGrants)
				st.TranscriptGrants = slices.DeleteFunc(st.TranscriptGrants, func(g state.TranscriptGrant) bool {
					return strings.EqualFold(g.To.Name, worker) || strings.EqualFold(g.To.Session, worker)
				})
				removed = before - len(st.TranscriptGrants)
				if removed == 0 {
					return nil, nil
				}
				return []state.Event{event(me, "transcripts.revoke", "revoked %d grant(s) of %s", removed, worker)}, nil
			})
			if err != nil {
				return err
			}
			_, err = fmt.Fprintf(a.out, "revoked %d transcript grant(s) for %q\n", removed, worker)
			return err
		},
	}
}

func (a *app) transcriptsRequestsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "requests",
		Short: "List open worker transcript access requests and active grants",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			st, err := a.store.Read()
			if err != nil {
				return err
			}
			var active []state.TranscriptGrant
			for _, g := range st.TranscriptGrants {
				if g.Active(a.now) {
					active = append(active, g)
				}
			}
			if a.json {
				return a.printJSON(struct {
					Requests []state.TranscriptRequest `json:"requests"`
					Grants   []state.TranscriptGrant   `json:"grants"`
				}{st.TranscriptRequests, active})
			}
			if len(st.TranscriptRequests) == 0 && len(active) == 0 {
				_, err = fmt.Fprintln(a.out, "no open transcript requests or active grants")
				return err
			}
			if len(st.TranscriptRequests) > 0 {
				_, _ = fmt.Fprintln(a.out, "Open transcript requests:")
				for _, r := range st.TranscriptRequests {
					_, _ = fmt.Fprintf(a.out, "  #%d by %s (%s ago): %s — %s\n",
						r.ID, r.By.Name, ago(a.now, r.At), transcriptScopeText(r.Repo, r.Session, r.All), r.Reason)
				}
			}
			if len(active) > 0 {
				_, _ = fmt.Fprintln(a.out, "Active transcript grants:")
				for _, g := range active {
					_, _ = fmt.Fprintf(a.out, "  #%d to %s: %s (expires in %s)\n",
						g.ID, g.To.Name, transcriptScopeText(g.Repo, g.Session, g.All), dur(g.Expires.Sub(a.now)))
				}
			}
			return nil
		},
	}
}

func (a *app) transcriptsSyncCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "sync",
		Short: "Collect and index local and remote session transcripts into the store",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			st, err := a.store.Read()
			if err != nil {
				return err
			}
			syncErr := a.syncAllTranscripts(cmd.Context(), st, true)
			metas, err := a.transcriptStore().List()
			if err != nil {
				return errors.Join(syncErr, err)
			}
			if a.json {
				return errors.Join(a.printJSON(metas), syncErr)
			}
			_, err = fmt.Fprintf(a.out, "synced %d transcript(s) in %s\n", len(metas), a.transcriptStore().Root)
			return errors.Join(err, syncErr)
		},
	}
}

func (a *app) runTranscriptsList(ctx context.Context, repo, harness, host string) error {
	req := remoteTranscriptListReq{Repo: repo, Harness: harness, Host: host}
	var metas []transcripts.Meta
	if a.managedExecutionID() != "" {
		if err := a.callCoordinatorTranscripts(ctx, transcriptsListOp, req, &metas); err != nil {
			return err
		}
	} else {
		var err error
		metas, err = a.listTranscriptsLocal(ctx, req)
		if err != nil {
			return err
		}
	}
	if a.json {
		return a.printJSON(metas)
	}
	if len(metas) == 0 {
		_, err := fmt.Fprintln(a.out, "no accessible transcripts")
		return err
	}
	w := a.table()
	_, _ = fmt.Fprintln(w, "SESSION\tROSTER\tROLE\tHARNESS\tHOST\tREPO / REF\tTURNS\tACTIVE\tSTATUS")
	for _, m := range metas {
		status := transcriptStatusEnded
		if m.Running {
			status = transcriptStatusRunning
		}
		repoRef := cmp.Or(m.Ref, m.Repo, "-")
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%d\t%s\t%s\n",
			truncate(m.DisplayName(), 36),
			cmp.Or(m.RosterName, "-"),
			cmp.Or(m.Role, "-"),
			m.Harness,
			m.Host,
			truncate(repoRef, 38),
			m.Turns,
			ago(a.now, m.LastActive),
			status,
		)
	}
	return w.Flush()
}

func (a *app) callCoordinatorTranscripts(ctx context.Context, op string, reqPayload any, out any) error {
	s, j, err := a.executionCaller()
	if err != nil {
		return err
	}
	co := s.Config.Coordinators[j.Request.Coordinator]
	return execution.CoordinatorCall(ctx, co.Callback, string(j.Handle.Ref.Host), op, j.Handle.Ref, reqPayload, j.Request.ControlKey, out, nil)
}

func (a *app) listTranscriptsLocal(ctx context.Context, req remoteTranscriptListReq) ([]transcripts.Meta, error) {
	st, err := a.store.Read()
	if err != nil {
		return nil, err
	}
	_ = a.syncAllTranscripts(ctx, st, false) // queries read the stored transcripts; transcripts sync reports a refresh failure
	caller, _ := a.resolveTranscriptCaller(st)
	return a.listTranscriptsForCaller(st, caller, req)
}

func (a *app) listTranscriptsForCaller(st *state.State, caller transcripts.Caller, req remoteTranscriptListReq) ([]transcripts.Meta, error) {
	all, err := a.transcriptStore().List()
	if err != nil {
		return nil, err
	}
	accessible := transcripts.Filter(all, caller, st.TranscriptGrants, a.now)
	var out []transcripts.Meta
	for _, m := range accessible {
		if req.Repo != "" && !strings.EqualFold(m.Repo, req.Repo) && !strings.EqualFold(transcripts.RepoOfRef(m.Ref), req.Repo) {
			continue
		}
		if req.Harness != "" && !strings.EqualFold(m.Harness, req.Harness) {
			continue
		}
		if req.Host != "" && !strings.EqualFold(m.Host, req.Host) {
			continue
		}
		out = append(out, m)
	}
	return out, nil
}

func (a *app) searchTranscriptsLocal(ctx context.Context, req remoteTranscriptSearchReq) ([]transcripts.SearchHit, error) {
	st, err := a.store.Read()
	if err != nil {
		return nil, err
	}
	_ = a.syncAllTranscripts(ctx, st, false) // queries read the stored transcripts; transcripts sync reports a refresh failure
	caller, _ := a.resolveTranscriptCaller(st)
	return a.searchTranscriptsForCaller(st, caller, req)
}

func (a *app) searchTranscriptsForCaller(st *state.State, caller transcripts.Caller, req remoteTranscriptSearchReq) ([]transcripts.SearchHit, error) {
	all, err := a.transcriptStore().List()
	if err != nil {
		return nil, err
	}
	if req.Repo != "" && !caller.IsGuide {
		probe := transcripts.Meta{Repo: req.Repo, Role: "worker"}
		if !transcripts.Allowed(caller, probe, st.TranscriptGrants, a.now) {
			return nil, refused("transcripts of %s are outside your repository (%s); request access with `beekeeper transcripts request --repo %s --reason <why>`", req.Repo, cmp.Or(caller.Repo, "none"), req.Repo)
		}
	}
	accessible := transcripts.Filter(all, caller, st.TranscriptGrants, a.now)
	var since time.Time
	if req.Since != "" {
		if d, err := time.ParseDuration(req.Since); err == nil {
			since = a.now.Add(-d)
		} else if t, err := time.Parse(time.RFC3339, req.Since); err == nil {
			since = t
		} else {
			return nil, usageErr("invalid --since %q: use a duration (24h) or RFC3339 timestamp", req.Since)
		}
	}
	return a.transcriptStore().Search(accessible, req.Query, transcripts.SearchOptions{
		Repo:    req.Repo,
		Harness: req.Harness,
		Host:    req.Host,
		Ref:     req.Ref,
		Session: req.Session,
		Since:   since,
		Tools:   req.Tools,
		Results: req.Results,
		Limit:   req.Limit,
	})
}

func (a *app) readTranscriptLocal(ctx context.Context, req remoteTranscriptReadReq) (remoteTranscriptReadResp, error) {
	st, err := a.store.Read()
	if err != nil {
		return remoteTranscriptReadResp{}, err
	}
	_ = a.syncAllTranscripts(ctx, st, false) // queries read the stored transcripts; transcripts sync reports a refresh failure
	caller, _ := a.resolveTranscriptCaller(st)
	return a.readTranscriptForCaller(ctx, st, caller, req)
}

func (a *app) readTranscriptForCaller(ctx context.Context, st *state.State, caller transcripts.Caller, req remoteTranscriptReadReq) (remoteTranscriptReadResp, error) {
	store := a.transcriptStore()
	all, err := store.List()
	if err != nil {
		return remoteTranscriptReadResp{}, err
	}
	accessible := transcripts.Filter(all, caller, st.TranscriptGrants, a.now)
	m, resolveErr := transcripts.Resolve(accessible, req.Target)
	if resolveErr != nil {
		// Check whether the target exists in all transcripts but was filtered out by access rules.
		if blocked, err := transcripts.Resolve(all, req.Target); err == nil {
			scopeFlag := fmt.Sprintf("--session %s", blocked.DisplayName())
			if blocked.Repo != "" && blocked.Role != guideRole.name {
				scopeFlag = fmt.Sprintf("--repo %s", blocked.Repo)
			}
			return remoteTranscriptReadResp{}, refused("transcript %q (%s) is outside your default repository access (%s); request access with `beekeeper transcripts request %s --reason <why>`",
				blocked.DisplayName(), cmp.Or(blocked.Repo, blocked.Role, blocked.Host), cmp.Or(caller.Repo, "none"), scopeFlag)
		}
		return remoteTranscriptReadResp{}, resolveErr
	}
	// If reading a remote execution transcript, try a quick incremental pull first.
	if m.Execution != "" {
		for _, s := range st.Starts {
			if s.Execution != nil && string(s.Execution.Handle.Ref.Execution) == m.Execution {
				// A failed pull leaves the stored transcript to read; the
				// host reconciliation keeps collecting a pending one.
				_ = a.collectExecutionTranscript(ctx, st, s, s.Execution)
				if updated, ok, _ := store.ReadMeta(m.Key); ok {
					m = updated
				}
				break
			}
		}
	}
	turns, err := store.ReadTurns(m.Key, transcripts.ReadOptions{
		Last:    req.Last,
		Tools:   req.Tools,
		Results: req.Results,
	})
	if err != nil {
		return remoteTranscriptReadResp{}, err
	}
	return remoteTranscriptReadResp{Meta: m, Turns: turns, Loss: transcripts.LossOf(turns, req.Results)}, nil
}

func (a *app) recordTranscriptRequest(st *state.State, me state.Party, req remoteTranscriptAccessReq) (state.TranscriptRequest, []state.Event, error) {
	if i := slices.IndexFunc(st.Agents, func(ag state.Agent) bool { return ag.Is(me) }); i >= 0 {
		me.Name = st.Agents[i].Name
	}
	st.NextTranscriptGrant++
	r := state.TranscriptRequest{
		ID:      st.NextTranscriptGrant,
		By:      me,
		Repo:    req.Repo,
		Session: req.Session,
		All:     req.All,
		Reason:  req.Reason,
		At:      a.now.UTC(),
	}
	st.TranscriptRequests = append(st.TranscriptRequests, r)
	scope := transcriptScopeText(r.Repo, r.Session, r.All)
	return r, []state.Event{event(me, transcriptsRequestOp, "#%d by %s (%s): %s", r.ID, me.Name, scope, r.Reason)}, nil
}

func transcriptScopeText(repo, session string, all bool) string {
	switch {
	case all:
		return "all transcripts"
	case repo != "" && session != "":
		return fmt.Sprintf("repo %s, session %s", repo, session)
	case repo != "":
		return "repo " + repo
	case session != "":
		return "session " + session
	default:
		return "unspecified"
	}
}

// resolveTranscriptCaller determines the caller's transcript access identity.
func (a *app) resolveTranscriptCaller(st *state.State) (transcripts.Caller, state.Party) {
	envP := envParty(os.Getenv)
	if a.as != "" && envP.Session == "" && envP.HostSession == "" {
		me := state.Party{Name: a.as, Person: a.cfg.Identity.Person, Team: a.cfg.Identity.Team, Host: a.cfg.Identity.Host}
		if strings.EqualFold(a.as, guideRole.name) || (a.cfg.Identity.Person != "" && strings.EqualFold(a.as, a.cfg.Identity.Person)) {
			return transcripts.Caller{Party: me, RosterName: a.as, Role: guideRole.name, IsGuide: true}, me
		}
		if idx, err := findAgent(st, a.as); err == nil {
			ag := st.Agents[idx]
			if h := st.GuideRole().Holder; h != nil && h.Is(ag.Party) {
				return transcripts.Caller{Party: ag.Party, RosterName: ag.Name, Role: guideRole.name, IsGuide: true}, ag.Party
			}
			return a.workerCallerForParty(st, ag.Party, ag.Name), ag.Party
		}
		for _, s := range st.Starts {
			if strings.EqualFold(s.Name, a.as) {
				return a.workerCallerForParty(st, s.Party, s.Name), s.Party
			}
		}
		return transcripts.Caller{Party: me, RosterName: a.as, Role: guideRole.name, IsGuide: true}, me
	}
	if envP.Session != "" || envP.HostSession != "" {
		me, _ := a.caller()
		if h := st.GuideRole().Holder; h != nil && h.Is(envP) {
			return transcripts.Caller{Party: me, RosterName: cmp.Or(h.Name, guideRole.name), Role: guideRole.name, IsGuide: true}, me
		}
		// An unregistered or retired harness session is still a worker.
		// Only the current guide holder above has unrestricted access.
		return a.workerCallerForParty(st, envP, me.Name), me
	}
	me := state.Party{Name: cmp.Or(a.cfg.Identity.Person, "operator"), Person: a.cfg.Identity.Person, Team: a.cfg.Identity.Team, Host: a.cfg.Identity.Host}
	return transcripts.Caller{Party: me, RosterName: me.Name, Role: guideRole.name, IsGuide: true}, me
}

func (a *app) workerCallerForParty(st *state.State, p state.Party, fallbackName string) transcripts.Caller {
	c := transcripts.Caller{
		Party:      p,
		RosterName: fallbackName,
		Role:       workerRole,
	}
	for _, ag := range st.Agents {
		if ag.Is(p) || (fallbackName != "" && strings.EqualFold(ag.Name, fallbackName)) {
			c.RosterName = ag.Name
			if ag.Execution != nil {
				c.Execution = string(ag.Execution.Execution)
			}
			for _, ref := range extractAgentRefs(ag) {
				if repo := transcripts.RepoOfRef(ref); repo != "" && c.Repo == "" {
					c.Repo = repo
				}
			}
			break
		}
	}
	for _, s := range st.Starts {
		if s.Is(p) || (c.RosterName != "" && strings.EqualFold(s.Name, c.RosterName)) {
			if c.RosterName == "" {
				c.RosterName = s.Name
			}
			if s.Execution != nil {
				c.Execution = string(s.Execution.Handle.Ref.Execution)
				if c.Repo == "" && s.Execution.Request.Repository != "" {
					c.Repo = s.Execution.Request.Repository
				}
			}
		}
	}
	for _, r := range st.Records {
		if r.Session.Is(p) || (c.RosterName != "" && strings.EqualFold(r.Session.Name, c.RosterName)) {
			if repo := transcripts.RepoOfRef(r.Issue); repo != "" && c.Repo == "" {
				c.Repo = repo
			}
		}
	}
	if c.Repo == "" {
		for _, dr := range st.DeskReviews {
			if dr.Reviewer.Is(p) || (c.RosterName != "" && strings.EqualFold(dr.Reviewer.Name, c.RosterName)) {
				if repo := transcripts.RepoOfRef(dr.Ref); repo != "" {
					c.Repo = repo
					break
				}
			}
		}
	}
	if c.Repo == "" {
		if wd, err := os.Getwd(); err == nil {
			repo, _ := claude.GitInfo(wd)
			c.Repo = repo
		}
	}
	return c
}

func extractAgentRefs(ag state.Agent) []string {
	var refs []string
	if ag.Park != nil && ag.Park.On != "" {
		if r := transcripts.NormalizeRef(ag.Park.On); r != "" {
			refs = append(refs, r)
		}
	}
	refs = append(refs, transcripts.ExtractRefs(ag.Task)...)
	refs = append(refs, transcripts.ExtractRefs(ag.LastTask)...)
	for _, m := range issueRefInText.FindAllString(ag.Task+" "+ag.LastTask, -1) {
		if r := transcripts.NormalizeRef(m); r != "" && !slices.Contains(refs, r) {
			refs = append(refs, r)
		}
	}
	return refs
}

var issueRefInText = issueRef

// syncAllTranscripts ingests local transcripts across all harnesses and
// records metadata (and optional incremental remote log pages) for SSH
// executions.
func (a *app) syncAllTranscripts(ctx context.Context, st *state.State, pullRemote bool) error {
	a.syncLocalTranscripts(st)
	return a.syncRemoteTranscripts(ctx, st, pullRemote)
}

func (a *app) syncLocalTranscripts(st *state.State) {
	store := a.transcriptStore()
	host := cmp.Or(a.cfg.Identity.Host, "local")
	byCLI := claude.ByCLI(a.cfg)

	running, _, _ := a.sessions()
	seenPaths := map[string]bool{}
	runningKeys := map[string]bool{}

	ingestSession := func(s *claude.Session, isRunning bool) {
		if s == nil || s.Transcript == "" {
			return
		}
		cleanPath := filepath.Clean(s.Transcript)
		seenPaths[cleanPath] = true
		harness := cmp.Or(s.Harness, claudeComm)
		id := cmp.Or(s.ID, s.HostID, strings.TrimSuffix(filepath.Base(cleanPath), filepath.Ext(cleanPath)))
		key := transcripts.SafeKey(harness, host, id)
		if isRunning {
			runningKeys[key] = true
		}
		meta := transcripts.Meta{
			Key:         key,
			ID:          s.ID,
			HostSession: s.HostID,
			Name:        s.Name,
			Harness:     harness,
			Host:        host,
			Cwd:         s.Cwd,
			Model:       s.Model,
			Repo:        s.Repo,
			Running:     isRunning,
			StartedAt:   s.Started,
			LastActive:  s.LastActive,
		}
		a.enrichMetaFromState(st, &meta, s.Party(), byCLI)
		_, _, _ = store.IngestFile(meta, cleanPath)
	}

	for _, s := range running {
		ingestSession(s, true)
	}

	// Ended sessions across harnesses (omp, agy, codex, etc.).
	since := a.now.Add(-30 * 24 * time.Hour)
	for _, h := range harnesses {
		for _, e := range h.Ended(a.cfg, running, since) {
			if e.Transcript != "" && !seenPaths[filepath.Clean(e.Transcript)] {
				if e.Harness == "" {
					e.Harness = omp.Harness
				}
				ingestSession(e, false)
			}
		}
	}

	// Stopped/archived Claude desktop records.
	for _, r := range byCLI {
		if r.CLISessionID == "" {
			continue
		}
		tp := claude.Transcript(a.cfg, r.CLISessionID, cmp.Or(r.Cwd, r.OriginCwd))
		if tp == "" || seenPaths[filepath.Clean(tp)] {
			continue
		}
		seenPaths[filepath.Clean(tp)] = true
		repo, _ := claude.GitInfo(cmp.Or(r.Cwd, r.OriginCwd))
		meta := transcripts.Meta{
			Key:         transcripts.SafeKey(claudeComm, host, r.CLISessionID),
			ID:          r.CLISessionID,
			HostSession: r.SessionID,
			Name:        r.Title,
			Harness:     claudeComm,
			Host:        host,
			Cwd:         cmp.Or(r.Cwd, r.OriginCwd),
			Model:       r.Model,
			Repo:        repo,
			Running:     false,
			LastActive:  r.LastSeen(),
		}
		a.enrichMetaFromState(st, &meta, r.Party(), byCLI)
		_, _, _ = store.IngestFile(meta, tp)
	}

	// Scan Claude ProjectsDir (<projectsDir>/<proj>/<session>.jsonl) for any
	// headless or started Claude sessions not already seen.
	if a.cfg.Claude.ProjectsDir != "" {
		projEntries, _ := os.ReadDir(a.cfg.Claude.ProjectsDir)
		for _, pe := range projEntries {
			if !pe.IsDir() {
				continue
			}
			pDir := filepath.Join(a.cfg.Claude.ProjectsDir, pe.Name())
			files, _ := os.ReadDir(pDir)
			for _, f := range files {
				if f.IsDir() || !strings.HasSuffix(f.Name(), ".jsonl") {
					continue
				}
				tp := filepath.Clean(filepath.Join(pDir, f.Name()))
				if seenPaths[tp] {
					continue
				}
				seenPaths[tp] = true
				sid := strings.TrimSuffix(f.Name(), ".jsonl")
				meta := transcripts.Meta{
					Key:     transcripts.SafeKey(claudeComm, host, sid),
					ID:      sid,
					Harness: claudeComm,
					Host:    host,
					Running: false,
				}
				a.enrichMetaFromState(st, &meta, state.Party{Session: sid}, byCLI)
				_, _, _ = store.IngestFile(meta, tp)
			}
		}
	}

	// Mark any previously running local transcripts that are no longer running as ended.
	if existing, err := store.List(); err == nil {
		for _, m := range existing {
			if m.Execution == "" && m.Host == host && m.Running && !runningKeys[m.Key] {
				m.Running = false
				if m.EndedAt.IsZero() {
					m.EndedAt = cmp.Or(m.LastActive, a.now).UTC()
				}
				_, _ = store.UpdateMeta(m)
			}
		}
	}
}

func (a *app) enrichMetaFromState(st *state.State, meta *transcripts.Meta, p state.Party, byCLI map[string]*claude.Record) {
	if meta.ID != "" && byCLI != nil {
		if r, ok := byCLI[meta.ID]; ok {
			if meta.HostSession == "" {
				meta.HostSession = r.SessionID
			}
			if meta.Name == "" {
				meta.Name = r.Title
			}
			if meta.Model == "" {
				meta.Model = r.Model
			}
			if meta.Cwd == "" {
				meta.Cwd = cmp.Or(r.Cwd, r.OriginCwd)
			}
		}
	}
	if p.HostSession == "" {
		p.HostSession = meta.HostSession
	}
	if p.Session == "" {
		p.Session = meta.ID
	}

	if h := st.GuideRole().Holder; h != nil && h.Is(p) {
		meta.Role = guideRole.name
		meta.RosterName = cmp.Or(h.Name, guideRole.name)
	}
	for _, rel := range st.GuideRole().Relieved {
		if rel.Party.Is(p) {
			meta.Role = guideRole.name
			if meta.RosterName == "" {
				meta.RosterName = cmp.Or(rel.Party.Name, guideRole.name)
			}
		}
	}

	for _, s := range slices.Concat(st.Starts, st.EndedWorkers) {
		conversation := meta.ID != "" && s.Conversation == meta.ID
		if s.Is(p) || conversation || (meta.ID != "" && s.Session == meta.ID) || (meta.HostSession != "" && s.HostSession == meta.HostSession) {
			if conversation {
				meta.HostSession = cmp.Or(meta.HostSession, s.HostSession)
				p.HostSession = cmp.Or(s.HostSession, p.HostSession)
			}
			for _, ref := range transcripts.ExtractRefs(s.Task) {
				meta.Ref = cmp.Or(meta.Ref, ref)
				if !slices.Contains(meta.Refs, ref) {
					meta.Refs = append(meta.Refs, ref)
				}
			}
			if meta.RosterName == "" {
				meta.RosterName = s.Name
			}
			if meta.Name == "" {
				meta.Name = s.Name
			}
			if meta.Role == "" {
				meta.Role = workerRole
			}
			if meta.StartedAt.IsZero() && !s.At.IsZero() {
				meta.StartedAt = s.At
			}
		}
	}

	for _, ag := range st.Agents {
		if ag.Is(p) || (meta.RosterName != "" && strings.EqualFold(ag.Name, meta.RosterName)) {
			meta.RosterName = ag.Name
			if meta.Role == "" {
				meta.Role = workerRole
			}
			if meta.Model == "" {
				meta.Model = ag.Model
			}
			for _, ref := range extractAgentRefs(ag) {
				if meta.Ref == "" {
					meta.Ref = ref
				}
				if !slices.Contains(meta.Refs, ref) {
					meta.Refs = append(meta.Refs, ref)
				}
			}
		}
	}

	for _, rec := range st.Records {
		if rec.Session.Is(p) || (meta.RosterName != "" && strings.EqualFold(rec.Session.Name, meta.RosterName)) {
			if ref := transcripts.NormalizeRef(rec.Issue); ref != "" {
				if meta.Ref == "" {
					meta.Ref = ref
				}
				if !slices.Contains(meta.Refs, ref) {
					meta.Refs = append(meta.Refs, ref)
				}
			}
			if !rec.Ended.IsZero() && meta.EndedAt.IsZero() && !meta.Running {
				meta.EndedAt = rec.Ended
			}
		}
	}

	for _, dr := range st.DeskReviews {
		if dr.Reviewer.Is(p) || (meta.RosterName != "" && strings.EqualFold(dr.Reviewer.Name, meta.RosterName)) {
			if ref := transcripts.NormalizeRef(dr.Ref); ref != "" {
				if meta.Ref == "" {
					meta.Ref = ref
				}
				if !slices.Contains(meta.Refs, ref) {
					meta.Refs = append(meta.Refs, ref)
				}
				if meta.Repo == "" {
					meta.Repo = transcripts.RepoOfRef(ref)
				}
			}
		}
	}

	if meta.Repo == "" && meta.Ref != "" {
		meta.Repo = transcripts.RepoOfRef(meta.Ref)
	}
	if meta.Repo == "" && meta.Cwd != "" {
		repo, _ := claude.GitInfo(meta.Cwd)
		meta.Repo = repo
	}
}

func (a *app) executionTranscriptMeta(st *state.State, start state.Start, r *state.ExecutionRecord) (transcripts.Meta, []transcripts.Turn) {
	execID := string(r.Handle.Ref.Execution)
	host := cmp.Or(string(r.Handle.Ref.Host), r.Selector, "remote")
	harness := cmp.Or(r.Request.Harness, claudeComm)
	running := !r.Terminal() && !r.Completed
	meta := transcripts.Meta{
		Key:        transcripts.SafeKey(harness, host, execID),
		ID:         r.Handle.ConversationID,
		Execution:  execID,
		Name:       cmp.Or(start.Name, r.Request.Name),
		RosterName: cmp.Or(start.Name, r.Request.Name),
		Role:       workerRole,
		Repo:       r.Request.Repository,
		Harness:    harness,
		Host:       host,
		Cwd:        r.Handle.Workspace,
		Model:      r.Request.Model,
		Running:    running,
		StartedAt:  cmp.Or(r.ReservedAt, start.At),
		LastActive: cmp.Or(r.LastActive, r.Observation.ObservedAt, r.ReservedAt, start.At),
	}
	if !running {
		meta.EndedAt = cmp.Or(r.Observation.ObservedAt, r.LastActive, a.now)
	}
	for _, ref := range transcripts.ExtractRefs(r.Request.Prompt) {
		if meta.Ref == "" {
			meta.Ref = ref
		}
		meta.Refs = append(meta.Refs, ref)
	}
	if r.Published != nil && r.Published.URL != "" {
		for _, ref := range transcripts.ExtractRefs(r.Published.URL) {
			if !slices.Contains(meta.Refs, ref) {
				meta.Refs = append(meta.Refs, ref)
			}
		}
	}
	a.enrichMetaFromState(st, &meta, start.Party, nil)

	var prompts []transcripts.Turn
	if strings.TrimSpace(r.Request.Prompt) != "" {
		prompts = append(prompts, transcripts.Turn{
			At:   cmp.Or(r.ReservedAt, start.At),
			Role: transcripts.RoleUser,
			Text: r.Request.Prompt,
		})
	}
	for _, m := range r.Messages {
		if strings.TrimSpace(m.Envelope.Text) != "" {
			prompts = append(prompts, transcripts.Turn{
				At:   cmp.Or(r.LastActive, r.ReservedAt, start.At),
				Role: transcripts.RoleUser,
				Text: m.Envelope.Text,
			})
		}
	}
	return meta, prompts
}

// syncRemoteTranscripts records each SSH execution's transcript metadata
// and, with pullLogs, collects its stream log. It returns every failure to
// collect or persist one; the caller says whether that fails it.
func (a *app) syncRemoteTranscripts(ctx context.Context, st *state.State, pullLogs bool) error {
	store := a.transcriptStore()
	var errs []error
	for _, s := range st.Starts {
		if s.Execution == nil {
			continue
		}
		r := s.Execution
		if pullLogs {
			err := a.collectExecutionTranscript(ctx, st, s, r)
			if err == nil {
				continue
			}
			errs = append(errs, fmt.Errorf("transcript of execution %s not collected: %w", r.Handle.Ref.Execution, err))
		}
		meta, prompts := a.executionTranscriptMeta(st, s, r)
		var err error
		if prev, ok, _ := store.ReadMeta(meta.Key); ok && prev.Turns > 0 {
			_, err = store.UpdateMeta(meta)
		} else {
			_, err = store.AppendRemoteLog(meta, nil, "", prompts)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("transcript of execution %s not stored: %w", r.Handle.Ref.Execution, err))
		}
	}
	return errors.Join(errs...)
}

// A transcript pull reads pages of transcriptPageLimit bytes. It stops after
// transcriptPullPages pages once it holds a complete record, and persists
// only through the last complete record: a secret split across two pages is
// redacted whole on a later pull, never written in two unmatched fragments.
// A single record longer than transcriptMaxRecord is persisted as it stands.
const (
	transcriptPageLimit = 256 << 10
	transcriptPullPages = 4
	transcriptMaxRecord = 64 << 20
)

// executionTranscriptPending says r ended but its stream log is not yet
// collected to EOF: the host reconciliation keeps pulling it after the
// execution settled.
func executionTranscriptPending(r *state.ExecutionRecord) bool {
	return r != nil && !r.Released() && r.Terminal() && !r.TranscriptCollected
}

// collectExecutionTranscript pulls r's stream log and, once a terminal
// execution's log is persisted to EOF, records it collected.
func (a *app) collectExecutionTranscript(ctx context.Context, st *state.State, start state.Start, r *state.ExecutionRecord) error {
	done, err := a.pullExecutionTranscript(ctx, st, start, r)
	if err != nil || !done || r.TranscriptCollected {
		return err
	}
	return a.store.Update(func(current *state.State) ([]state.Event, error) {
		if rec, _ := executionRecord(current, r.Handle.Ref); rec != nil && rec.Terminal() {
			rec.TranscriptCollected = true
		}
		return nil, nil
	})
}

// pullExecutionTranscript persists the next complete records of r's stream
// log and says whether the log of a terminal execution is persisted to EOF.
func (a *app) pullExecutionTranscript(ctx context.Context, st *state.State, start state.Start, r *state.ExecutionRecord) (bool, error) {
	if r == nil {
		return true, nil
	}
	h, err := a.executionHost(r.Selector)
	if err != nil {
		return false, err
	}
	store := a.transcriptStore()
	meta, prompts := a.executionTranscriptMeta(st, start, r)
	prev, _, err := store.ReadMeta(meta.Key)
	if err != nil {
		return false, err
	}
	// The record's terminal state is read before the log: a log read to EOF
	// after the process ended holds every record it will ever hold.
	terminal := r.Terminal()
	base, err := strconv.ParseInt(cmp.Or(prev.RemoteCursor, "0"), 10, 64)
	if err != nil || base < 0 {
		return false, fmt.Errorf("transcript cursor %q of %s is no byte offset", prev.RemoteCursor, meta.Key)
	}
	client := a.executionClient(h)
	var buf []byte
	after := strconv.FormatInt(base, 10)
	eof := false
	var pullErr error
	for pages := 1; ; pages++ {
		page, err := client.Logs(ctx, r.Handle.Ref, execution.LogRequest{After: after, Limit: transcriptPageLimit})
		if err != nil {
			pullErr = err
			break
		}
		buf = append(buf, page.Data...)
		if !page.Truncated {
			eof = true
			break
		}
		next := cmp.Or(page.Next, after)
		if next == after || len(page.Data) == 0 {
			pullErr = fmt.Errorf("execution %s log stalled at %s", r.Handle.Ref.Execution, after)
			break
		}
		after = next
		if (pages >= transcriptPullPages && bytes.IndexByte(buf, '\n') >= 0) || len(buf) >= transcriptMaxRecord {
			break
		}
	}
	cut := bytes.LastIndexByte(buf, '\n') + 1
	if (eof && terminal) || (cut == 0 && len(buf) >= transcriptMaxRecord) {
		cut = len(buf)
	}
	if _, err := store.AppendRemoteLog(meta, buf[:cut], strconv.FormatInt(base+int64(cut), 10), prompts); err != nil {
		return false, errors.Join(pullErr, err)
	}
	if errors.Is(pullErr, execution.ErrNotFound) && terminal {
		// The host holds no journal of the ended execution: nothing more
		// to collect.
		return true, nil
	}
	if pullErr != nil {
		return false, pullErr
	}
	return eof && terminal, nil
}

// syncRemoteExecutionTranscriptByRef collects the transcript of the
// execution ref and records the outcome on its record: a failure is an
// execution.transcript-unsaved event once, and kept until a pull succeeds.
// It returns the failure, which costs the host's reconciliation nothing
// unless the host is unreachable.
func (a *app) syncRemoteExecutionTranscriptByRef(ctx context.Context, ref execution.Ref) error {
	st, err := a.store.Read()
	if err != nil {
		return err
	}
	r, i := executionRecord(st, ref)
	if r == nil || i < 0 {
		return nil
	}
	pullErr := a.collectExecutionTranscript(ctx, st, st.Starts[i], r)
	why := ""
	if pullErr != nil {
		why = pullErr.Error()
	}
	if why == r.TranscriptError {
		return pullErr
	}
	return errors.Join(pullErr, a.store.Update(func(current *state.State) ([]state.Event, error) {
		rec, j := executionRecord(current, ref)
		if rec == nil || rec.TranscriptError == why {
			return nil, nil
		}
		was := rec.TranscriptError
		rec.TranscriptError = why
		if why == "" || was != "" {
			return nil, nil
		}
		return []state.Event{event(current.Starts[j].By, "execution.transcript-unsaved", "%s: %s", current.Starts[j].Name, why)}, nil
	}))
}

func (a *app) executionTranscriptRPC(ctx context.Context, host string, req execution.CallbackRequest) (any, error) {
	st, err := a.store.Read()
	if err != nil {
		return nil, err
	}
	r, i := executionRecord(st, req.Ref)
	if r == nil || i < 0 || req.Protocol != execution.Protocol || req.RequestID == "" || req.Ref.Validate() != nil || string(req.Ref.Host) != host || !execution.VerifyCallback(req, r.Request.ControlKey) {
		return nil, refused("unauthenticated execution callback")
	}
	_ = a.syncAllTranscripts(ctx, st, false) // queries read the stored transcripts; transcripts sync reports a refresh failure
	caller := a.workerCallerForParty(st, st.Starts[i].Party, st.Starts[i].Name)
	switch req.Operation {
	case transcriptsListOp:
		var q remoteTranscriptListReq
		if len(req.Payload) > 0 && execution.Decode(req.Payload, &q) != nil {
			return nil, refused("invalid transcript list request")
		}
		return a.listTranscriptsForCaller(st, caller, q)
	case transcriptsSearchOp:
		var q remoteTranscriptSearchReq
		if execution.Decode(req.Payload, &q) != nil {
			return nil, refused("invalid transcript search request")
		}
		return a.searchTranscriptsForCaller(st, caller, q)
	case transcriptsReadOp:
		var q remoteTranscriptReadReq
		if execution.Decode(req.Payload, &q) != nil {
			return nil, refused("invalid transcript read request")
		}
		return a.readTranscriptForCaller(ctx, st, caller, q)
	default:
		return nil, refused("unsupported transcript operation")
	}
}
