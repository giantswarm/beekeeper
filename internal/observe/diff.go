package observe

import (
	"reflect"
	"sort"
	"time"

	"github.com/giantswarm/beekeeper/api/v1alpha1"
	"github.com/giantswarm/beekeeper/internal/history"
)

// Observed kinds (contract §5.2).
const (
	KindBoard        = "board"
	KindIssue        = "issue"
	KindPR           = "pr"
	KindCI           = "ci"
	KindReview       = "review"
	KindMerge        = "merge"
	KindSubIssues    = "sub-issues"
	KindDependencies = "dependencies"
)

// Event types written by observation.
const (
	EventObserved = "observed"
	EventMissed   = "webhook.missed"
	EventRefusal  = "refusal"
)

// Webhook events an observation can find missed.
const (
	eventProjectsV2Item    = "projects_v2_item"
	eventIssues            = "issues"
	eventSubIssues         = "sub_issues"
	eventIssueDependencies = "issue_dependencies"
	eventPullRequest       = "pull_request"
	eventPullRequestReview = "pull_request_review"
	eventCheckRun          = "check_run"
	eventCheckSuite        = "check_suite"
	eventStatus            = "status"
)

// Observation is one observed change of an object's facts.
type Observation struct {
	Kind   string
	Fields []string
	// Missed are the webhook deliveries that would have announced the change, one per delivery.
	Missed []Missed
}

// Missed is a webhook delivery GitHub would have sent for an observed change.
type Missed struct {
	Event string
	// Repo is where the delivery would come from; changes in repos outside the installation can't send
	// webhooks to the App and don't count as missed.
	Repo string
}

// DiffItem compares an Item's observed facts before and after an observation. A first observation
// (nothing observed before) is one board observation, never missed.
func DiffItem(old, cur *v1alpha1.ItemStatus) []Observation {
	if old == nil || old.Issue == nil || old.Board == nil {
		return []Observation{{Kind: KindBoard, Fields: setItemFields(cur)}}
	}
	repo := cur.Issue.Repo
	var obs []Observation
	add := func(kind string, fields []string, missed []Missed) {
		if len(fields) > 0 {
			obs = append(obs, Observation{Kind: kind, Fields: fields, Missed: missed})
		}
	}

	var fields []string
	var missed []Missed
	field := func(name string, changed bool, event string) {
		if changed {
			fields = append(fields, name)
			if event != "" {
				missed = append(missed, Missed{Event: event, Repo: repo})
			}
		}
	}
	const pvi, iss = eventProjectsV2Item, eventIssues
	ob, nb := old.Board, cur.Board
	field("board.status", ob.Status != nb.Status, pvi)
	field("board.kind", ob.Kind != nb.Kind, pvi)
	field("board.team", ob.Team != nb.Team, pvi)
	field("board.priority", !intPtrEqual(ob.Priority, nb.Priority), pvi)
	add(KindBoard, fields, missed)

	fields, missed = nil, nil
	oi, ni := old.Issue, cur.Issue
	field("issue.title", oi.Title != ni.Title, iss)
	field("issue.body", oi.Body != ni.Body, iss)
	field("issue.state", oi.State != ni.State, iss)
	field("issue.labels", !strsEqual(oi.Labels, ni.Labels), iss)
	field("assignees", !strsEqual(old.Assignees, cur.Assignees), iss)
	add(KindIssue, fields, missed)

	fields, missed = nil, nil
	field("parent", old.Parent != cur.Parent, eventSubIssues)
	oldSubs, newSubs := subRefs(old.SubIssues), subRefs(cur.SubIssues)
	for range symDiff(oldSubs, newSubs) {
		missed = append(missed, Missed{Event: eventSubIssues, Repo: repo})
	}
	if !strsEqual(oldSubs, newSubs) {
		fields = append(fields, "subIssues")
	}
	if subStates(old.SubIssues) != subStates(cur.SubIssues) && strsEqual(oldSubs, newSubs) {
		// A sub-issue's own state or status changed: its own webhook went to it, not to the epic.
		fields = append(fields, "subIssues.state")
	}
	if old.SubIssuesTotal != cur.SubIssuesTotal {
		fields = append(fields, "subIssuesTotal")
	}
	add(KindSubIssues, fields, missed)

	fields, missed = nil, nil
	oldB, newB := blockerRefs(old.BlockedBy), blockerRefs(cur.BlockedBy)
	for range symDiff(oldB, newB) {
		missed = append(missed, Missed{Event: eventIssueDependencies, Repo: repo})
	}
	if !strsEqual(oldB, newB) {
		fields = append(fields, "blockedBy")
	}
	if blockerStates(old.BlockedBy) != blockerStates(cur.BlockedBy) && strsEqual(oldB, newB) {
		fields = append(fields, "blockedBy.state")
	}
	if old.BlockedByTotal != cur.BlockedByTotal {
		fields = append(fields, "blockedByTotal")
	}
	add(KindDependencies, fields, missed)

	fields, missed = nil, nil
	for _, pr := range symDiff(old.Changes, cur.Changes) {
		if r, err := v1alpha1.ParseRef(pr); err == nil && contains(cur.Changes, pr) {
			missed = append(missed, Missed{Event: eventPullRequest, Repo: r.FullRepo()})
		}
	}
	if !strsEqual(old.Changes, cur.Changes) {
		fields = append(fields, "changes")
	}
	add(KindPR, fields, missed)
	return obs
}

func setItemFields(st *v1alpha1.ItemStatus) []string {
	var f []string
	if b := st.Board; b != nil {
		f = appendIf(f, "board.status", b.Status != "")
		f = appendIf(f, "board.kind", b.Kind != "")
		f = appendIf(f, "board.team", b.Team != "")
		f = appendIf(f, "board.priority", b.Priority != nil)
	}
	if i := st.Issue; i != nil {
		f = appendIf(f, "issue.title", i.Title != "")
		f = appendIf(f, "issue.body", i.Body != "")
		f = appendIf(f, "issue.state", i.State != "")
		f = appendIf(f, "issue.labels", len(i.Labels) > 0)
	}
	f = appendIf(f, "assignees", len(st.Assignees) > 0)
	f = appendIf(f, "parent", st.Parent != "")
	f = appendIf(f, "subIssues", len(st.SubIssues) > 0)
	f = appendIf(f, "blockedBy", len(st.BlockedBy) > 0)
	f = appendIf(f, "changes", len(st.Changes) > 0)
	return f
}

// DiffChange compares a Change's observed PR and facts before and after an observation. A first
// observation is one pr observation, never missed.
func DiffChange(old, cur *v1alpha1.ChangeStatus) []Observation {
	if cur.PR == nil || cur.PR.State == "" {
		return nil
	}
	if old == nil || old.PR == nil || old.PR.State == "" {
		return []Observation{{Kind: KindPR, Fields: setChangeFields(cur)}}
	}
	repo := cur.PR.Repo
	var obs []Observation
	op, np := old.PR, cur.PR

	var fields []string
	var missed []Missed
	if op.State != np.State || op.MergeSHA != np.MergeSHA {
		fields = appendIf(fields, "pr.state", op.State != np.State)
		fields = appendIf(fields, "pr.mergeSha", op.MergeSHA != np.MergeSHA)
		missed = append(missed, Missed{Event: eventPullRequest, Repo: repo})
	}
	if op.HeadSHA != np.HeadSHA {
		fields = append(fields, "pr.headSha")
		missed = append(missed, Missed{Event: eventPullRequest, Repo: repo})
	}
	if op.Draft != np.Draft {
		fields = append(fields, "pr.draft")
		missed = append(missed, Missed{Event: eventPullRequest, Repo: repo})
	}
	if op.Base != np.Base {
		fields = append(fields, "pr.base")
		missed = append(missed, Missed{Event: eventPullRequest, Repo: repo})
	}
	if len(fields) > 0 {
		obs = append(obs, Observation{Kind: KindPR, Fields: fields, Missed: missed})
	}

	of, nf := old.Facts, cur.Facts
	if of == nil {
		of = &v1alpha1.ChangeFacts{}
	}
	if nf == nil {
		nf = &v1alpha1.ChangeFacts{}
	}
	if o := diffChecks(KindCI, "ci.checks", of.Head, nf.Head, op.HeadSHA == np.HeadSHA, repo); o != nil {
		obs = append(obs, *o)
	}
	if !reflect.DeepEqual(reviewKeys(of.Reviews), reviewKeys(nf.Reviews)) {
		o := Observation{Kind: KindReview, Fields: []string{"review"}}
		for range newEntries(reviewKeys(of.Reviews), reviewKeys(nf.Reviews)) {
			o.Missed = append(o.Missed, Missed{Event: eventPullRequestReview, Repo: repo})
		}
		obs = append(obs, o)
	}
	sameMerge := op.MergeSHA == np.MergeSHA && np.MergeSHA != ""
	if o := diffChecks(KindMerge, "merge.checks", of.Merge, nf.Merge, sameMerge, repo); o != nil {
		obs = append(obs, *o)
	}
	return obs
}

func setChangeFields(st *v1alpha1.ChangeStatus) []string {
	pr := st.PR
	f := []string{"pr.state"}
	f = appendIf(f, "pr.headSha", pr.HeadSHA != "")
	f = appendIf(f, "pr.mergeSha", pr.MergeSHA != "")
	f = appendIf(f, "pr.draft", pr.Draft)
	if st.Facts != nil {
		f = appendIf(f, "ci.checks", len(st.Facts.Head) > 0)
		f = appendIf(f, "review", len(st.Facts.Reviews) > 0)
		f = appendIf(f, "merge.checks", len(st.Facts.Merge) > 0)
	}
	return f
}

// diffChecks diffs the checks and statuses of one commit. When the commit is the same, every check or
// status that appeared or changed is a missed check_run or status delivery; when it changed (a push or
// a merge), the push or merge is what was missed.
func diffChecks(kind, field string, old, cur []v1alpha1.CheckFact, sameCommit bool, repo string) *Observation {
	ok, nk := checkKeys(old), checkKeys(cur)
	if reflect.DeepEqual(ok, nk) {
		return nil
	}
	o := &Observation{Kind: kind, Fields: []string{field}}
	if sameCommit {
		for _, f := range cur {
			if ok[f.Kind+"/"+f.Name] == nk[f.Kind+"/"+f.Name] {
				continue
			}
			ev := eventCheckRun
			if f.Kind == FactStatus {
				ev = eventStatus
			}
			o.Missed = append(o.Missed, Missed{Event: ev, Repo: repo})
		}
	}
	return o
}

func checkKeys(fs []v1alpha1.CheckFact) map[string]string {
	m := map[string]string{}
	for _, f := range fs {
		m[f.Kind+"/"+f.Name] = f.Status + "/" + f.Conclusion
	}
	return m
}

func reviewKeys(rs []v1alpha1.ReviewFact) []string {
	var out []string
	for _, r := range rs {
		out = append(out, r.By+"/"+r.State)
	}
	return out
}

// newEntries are the entries of cur beyond those of old, as multisets.
func newEntries(old, cur []string) []string {
	count := map[string]int{}
	for _, o := range old {
		count[o]++
	}
	var out []string
	for _, c := range cur {
		if count[c] > 0 {
			count[c]--
			continue
		}
		out = append(out, c)
	}
	return out
}

// Rows are the history rows of a set of observations of one object: one observed row each, and one
// webhook.missed row per missed delivery when the source isn't a webhook and the repo is installed.
type Rows struct {
	Events []history.Event
	// Missed counts missed deliveries by event, for engine_webhook_missed_total.
	Missed map[string]int
}

// Subject names the object rows are about.
type Subject struct {
	Team, Item, Change string
}

// Record builds the rows of observations made at at by source. A webhook vouches only for the changes
// its own event announces: when source is webhook, events are the webhook events that asked for the
// reads, and a change another event would have announced is missed all the same (an issues delivery
// whose read finds the board status changed doesn't hide the missed projects_v2_item delivery).
func Record(obs []Observation, subj Subject, source string, events []string, at time.Time, installed func(repo string) bool) Rows {
	var rows Rows
	ref := subj.Item
	if subj.Change != "" {
		ref = subj.Change
	}
	vouched := Vouched(source, events)
	for _, o := range obs {
		rows.Events = append(rows.Events, history.Event{At: at, Team: subj.Team, Item: subj.Item, Change: subj.Change,
			Type: EventObserved, Source: source, Data: map[string]any{"kind": o.Kind, "fields": o.Fields}})
		for _, m := range o.Missed {
			if vouched(m.Event) || (installed != nil && !installed(m.Repo)) {
				continue
			}
			rows.Events = append(rows.Events, history.Event{At: at, Team: subj.Team, Item: subj.Item, Change: subj.Change,
				Type: EventMissed, Source: source, Data: map[string]any{"event": m.Event, "ref": ref}})
			if rows.Missed == nil {
				rows.Missed = map[string]int{}
			}
			rows.Missed[m.Event]++
		}
	}
	return rows
}

// Vouched returns whether a change that event would announce was announced by the reads' source: a
// webhook of that event (a check_suite delivery covers its check runs too). Other sources vouch for
// nothing, and so does a webhook source that names no event (a wake that asked for no reads of its own,
// merged with reads another source asked for).
func Vouched(source string, events []string) func(event string) bool {
	return func(event string) bool {
		if source != v1alpha1.SourceWebhook {
			return false
		}
		for _, e := range events {
			if e == event || (e == eventCheckSuite && event == eventCheckRun) {
				return true
			}
		}
		return false
	}
}

func appendIf(s []string, v string, ok bool) []string {
	if ok {
		return append(s, v)
	}
	return s
}

func intPtrEqual(a, b *int) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func strsEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

// symDiff returns the entries in exactly one of two sorted sets.
func symDiff(a, b []string) []string {
	in := map[string]int{}
	for _, x := range a {
		in[x] |= 1
	}
	for _, x := range b {
		in[x] |= 2
	}
	var out []string
	for x, m := range in {
		if m != 3 {
			out = append(out, x)
		}
	}
	sort.Strings(out)
	return out
}

func subRefs(s []v1alpha1.SubIssue) []string {
	var out []string
	for _, x := range s {
		out = append(out, x.Ref)
	}
	return out
}

func subStates(s []v1alpha1.SubIssue) string {
	var b []byte
	for _, x := range s {
		b = append(b, x.Ref+"="+x.State+"/"+x.Status+";"...)
	}
	return string(b)
}

func blockerRefs(s []v1alpha1.Blocker) []string {
	var out []string
	for _, x := range s {
		out = append(out, x.Ref)
	}
	return out
}

func blockerStates(s []v1alpha1.Blocker) string {
	var b []byte
	for _, x := range s {
		b = append(b, x.Ref+"="+x.State+";"...)
	}
	return string(b)
}
