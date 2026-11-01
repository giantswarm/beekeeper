package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/execution"
	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/merge"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/superloop"
)

func (a *app) mergeAuthorityRoot() string {
	return filepath.Join(a.cfg.StateDir, "execution-merge-authority")
}

// Mutations have no transport retry: a lost response is reconciled by reading
// GitHub, never by issuing the merge a second time.
var runExecutionMerge = func(ctx context.Context, use execution.MergeUse) error {
	c := exec.CommandContext(ctx, "gh", "api", "--method", "PUT", fmt.Sprintf("repos/%s/pulls/%d/merge", use.Repository, use.PR), "-f", "sha="+use.Head, "-f", "merge_method=squash") //nolint:gosec // repository and head validated by the grant, no shell
	out, err := c.Output()
	if err != nil {
		return errors.New("merge outcome uncertain; reconcile authorization before further action")
	}
	var reply struct {
		Merged bool `json:"merged"`
	}
	if json.Unmarshal(out, &reply) != nil || !reply.Merged {
		return errors.New("merge not confirmed; reconcile authorization")
	}
	return nil
}

func (a *app) executionMergeLive(ctx context.Context, st *state.State, use execution.MergeUse) error {
	r, _ := executionRecord(st, use.Ref)
	if r == nil || r.Request.Repository != use.Repository || !execution.SupportedHarness(r.Request.Harness) || !r.Request.LeaseRequired || r.Lease == nil || !r.Lease.ExpiresAt.After(time.Now()) || r.StopRequest != nil || r.Completed || r.Terminal() || st.Stop != nil {
		return refused("execution merge authority stopped or mismatched")
	}
	owner, _, _ := strings.Cut(use.Repository, "/")
	if !slices.Contains(a.cfg.GitHub.AllowedOrgs, owner) {
		return refused("merge repository is outside the coordinator allowlist")
	}
	lane := a.cfg.LaneOf(use.Repository)
	now := time.Now()
	_, workerIndex := executionRecord(st, use.Ref)
	worker := st.Starts[workerIndex].Party
	if _, held := merge.BlockingFor(st, now, worker.Name, use.Repository, use.PR, lane); held {
		return refused("merge held by repository, lane or installation policy")
	}
	queued := *st
	queued.Merges = append([]state.Merge(nil), st.Merges...)
	if !slices.ContainsFunc(queued.Merges, func(entry state.Merge) bool { return entry.Repo == use.Repository && entry.PR == use.PR }) {
		queued.Merges = append(queued.Merges, state.Merge{Repo: use.Repository, PR: use.PR, Lane: lane.Name, By: worker, Phase: state.Waiting, Joined: now})
	}
	queue := merge.Queue(&queued, lane.Name)
	if queue.Running != nil {
		return refused("merge lane is running another merge")
	}
	if _, ahead := queue.Ahead(use.Repository, use.PR, merge.Unheld(st, now, lane, func(state.Merge) bool { return true })); ahead {
		return refused("another unheld merge precedes this request")
	}
	_, fixWindow := merge.FixWindow(st, now, use.Repository, use.PR, lane)
	if lane.Installation == "" || !fixWindow {
		var releases []merge.HelmRelease
		if lane.Installation != "" {
			var err error
			releases, err = readHelmReleases(ctx, lane)
			if err != nil {
				return refused("merge lane readiness unavailable")
			}
		}
		settling := queue.AllSettling
		if len(settling) == 0 {
			settling = []*state.Merge{nil}
		}
		for _, entry := range settling {
			if ready, _ := merge.Ready(lane, releases, entry, now, a.cfg.Merge.Settle.Duration); !ready {
				return refused("merge lane has not settled")
			}
		}
	}
	pull, err := readLoopPull(ctx, fmt.Sprintf("%s#%d", use.Repository, use.PR))
	if err != nil {
		return err
	}
	if pull.State != superloop.Open || pull.Head != use.Head || !pull.Green {
		return refused("merge head moved or current CI is not green")
	}
	return a.authenticateExecutionReview(st, use, pull)
}

func (a *app) issueExecutionMerge(ctx context.Context, ref execution.Ref, repository string, pr int, head string, issuer state.Party) (execution.MergeAuthorization, error) {
	grant := execution.MergeAuthorization{ID: uuid.NewString(), Ref: ref, Repository: repository, PR: pr, Head: head, Issuer: issuer.Name, IssuedAt: time.Now().UTC()}
	grant.ExpiresAt = grant.IssuedAt.Add(2 * time.Minute)
	use := execution.MergeUse{ID: grant.ID, Ref: ref, Repository: repository, PR: pr, Head: head}
	if err := grant.Admits(use, time.Now()); err != nil {
		return grant, err
	}
	err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		supervisor := st.SupervisorRole().Holder
		if (supervisor == nil || !supervisor.Is(issuer)) && issuer != loopParty {
			return nil, refused("only the coordinating supervisor can issue merge authority")
		}
		if err := a.executionMergeLive(ctx, st, use); err != nil {
			return nil, err
		}
		round := executionReviewRound(st, use)
		grant.DeskReviewID, grant.ReviewID, grant.ReviewDigest = round.ID, round.Result.ID, round.Result.BodyDigest
		if err := execution.WriteMergeAuthorization(a.mergeAuthorityRoot(), grant, "issued", time.Now()); err != nil {
			return nil, err
		}
		return []state.Event{event(issuer, "execution.merge-issued", "%s for https://github.com/%s/pull/%d at %s", grant.ID, repository, pr, head)}, nil
	})
	return grant, err
}

func (a *app) executionAuthorizationsCmd() *cobra.Command {
	c := &cobra.Command{Use: "authorizations", Short: "Supervisor-owned SSH merge grants and reconciliation"}
	var head string
	issue := &cobra.Command{Use: "grant <worker> <repository> <pr>", Args: cobra.ExactArgs(3), RunE: func(cmd *cobra.Command, args []string) error {
		me, err := a.caller()
		if err != nil {
			return err
		}
		ref, err := a.executionByName(args[0])
		if err != nil {
			return err
		}
		pr, err := strconv.Atoi(args[2])
		if err != nil {
			return err
		}
		grant, err := a.issueExecutionMerge(cmd.Context(), ref, args[1], pr, head, me)
		if err != nil {
			return err
		}
		return json.NewEncoder(cmd.OutOrStdout()).Encode(grant)
	}}
	issue.Flags().StringVar(&head, "head", "", "independently reviewed exact head")
	c.AddCommand(issue)
	for _, verb := range []string{"inspect", "revoke", "reconcile"} {
		c.AddCommand(&cobra.Command{Use: verb + " <authorization>", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
			me, err := a.caller()
			if err != nil {
				return err
			}
			var grant execution.MergeAuthorization
			err = a.store.Update(func(st *state.State) ([]state.Event, error) {
				sup := st.SupervisorRole().Holder
				if (sup == nil || !sup.Is(me)) && me != loopParty {
					return nil, refused("only the coordinating supervisor controls merge authorizations")
				}
				grant, err = execution.ReadMergeAuthorization(a.mergeAuthorityRoot(), args[0])
				if err != nil {
					return nil, err
				}
				switch verb {
				case "revoke":
					grant.Revoked = true
				case "reconcile":
					if !grant.Consumed {
						return nil, refused("authorization has no merge attempt to reconcile")
					}
					out, readErr := github.RunGH(cmd.Context(), "pr", "view", strconv.Itoa(grant.PR), "--repo", grant.Repository, "--json", "state,headRefOid")
					if readErr != nil {
						return nil, readErr
					}
					var pull struct {
						State string `json:"state"`
						Head  string `json:"headRefOid"`
					}
					if json.Unmarshal(out, &pull) != nil {
						return nil, errors.New("invalid merge reconciliation")
					}
					grant.Outcome = "unconfirmed"
					if pull.State == superloop.Merged && pull.Head == grant.Head {
						grant.Outcome = execution.MergeSucceeded
					}
					a.recordExecutionMergeLane(st, execution.MergeUse{ID: grant.ID, Ref: grant.Ref, Repository: grant.Repository, PR: grant.PR, Head: grant.Head}, grant.Outcome, me)
				}
				if verb != "inspect" {
					if err := execution.WriteMergeAuthorization(a.mergeAuthorityRoot(), grant, verb, time.Now()); err != nil {
						return nil, err
					}
				}
				return []state.Event{event(me, "execution.merge-"+verb, "%s", grant.ID)}, nil
			})
			if err != nil {
				return err
			}
			return json.NewEncoder(cmd.OutOrStdout()).Encode(grant)
		}})
	}
	return c
}

func (a *app) executionMergeCmd() *cobra.Command {
	var head, id string
	c := &cobra.Command{Use: "merge <repository> <pr>", Short: "Request one authorized SSH merge through the live coordinator", Args: cobra.ExactArgs(2), RunE: func(cmd *cobra.Command, args []string) error {
		s, j, err := a.executionCaller()
		if err != nil {
			return err
		}
		policy, policyErr := s.InstalledPolicy()
		if policyErr != nil || policy.Digest() != j.Request.PolicyDigest || j.StopRequest != nil {
			return refused("remote merge policy changed or execution stopped")
		}
		if !execution.SupportedHarness(j.Request.Harness) {
			return refused("harness has no supported remote merge surface")
		}
		pr, err := strconv.Atoi(args[1])
		if err != nil {
			return err
		}
		use := execution.MergeUse{ID: id, Ref: j.Handle.Ref, Repository: args[0], PR: pr, Head: head}
		if use.Repository != j.Request.Repository || id == "" {
			return refused("merge request does not match the owned repository")
		}
		var result execution.MergeResult
		co := s.Config.Coordinators[j.Request.Coordinator]
		err = execution.CoordinatorCall(cmd.Context(), co.Callback, string(use.Ref.Host), runMerge, use.Ref, use, j.Request.ControlKey, &result, nil)
		// Both sides retain an audit identity even when the callback disconnects.
		audit, _ := execution.ReadMergeAuthorization(filepath.Join(s.Root, "merge-audit"), id)
		audit.ID, audit.Ref, audit.Repository, audit.PR, audit.Head, audit.Outcome = id, use.Ref, use.Repository, pr, head, result.Outcome
		action := "merge-acknowledged"
		if err != nil {
			action = "merge-unconfirmed"
		}
		auditErr := execution.WriteMergeAuthorization(filepath.Join(s.Root, "merge-audit"), audit, action, time.Now())
		if err != nil {
			return errors.Join(err, auditErr)
		}
		if auditErr != nil {
			return auditErr
		}
		return json.NewEncoder(cmd.OutOrStdout()).Encode(result)
	}}
	c.Flags().StringVar(&head, "head", "", "exact reviewed head")
	c.Flags().StringVar(&id, "authorization", "", "supervisor-issued authorization identity")
	return c
}

// A retried consumed request reads the result first, never another mutation.
func (a *app) reconcileExecutionMerge(ctx context.Context, use execution.MergeUse) (execution.MergeResult, error) {
	result := execution.MergeResult{ID: use.ID, Outcome: "uncertain"}
	grant, err := execution.ReadMergeAuthorization(a.mergeAuthorityRoot(), use.ID)
	if err != nil {
		return result, err
	}
	if grant.ID != use.ID || grant.Ref != use.Ref || grant.Repository != use.Repository || grant.PR != use.PR || grant.Head != use.Head || !grant.Consumed {
		return result, refused("merge reconciliation tuple mismatch")
	}
	pull, err := readLoopPull(ctx, fmt.Sprintf("%s#%d", grant.Repository, grant.PR))
	if err != nil {
		return result, err
	}
	if pull.State == superloop.Merged && pull.Head == grant.Head {
		result.Outcome = execution.MergeSucceeded
	}
	grant.Outcome = result.Outcome
	if err := execution.WriteMergeAuthorization(a.mergeAuthorityRoot(), grant, "reconciled", time.Now()); err != nil {
		return result, err
	}
	if result.Outcome != execution.MergeSucceeded {
		return result, refused("consumed merge outcome remains uncertain; supervisor reconciliation required")
	}
	return result, nil
}

func (a *app) recordExecutionMergeLane(st *state.State, use execution.MergeUse, outcome string, by state.Party) {
	now := time.Now().UTC()
	i := slices.IndexFunc(st.Merges, func(entry state.Merge) bool { return entry.Repo == use.Repository && entry.PR == use.PR })
	if i < 0 {
		st.Merges = append(st.Merges, state.Merge{Repo: use.Repository, PR: use.PR, Lane: a.cfg.LaneOf(use.Repository).Name, By: by, Outside: true, Phase: state.Waiting, Joined: now, Seen: now})
		i = len(st.Merges) - 1
	}
	entry := &st.Merges[i]
	entry.Outside, entry.Seeded, entry.Seen = true, false, now
	if outcome == execution.MergeSucceeded && entry.Phase != state.Settling {
		entry.Phase, entry.Finished = state.Settling, now
	}
}
