package state

import (
	"strings"
	"time"

	"github.com/giantswarm/beekeeper/internal/execution"
)

// DeskReview binds an independent round to the concrete sessions the
// coordinating component started. A display name alone is never a credential.
type DeskReview struct {
	ID         string            `json:"id"`
	Ref        string            `json:"ref"`
	Head       string            `json:"head"`
	Round      int               `json:"round"`
	Worker     Party             `json:"worker"`
	Reviewer   Party             `json:"reviewer"`
	AssignedAt time.Time         `json:"assignedAt"`
	Result     *DeskReviewResult `json:"result,omitempty"`
}

// DeskReviewResult authenticates a specific public review through the assigned
// reviewer's durable completion report, not through their shared GitHub login.
type DeskReviewResult struct {
	ID           string    `json:"id"`
	URL          string    `json:"url"`
	Author       string    `json:"author"`
	Verdict      string    `json:"verdict"`
	Head         string    `json:"head,omitempty"`
	BodyDigest   string    `json:"bodyDigest"`
	ReportDigest string    `json:"reportDigest"`
	ReportedAt   time.Time `json:"reportedAt"`
	By           Party     `json:"by"`
}

// ReviewPark retains each proven pull-request review request independently
// of the worker's current park and roster lifetime.
type ReviewPark struct {
	On         string         `json:"on"`
	At         time.Time      `json:"at"`
	ReviewHead string         `json:"reviewHead"`
	Agent      string         `json:"agent"`
	Party      Party          `json:"party"`
	Execution  *execution.Ref `json:"execution,omitempty"`
	Selector   string         `json:"selector,omitempty"`
	Model      string         `json:"model,omitempty"`
	Serves     string         `json:"serves,omitempty"`
	PostMerge  *string        `json:"postMerge,omitempty"`
	Worktree   string         `json:"worktree,omitempty"`
}

// ReviewParkFor returns the recorded desk-review park for on (owner/repo#n).
func (st *State) ReviewParkFor(on string) (ReviewPark, bool) {
	if st == nil {
		return ReviewPark{}, false
	}
	for _, rp := range st.ReviewParks {
		if strings.EqualFold(rp.On, on) {
			return rp, true
		}
	}
	return ReviewPark{}, false
}

// UpsertReviewPark records or updates the desk-review park for rp.On.
func (st *State) UpsertReviewPark(rp ReviewPark) {
	if st == nil || rp.On == "" || rp.ReviewHead == "" {
		return
	}
	for i, cur := range st.ReviewParks {
		if !strings.EqualFold(cur.On, rp.On) {
			continue
		}
		if rp.Serves == "" {
			rp.Serves = cur.Serves
		}
		if rp.PostMerge == nil {
			rp.PostMerge = cur.PostMerge
		}
		if rp.Selector == "" {
			rp.Selector = cur.Selector
		}
		if rp.Model == "" {
			rp.Model = cur.Model
		}
		if rp.Worktree == "" {
			rp.Worktree = cur.Worktree
		}
		st.ReviewParks[i] = rp
		return
	}
	st.ReviewParks = append(st.ReviewParks, rp)
}

// ReviewRequest reserves a review before its session starts.
// It survives roster pruning and prevents duplicate starts on an unchanged head.
type ReviewRequest struct {
	Ref   string `json:"ref"`
	Head  string `json:"head"`
	Round int    `json:"round"`
	// By is the session that asked for the review by hand (agents review);
	// the loop's own reviews name none. It gets the verdict.
	By Party `json:"by,omitzero"`
}

// ReviewNitCheck is a supervisor's explicit attestation that a new head only
// fixes nits in an approval. It never substitutes for that original review.
type ReviewNitCheck struct {
	Ref          string    `json:"ref"`
	Head         string    `json:"head"`
	ApprovedHead string    `json:"approvedHead"`
	By           Party     `json:"by"`
	At           time.Time `json:"at"`
}

func (st *State) ReviewNitsChecked(ref, head, approved string) bool {
	if st == nil || head == "" || approved == "" {
		return false
	}
	for _, check := range st.ReviewNitChecks {
		if strings.EqualFold(check.Ref, ref) && check.Head == head && check.ApprovedHead == approved && check.By.Session == "" && check.By.HostSession == "" {
			return true
		}
	}
	return false
}
