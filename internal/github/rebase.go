package github

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
)

// The mergeable_state of a pull request that conflicts with its base
// (DirtyState), and of one whose head lacks commits of a base whose branch
// protection requires branches to be up to date (BehindState).
const (
	DirtyState  = "dirty"
	BehindState = "behind"
)

// Rebase is whether a pull request rebases onto its base as GitHub computed
// it. Known is false while GitHub has not computed it yet (rebaseable null),
// which it does in the background after the first read.
type Rebase struct {
	Known      bool
	Rebaseable bool
	// State is GitHub's mergeable_state: clean, dirty, blocked, behind, …
	State string
	// Head is the pull request's head commit.
	Head string
}

// Conflicts says the pull request cannot be rebase-merged: GitHub reports it
// not rebaseable, or conflicting with its base.
func (r Rebase) Conflicts() bool {
	return r.Known && !r.Rebaseable || r.State == DirtyState
}

// Behind says the pull request rebases cleanly but lags a base that requires
// branches to be up to date: GitHub refuses its merge until it is updated.
func (r Rebase) Behind() bool {
	return !r.Conflicts() && r.State == BehindState
}

// ReadRebase reads repo#n's rebaseable, mergeable_state and head, one
// request; an error is GitHub not answering.
func ReadRebase(ctx context.Context, run GH, repo string, n int) (Rebase, error) {
	out, err := run(ctx, "api", fmt.Sprintf("repos/%s/pulls/%s", repo, strconv.Itoa(n)))
	if err != nil {
		return Rebase{}, err
	}
	var p struct {
		Rebaseable     *bool  `json:"rebaseable"`
		MergeableState string `json:"mergeable_state"`
		Head           struct {
			SHA string `json:"sha"`
		} `json:"head"`
	}
	if err := json.Unmarshal(out, &p); err != nil {
		return Rebase{}, fmt.Errorf("gh api repos/%s/pulls/%d: %w", repo, n, err)
	}
	r := Rebase{State: p.MergeableState, Head: p.Head.SHA}
	if p.Rebaseable != nil {
		r.Known, r.Rebaseable = true, *p.Rebaseable
	}
	return r, nil
}

// UpdateByRebase rebases repo#n's head onto its base on GitHub's side (the
// pull request's "Update with rebase"), never a merge of the base into the
// branch, which a rebase-merge line forbids; an error carries GitHub's
// sentence (a conflict, a head GitHub may not write).
func UpdateByRebase(ctx context.Context, run GH, repo string, n int) error {
	_, err := run(ctx, "pr", "update-branch", strconv.Itoa(n), "--repo", repo, "--rebase")
	return err
}
