package observe

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/giantswarm/beekeeper/api/v1alpha1"
	"github.com/giantswarm/beekeeper/internal/clock"
	"github.com/giantswarm/beekeeper/internal/forge"
)

// ReadSet says which conditional REST reads a Change needs.
type ReadSet struct {
	// PR reads the pull request (state, head, merge commit).
	PR bool
	// Reviews reads the submitted reviews.
	Reviews bool
	// ReviewsIfChanged reads the reviews only when the PR read found the PR changed: submitting a
	// review updates the PR, so a 304 on the PR means its reviews are unchanged too.
	ReviewsIfChanged bool
	// Head and Merge read the check runs and statuses of the head and merge commits.
	Head  bool
	Merge bool
	// SHAs read the check runs and statuses of these commits when they are the head or merge commit.
	SHAs []string
}

// FullReads are every read of a Change (resync, startup): the PR, its reviews when the PR changed, and
// the checks and statuses of its head and merge commits.
func FullReads() ReadSet { return ReadSet{PR: true, ReviewsIfChanged: true, Head: true, Merge: true} }

// Empty reports whether nothing is to be read.
func (r ReadSet) Empty() bool {
	return !r.PR && !r.Reviews && !r.ReviewsIfChanged && !r.Head && !r.Merge && len(r.SHAs) == 0
}

// Add merges o into r.
func (r *ReadSet) Add(o ReadSet) {
	r.PR = r.PR || o.PR
	r.Reviews = r.Reviews || o.Reviews
	r.ReviewsIfChanged = r.ReviewsIfChanged || o.ReviewsIfChanged
	r.Head = r.Head || o.Head
	r.Merge = r.Merge || o.Merge
	for _, s := range o.SHAs {
		if !contains(r.SHAs, s) {
			r.SHAs = append(r.SHAs, s)
		}
	}
	sort.Strings(r.SHAs)
}

// WaitReads are the reads a wait needs at its deadline or while polling.
func WaitReads(w *v1alpha1.Wait) ReadSet {
	if w == nil {
		return ReadSet{}
	}
	switch w.On {
	case v1alpha1.WaitOnCI:
		if _, sha, ok := splitCI(w.Ref); ok {
			return ReadSet{SHAs: []string{sha}}
		}
		return ReadSet{Head: true}
	case v1alpha1.WaitOnReview:
		return ReadSet{PR: true, ReviewsIfChanged: true}
	}
	return ReadSet{}
}

func splitCI(ref string) (repo, sha string, ok bool) {
	v, found := strings.CutPrefix(ref, "ci:")
	if !found {
		return "", "", false
	}
	repo, sha, ok = strings.Cut(v, "@")
	return repo, sha, ok && sha != ""
}

// ETag keys of the reads, kept in status.etags.
func etagKey(op, path string) string { return op + " " + path }

// Rules returns the merge rules of a repo's branch (required checks and approvals), read when first
// needed and then served from memory; ok is false when they can't be read.
type Rules interface {
	Rules(ctx context.Context, repo, branch string) (forge.BranchRules, bool)
}

// DefaultRulesTTL is how long branch rules are served from memory before they are read again.
const DefaultRulesTTL = 24 * time.Hour

// RulesCache reads branch rules once per repo and branch and keeps them for TTL on Clock (forever
// without a clock), or until Forget (a configuration or installation change); concurrent first reads of
// the same branch make one request.
type RulesCache struct {
	Reader forge.Reader
	Clock  clock.Clock
	// TTL is DefaultRulesTTL when zero.
	TTL   time.Duration
	mu    sync.Mutex
	rules map[string]cachedRules
	reads map[string]*sync.Mutex
}

type cachedRules struct {
	rules forge.BranchRules
	at    time.Time
}

func (c *RulesCache) fresh(key string) (forge.BranchRules, bool) {
	r, ok := c.rules[key]
	if !ok {
		return forge.BranchRules{}, false
	}
	if c.Clock != nil {
		ttl := c.TTL
		if ttl <= 0 {
			ttl = DefaultRulesTTL
		}
		if !c.Clock.Now().Before(r.at.Add(ttl)) {
			return forge.BranchRules{}, false
		}
	}
	return r.rules, true
}

// Rules implements Rules.
func (c *RulesCache) Rules(ctx context.Context, repo, branch string) (forge.BranchRules, bool) {
	key := repo + "@" + branch
	c.mu.Lock()
	if r, ok := c.fresh(key); ok {
		c.mu.Unlock()
		return r, true
	}
	if c.reads == nil {
		c.reads = map[string]*sync.Mutex{}
	}
	l, ok := c.reads[key]
	if !ok {
		l = &sync.Mutex{}
		c.reads[key] = l
	}
	c.mu.Unlock()

	l.Lock()
	defer l.Unlock()
	c.mu.Lock()
	r, ok := c.fresh(key)
	c.mu.Unlock()
	if ok {
		return r, true
	}
	r, _, err := c.Reader.BranchRules(ctx, repo, branch, "")
	if err != nil {
		return forge.BranchRules{}, false
	}
	var at time.Time
	if c.Clock != nil {
		at = c.Clock.Now()
	}
	c.mu.Lock()
	if c.rules == nil {
		c.rules = map[string]cachedRules{}
	}
	c.rules[key] = cachedRules{rules: r, at: at}
	c.mu.Unlock()
	return r, true
}

// Forget drops every cached rule (the installation's repos or the configuration changed).
func (c *RulesCache) Forget() {
	c.mu.Lock()
	c.rules = nil
	c.mu.Unlock()
}

// ReadResult says what a Change's reads did.
type ReadResult struct {
	// Made is true when at least one request reached GitHub.
	Made bool
	// Err is the first failed read; the reads after it weren't made.
	Err *forge.Error
	// Done are the operations that succeeded (a 200 or a 304).
	Done []string
}

// ReadChange makes the reads rs of the Change of PR ref, conditionally with the ETags in st.ETags, and
// merges what changed into st.PR and st.Facts (a 304 keeps the previous answer). It stops at the first
// failed read and keeps what the reads before it returned.
func ReadChange(ctx context.Context, r forge.Reader, rules Rules, ref string, st *v1alpha1.ChangeStatus, rs ReadSet) ReadResult {
	var res ReadResult
	pr, err := v1alpha1.ParseRef(ref)
	if err != nil {
		res.Err = &forge.Error{Kind: forge.Refused, Operation: forge.OpPullsGet, Err: err}
		return res
	}
	repo := pr.FullRepo()
	if st.ETags == nil {
		st.ETags = map[string]string{}
	}
	if st.Facts == nil {
		st.Facts = &v1alpha1.ChangeFacts{}
	}
	if st.PR == nil || st.PR.State == "" {
		// Nothing observed yet: every read is needed.
		rs = FullReads()
		rs.Reviews = true
	}
	fail := func(op string, err error) ReadResult {
		fe, ok := forge.AsError(err)
		if !ok {
			fe = &forge.Error{Kind: forge.Transient, Operation: op, Err: err}
		}
		if fe.Operation == "" {
			fe.Operation = op
		}
		if fe.Code != 0 {
			res.Made = true
		}
		res.Err = fe
		return res
	}

	prChanged := false
	oldHead, oldMerge := "", ""
	if st.PR != nil {
		oldHead, oldMerge = st.PR.HeadSHA, st.PR.MergeSHA
	}
	if rs.PR {
		path := fmt.Sprintf("/repos/%s/pulls/%d", repo, pr.Number)
		k := etagKey(forge.OpPullsGet, path)
		got, meta, err := r.PullRequest(ctx, ref, st.ETags[k])
		if err != nil {
			return fail(forge.OpPullsGet, err)
		}
		res.Made = true
		res.Done = append(res.Done, forge.OpPullsGet)
		if !meta.NotModified || st.PR == nil || st.PR.State == "" {
			st.PR = PRStatus(got, repo, pr.Number)
			prChanged = true
		}
		if meta.ETag != "" {
			st.ETags[k] = meta.ETag
		}
	}
	head, merge := "", ""
	if st.PR != nil {
		head, merge = st.PR.HeadSHA, st.PR.MergeSHA
	}
	st.Facts.HeadSHA, st.Facts.MergeSHA = head, merge

	// The rules come from the branch's rulesets (required checks and approvals). Nothing fills
	// CodeOwnerTeams (nor the reviewers' teams) yet, so another team's approval is never required in
	// phase 1: the fields stay for when CODEOWNERS are read.
	if rules != nil && st.PR != nil && st.PR.Base != "" {
		if br, ok := rules.Rules(ctx, repo, st.PR.Base); ok {
			st.Facts.RequiredChecks = sortedCopy(br.RequiredChecks)
			st.Facts.RequiredApprovals = br.RequiredApprovals
			st.Facts.CodeOwnerTeams = sortedCopy(br.CodeOwnerTeams)
		}
	}
	if rs.Reviews || (rs.ReviewsIfChanged && (prChanged || !rs.PR)) {
		path := fmt.Sprintf("/repos/%s/pulls/%d/reviews", repo, pr.Number)
		k := etagKey(forge.OpPullsListReviews, path)
		got, meta, err := r.Reviews(ctx, ref, st.ETags[k])
		if err != nil {
			return fail(forge.OpPullsListReviews, err)
		}
		res.Made = true
		res.Done = append(res.Done, forge.OpPullsListReviews)
		if !meta.NotModified {
			st.Facts.Reviews = ReviewFacts(got)
		}
		if meta.ETag != "" {
			st.ETags[k] = meta.ETag
		}
	}

	readCommit := func(sha string, facts *[]v1alpha1.CheckFact, changed bool) error {
		if changed {
			*facts = nil
		}
		path := fmt.Sprintf("/repos/%s/commits/%s/check-runs", repo, sha)
		k := etagKey(forge.OpChecksListForRef, path)
		runs, meta, err := r.CheckRuns(ctx, repo, sha, st.ETags[k])
		if err != nil {
			fail(forge.OpChecksListForRef, err)
			return err
		}
		res.Made = true
		res.Done = append(res.Done, forge.OpChecksListForRef)
		if !meta.NotModified {
			*facts = replaceKind(*facts, FactCheck, CheckFacts(runs))
		}
		if meta.ETag != "" {
			st.ETags[k] = meta.ETag
		}
		path = fmt.Sprintf("/repos/%s/commits/%s/statuses", repo, sha)
		k = etagKey(forge.OpStatusesListForRef, path)
		statuses, meta, err := r.Statuses(ctx, repo, sha, st.ETags[k])
		if err != nil {
			fail(forge.OpStatusesListForRef, err)
			return err
		}
		res.Done = append(res.Done, forge.OpStatusesListForRef)
		if !meta.NotModified {
			*facts = replaceKind(*facts, FactStatus, StatusFacts(statuses))
		}
		if meta.ETag != "" {
			st.ETags[k] = meta.ETag
		}
		return nil
	}
	if head != "" && (rs.Head || contains(rs.SHAs, head) || head != oldHead) {
		if readCommit(head, &st.Facts.Head, head != oldHead) != nil {
			return res
		}
	}
	if merge != "" && (rs.Merge || contains(rs.SHAs, merge) || merge != oldMerge) {
		if readCommit(merge, &st.Facts.Merge, merge != oldMerge) != nil {
			return res
		}
	}
	if merge == "" {
		st.Facts.Merge = nil
	}

	pruneETags(st.ETags, repo, head, merge)
	return res
}

// pruneETags drops the ETags of commits that are neither the head nor the merge commit any more.
func pruneETags(etags map[string]string, repo, head, merge string) {
	prefix := "/repos/" + repo + "/commits/"
	for k := range etags {
		_, path, _ := strings.Cut(k, " ")
		rest, ok := strings.CutPrefix(path, prefix)
		if !ok {
			continue
		}
		sha, _, _ := strings.Cut(rest, "/")
		if sha != head && sha != merge {
			delete(etags, k)
		}
	}
}
