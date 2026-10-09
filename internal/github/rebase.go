package github

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
)

// DirtyState is GitHub's mergeable_state of a pull request that conflicts
// with its base.
const DirtyState = "dirty"

// Rebase is whether a pull request rebases onto its base as GitHub computed
// it. Known is false while GitHub has not computed it yet (rebaseable null),
// which it does in the background after the first read.
type Rebase struct {
	Known      bool
	Rebaseable bool
	// State is GitHub's mergeable_state: clean, dirty, blocked, behind, …
	State string
}

// Conflicts says the pull request cannot be rebase-merged: GitHub reports it
// not rebaseable, or conflicting with its base.
func (r Rebase) Conflicts() bool {
	return r.Known && !r.Rebaseable || r.State == DirtyState
}

// ReadRebase reads repo#n's rebaseable and mergeable_state, one request; an
// error is GitHub not answering.
func ReadRebase(ctx context.Context, run GH, repo string, n int) (Rebase, error) {
	out, err := run(ctx, "api", fmt.Sprintf("repos/%s/pulls/%s", repo, strconv.Itoa(n)))
	if err != nil {
		return Rebase{}, err
	}
	var p struct {
		Rebaseable     *bool  `json:"rebaseable"`
		MergeableState string `json:"mergeable_state"`
	}
	if err := json.Unmarshal(out, &p); err != nil {
		return Rebase{}, fmt.Errorf("gh api repos/%s/pulls/%d: %w", repo, n, err)
	}
	r := Rebase{State: p.MergeableState}
	if p.Rebaseable != nil {
		r.Known, r.Rebaseable = true, *p.Rebaseable
	}
	return r, nil
}
