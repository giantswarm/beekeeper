//go:build scenario

package fakegithub

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

// The scenario's steps and expectations of its GitHub: the github step
// (what happens on GitHub: a push, a check's result, a review, a merge, a
// close, a comment, a budget), the fault step (arm or clear a fault), and
// the github expectation (the calls it saw, what callers spent, and the
// facts of a pull request or an issue).

// Step is a github step: exactly one of its actions.
type Step struct {
	Push    *PushStep              `yaml:"push"`
	Check   *CheckStep             `yaml:"check"`
	Review  *ReviewStep            `yaml:"review"`
	Merge   *MergeStep             `yaml:"merge"`
	Close   *CloseStep             `yaml:"close"`
	Comment *CommentStep           `yaml:"comment"`
	Release *ReleaseStep           `yaml:"release"`
	Budget  map[string]GivenBudget `yaml:"budget"`
}

// PushStep commits one file onto a branch of a repository's origin; a
// new branch starts at From (the default branch).
type PushStep struct {
	Repo    string `yaml:"repo"`
	Branch  string `yaml:"branch"`
	From    string `yaml:"from"`
	File    string `yaml:"file"`
	Content string `yaml:"content"`
	Message string `yaml:"message"`
}

// CheckStep sets the check run Name on a head to pending, green or red:
// the head of pull request Pull, or Head, a branch or a commit.
type CheckStep struct {
	Repo     string `yaml:"repo"`
	Pull     int    `yaml:"pull"`
	Head     string `yaml:"head"`
	Name     string `yaml:"name"`
	Workflow string `yaml:"workflow"`
	State    string `yaml:"state"`
}

// ReviewStep submits a review of a pull request's head: approved,
// changes_requested or commented, by Author (reviewer).
type ReviewStep struct {
	Repo   string `yaml:"repo"`
	Pull   int    `yaml:"pull"`
	State  string `yaml:"state"`
	Author string `yaml:"author"`
	Body   string `yaml:"body"`
}

// MergeStep merges a pull request as GitHub's merge button does (squash
// by default), whatever its base's protection says.
type MergeStep struct {
	Repo   string `yaml:"repo"`
	Pull   int    `yaml:"pull"`
	Method string `yaml:"method"`
}

// CloseStep closes an issue or a pull request (exactly one).
type CloseStep struct {
	Repo  string `yaml:"repo"`
	Issue int    `yaml:"issue"`
	Pull  int    `yaml:"pull"`
}

// CommentStep comments on an issue or a pull request (exactly one).
type CommentStep struct {
	Repo   string `yaml:"repo"`
	Issue  int    `yaml:"issue"`
	Pull   int    `yaml:"pull"`
	Author string `yaml:"author"`
	Body   string `yaml:"body"`
}

// ReleaseStep publishes release Tag of a repository at Target, a branch
// or a commit (the default branch), and tags Target with it as GitHub's
// release does for a new tag.
type ReleaseStep struct {
	Repo   string `yaml:"repo"`
	Tag    string `yaml:"tag"`
	Target string `yaml:"target"`
}

// Kind is the step's action, "" unless exactly one is set.
func (s *Step) Kind() string {
	var kinds []string
	for name, set := range map[string]bool{
		"push": s.Push != nil, "check": s.Check != nil, "review": s.Review != nil, "merge": s.Merge != nil,
		"close": s.Close != nil, "comment": s.Comment != nil, "release": s.Release != nil, "budget": len(s.Budget) > 0,
	} {
		if set {
			kinds = append(kinds, name)
		}
	}
	if len(kinds) != 1 {
		return ""
	}
	return kinds[0]
}

var repoNameRE = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

func needRepo(repo string) error {
	if !repoNameRE.MatchString(repo) {
		return fmt.Errorf("repo %q is not owner/name", repo)
	}
	return nil
}

func oneOf(issue, pull int) error {
	if (issue > 0) == (pull > 0) {
		return errors.New("exactly one of issue and pull")
	}
	return nil
}

// Validate checks the step without the model.
func (s *Step) Validate() error {
	var errs []error
	switch s.Kind() {
	case "":
		return errors.New("exactly one of push, check, review, merge, close, comment, release and budget")
	case "push":
		errs = append(errs, needRepo(s.Push.Repo))
		if s.Push.Branch == "" {
			errs = append(errs, errors.New("push without a branch"))
		}
	case "check":
		c := s.Check
		errs = append(errs, needRepo(c.Repo))
		if (c.Pull > 0) == (c.Head != "") {
			errs = append(errs, errors.New("check: exactly one of pull and head"))
		}
		if _, ok := checkStates[strings.ToLower(c.State)]; !ok || c.Name == "" {
			errs = append(errs, fmt.Errorf("check: a name and a state of queued, pending, green or red (has %q, %q)", c.Name, c.State))
		}
	case "review":
		r := s.Review
		errs = append(errs, needRepo(r.Repo))
		if _, ok := reviewStates[strings.ToLower(r.State)]; !ok || r.Pull <= 0 {
			errs = append(errs, fmt.Errorf("review: a pull and a state of approved, changes_requested or commented (has %q)", r.State))
		}
	case "merge":
		errs = append(errs, needRepo(s.Merge.Repo))
		if s.Merge.Pull <= 0 {
			errs = append(errs, errors.New("merge without a pull"))
		}
		if m := strings.ToLower(s.Merge.Method); m != "" && m != "merge" && m != "squash" && m != "rebase" {
			errs = append(errs, fmt.Errorf("merge: method %q is none of merge, squash, rebase", s.Merge.Method))
		}
	case "close":
		errs = append(errs, needRepo(s.Close.Repo), oneOf(s.Close.Issue, s.Close.Pull))
	case "comment":
		errs = append(errs, needRepo(s.Comment.Repo), oneOf(s.Comment.Issue, s.Comment.Pull))
		if s.Comment.Body == "" {
			errs = append(errs, errors.New("comment without a body"))
		}
	case "release":
		errs = append(errs, needRepo(s.Release.Repo))
		if s.Release.Tag == "" {
			errs = append(errs, errors.New("release without a tag"))
		}
	case "budget":
		for name := range s.Budget {
			if !slices.Contains(Buckets, name) {
				errs = append(errs, fmt.Errorf("budget: no bucket %q (%s)", name, strings.Join(Buckets, ", ")))
			}
		}
	}
	return errors.Join(errs...)
}

func (m *Model) repoOf(name string) (*Repo, error) {
	r, ok := m.Repo(name)
	if !ok {
		return nil, fmt.Errorf("%s is no repository of the scenario", name)
	}
	return r, nil
}

func (m *Model) pullOf(repo string, number int) (*Pull, error) {
	r, err := m.repoOf(repo)
	if err != nil {
		return nil, err
	}
	p, ok := r.Pull(number)
	if !ok {
		return nil, fmt.Errorf("%s#%d is no pull request", repo, number)
	}
	return p, nil
}

func (m *Model) issueOf(repo string, number int) (*Issue, error) {
	r, err := m.repoOf(repo)
	if err != nil {
		return nil, err
	}
	i, ok := r.Issue(number)
	if !ok {
		return nil, fmt.Errorf("%s#%d is no issue", repo, number)
	}
	return i, nil
}

// Run runs a github step.
func (s *Server) Run(st *Step) error {
	if st.Kind() == "budget" {
		for name, spec := range st.Budget {
			if err := s.SetBudget(name, spec.spec()); err != nil {
				return err
			}
		}
		return nil
	}
	return s.Model(func(m *Model) error {
		switch st.Kind() {
		case "push":
			p := st.Push
			r, err := m.repoOf(p.Repo)
			if err != nil {
				return err
			}
			if !m.Origins.Has(r) {
				return fmt.Errorf("push: %s has no origin", p.Repo)
			}
			file := cmpOr(p.File, ".scenario/"+p.Branch)
			content := cmpOr(p.Content, m.now().Format("2006-01-02T15:04:05.000000000Z07:00")+"\n")
			_, err = m.Origins.Push(r, p.Branch, cmpOr(p.From, r.DefaultBranch), file, content, cmpOr(p.Message, "Update "+file))
			r.PushedAt = m.now()
			return err
		case "check":
			c := st.Check
			r, err := m.repoOf(c.Repo)
			if err != nil {
				return err
			}
			sha, branch := "", ""
			if c.Pull > 0 {
				p, err := m.pullOf(c.Repo, c.Pull)
				if err != nil {
					return err
				}
				sha, branch = m.HeadSHA(p), p.Head
			} else {
				resolved, ok := m.Commit(r, c.Head)
				if !ok {
					return fmt.Errorf("check: %s is no branch or commit of %s", c.Head, c.Repo)
				}
				sha = resolved
				if _, isBranch := m.Branch(r, c.Head); isBranch {
					branch = c.Head
				}
			}
			if sha == "" {
				return fmt.Errorf("check: %s#%d has no head", c.Repo, c.Pull)
			}
			state := checkStates[strings.ToLower(c.State)]
			m.SetCheck(r, sha, CheckSpec{Name: c.Name, Workflow: c.Workflow, Status: state[0], Conclusion: state[1], Branch: branch})
			return nil
		case "review":
			rv := st.Review
			p, err := m.pullOf(rv.Repo, rv.Pull)
			if err != nil {
				return err
			}
			m.AddReview(p, cmpOr(rv.Author, "reviewer"), reviewStates[strings.ToLower(rv.State)], rv.Body, "")
			return nil
		case "merge":
			p, err := m.pullOf(st.Merge.Repo, st.Merge.Pull)
			if err != nil {
				return err
			}
			return m.Merge(p, MergeSpec{Method: cmpOr(st.Merge.Method, "squash"), Force: true})
		case "close":
			c := st.Close
			if c.Pull > 0 {
				p, err := m.pullOf(c.Repo, c.Pull)
				if err != nil {
					return err
				}
				m.ClosePull(p)
				return nil
			}
			i, err := m.issueOf(c.Repo, c.Issue)
			if err != nil {
				return err
			}
			m.CloseIssue(i, "")
			return nil
		case "comment":
			c := st.Comment
			if c.Pull > 0 {
				p, err := m.pullOf(c.Repo, c.Pull)
				if err != nil {
					return err
				}
				m.AddComment(nil, p, c.Author, c.Body)
				return nil
			}
			i, err := m.issueOf(c.Repo, c.Issue)
			if err != nil {
				return err
			}
			m.AddComment(i, nil, c.Author, c.Body)
			return nil
		case "release":
			rs := st.Release
			r, err := m.repoOf(rs.Repo)
			if err != nil {
				return err
			}
			target := cmpOr(rs.Target, r.DefaultBranch)
			sha, ok := m.Commit(r, target)
			if !ok {
				return fmt.Errorf("release: %s is no branch or commit of %s", target, rs.Repo)
			}
			if _, tagged := m.Tag(r, rs.Tag); tagged {
				return fmt.Errorf("release: %s of %s exists", rs.Tag, rs.Repo)
			}
			if _, err := m.AddRelease(r, Release{Tag: rs.Tag, Name: rs.Tag, Target: target}); err != nil {
				return err
			}
			return m.SetRef(r, "refs/tags/"+rs.Tag, sha)
		}
		return errors.New("a github step with exactly one action")
	})
}

// FaultStep is a fault step: it arms a fault, or clears the one of a name
// ("all" for every fault).
type FaultStep struct {
	Arm   *Fault `yaml:"arm"`
	Clear string `yaml:"clear"`
}

// Validate checks the step.
func (f *FaultStep) Validate() error {
	switch {
	case (f.Arm != nil) == (f.Clear != ""):
		return errors.New("exactly one of arm and clear")
	case f.Arm != nil:
		return f.Arm.Validate()
	}
	return nil
}

// RunFault runs a fault step.
func (s *Server) RunFault(f *FaultStep) error {
	if f.Arm != nil {
		return s.Arm(*f.Arm)
	}
	return s.Clear(f.Clear)
}

// Range bounds a number: a plain number is that number exactly, a map
// names min and max.
type Range struct {
	Equals *int
	Min    *int `yaml:"min"`
	Max    *int `yaml:"max"`
}

// UnmarshalYAML reads a number or {min, max}.
func (r *Range) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		var v int
		if err := n.Decode(&v); err != nil {
			return err
		}
		r.Equals = &v
		return nil
	}
	if n.Kind != yaml.MappingNode {
		return fmt.Errorf("line %d: a number, or min and max", n.Line)
	}
	// The scenario's decoder refuses unknown keys; so does this one.
	for i := 0; i+1 < len(n.Content); i += 2 {
		var v int
		if err := n.Content[i+1].Decode(&v); err != nil {
			return err
		}
		switch n.Content[i].Value {
		case "min":
			r.Min = &v
		case "max":
			r.Max = &v
		default:
			return fmt.Errorf("line %d: field %s not found in a range of min and max", n.Content[i].Line, n.Content[i].Value)
		}
	}
	if r.Min == nil && r.Max == nil {
		return fmt.Errorf("line %d: a number, or min and max", n.Line)
	}
	return nil
}

// check says how got misses the range, nil when it holds.
func (r *Range) check(what string, got int) error {
	switch {
	case r.Equals != nil && got != *r.Equals:
		return fmt.Errorf("%s: %d, want %d", what, got, *r.Equals)
	case r.Min != nil && got < *r.Min:
		return fmt.Errorf("%s: %d, at least %d", what, got, *r.Min)
	case r.Max != nil && got > *r.Max:
		return fmt.Errorf("%s: %d, at most %d", what, got, *r.Max)
	}
	return nil
}

// Expect is a github expectation: exactly one of its kinds.
type Expect struct {
	Calls  *CallsExpect  `yaml:"calls"`
	Budget *BudgetExpect `yaml:"budget"`
	Pull   *PullExpect   `yaml:"pull"`
	Issue  *IssueExpect  `yaml:"issue"`
}

// CallsExpect counts the calls GitHub saw that every field set holds.
// Method, Path, Operation and Caller are regular expressions that match
// whole, as a fault's match reads them.
type CallsExpect struct {
	Method      string `yaml:"method"`
	Path        string `yaml:"path"`
	Operation   string `yaml:"operation"`
	Caller      string `yaml:"caller"`
	Via         string `yaml:"via"`
	Status      *int   `yaml:"status"`
	Conditional *bool  `yaml:"conditional"`
	// Fault picks the calls a fault of that name answered or delayed.
	Fault string `yaml:"fault"`
	Min   *int   `yaml:"min"`
	Max   *int   `yaml:"max"`
}

// BudgetExpect checks a bucket: what Caller spent of it (every caller
// when empty), and what remains of it.
type BudgetExpect struct {
	Bucket    string `yaml:"bucket"`
	Caller    string `yaml:"caller"`
	Spent     *Range `yaml:"spent"`
	Remaining *Range `yaml:"remaining"`
}

// PullExpect checks a pull request: the one of Number, or the newest of
// the branch Head.
type PullExpect struct {
	Repo   string `yaml:"repo"`
	Number int    `yaml:"number"`
	Head   string `yaml:"head"`
	// State is open, closed or merged.
	State  string   `yaml:"state"`
	Draft  *bool    `yaml:"draft"`
	Labels []string `yaml:"labels"`
	// Checks is the rollup of its head: none, pending, green or red.
	Checks string `yaml:"checks"`
	// ReviewDecision is approved, changes_requested, review_required or
	// none.
	ReviewDecision string `yaml:"review_decision"`
	// MergedInto says its merge commit is the head of that branch.
	MergedInto string `yaml:"merged_into"`
	Comments   *Range `yaml:"comments"`
	// Ahead is how many commits the head of an open pull request has
	// beyond its base: what its branch adds, read from the origin.
	Ahead *int `yaml:"ahead"`
}

// IssueExpect checks an issue.
type IssueExpect struct {
	Repo   string `yaml:"repo"`
	Number int    `yaml:"number"`
	// State is open or closed.
	State  string   `yaml:"state"`
	Labels []string `yaml:"labels"`
	// Board is its Status on the scenario's board; "none" for an issue
	// that is not on it. Archived says whether its item there is archived.
	Board    string `yaml:"board"`
	Archived *bool  `yaml:"archived"`
	Comments *Range `yaml:"comments"`
	// OpenPulls and Pulls count the pull requests that close it: the open
	// ones, and all of them.
	OpenPulls *Range `yaml:"open_pulls"`
	Pulls     *Range `yaml:"pulls"`
}

// Kind is the expectation's kind, "" unless exactly one is set.
func (e *Expect) Kind() string {
	var kinds []string
	for name, set := range map[string]bool{"calls": e.Calls != nil, "budget": e.Budget != nil, "pull": e.Pull != nil, "issue": e.Issue != nil} {
		if set {
			kinds = append(kinds, name)
		}
	}
	if len(kinds) != 1 {
		return ""
	}
	return kinds[0]
}

// Validate checks the expectation without the model.
func (e *Expect) Validate() error {
	var errs []error
	switch e.Kind() {
	case "":
		return errors.New("exactly one of calls, budget, pull and issue")
	case "calls":
		c := e.Calls
		for field, pattern := range map[string]string{"method": c.Method, "path": c.Path, "operation": c.Operation, "caller": c.Caller} {
			if _, err := whole(pattern); err != nil {
				errs = append(errs, fmt.Errorf("calls.%s %q: %w", field, pattern, err))
			}
		}
		if c.Via != "" && c.Via != viaGH && c.Via != viaHTTP {
			errs = append(errs, fmt.Errorf("calls.via %q: %s or %s", c.Via, viaGH, viaHTTP))
		}
		if c.Min == nil && c.Max == nil {
			errs = append(errs, errors.New("calls without min or max"))
		}
	case "budget":
		b := e.Budget
		if !slices.Contains(Buckets, b.Bucket) {
			errs = append(errs, fmt.Errorf("budget: bucket %q is none of %s", b.Bucket, strings.Join(Buckets, ", ")))
		}
		if b.Spent == nil && b.Remaining == nil {
			errs = append(errs, errors.New("budget without spent or remaining"))
		}
		if b.Caller != "" && b.Spent == nil {
			errs = append(errs, errors.New("budget: caller is for spent"))
		}
	case "pull":
		p := e.Pull
		errs = append(errs, needRepo(p.Repo))
		if (p.Number > 0) == (p.Head != "") {
			errs = append(errs, errors.New("pull: exactly one of number and head"))
		}
		if p.State != "" && !slices.Contains([]string{"open", "closed", "merged"}, p.State) {
			errs = append(errs, fmt.Errorf("pull: state %q is none of open, closed, merged", p.State))
		}
		if p.Checks != "" && !slices.Contains([]string{"none", "pending", "green", "red"}, p.Checks) {
			errs = append(errs, fmt.Errorf("pull: checks %q is none of none, pending, green, red", p.Checks))
		}
		if p.ReviewDecision != "" && !slices.Contains([]string{"none", "approved", "changes_requested", "review_required"}, p.ReviewDecision) {
			errs = append(errs, fmt.Errorf("pull: review_decision %q is none of none, approved, changes_requested, review_required", p.ReviewDecision))
		}
		if p.Ahead != nil && (*p.Ahead < 0 || (p.State != "" && p.State != "open")) {
			errs = append(errs, errors.New("pull: ahead counts the commits of an open pull request, at least 0"))
		}
		if p.State == "" && p.Draft == nil && p.Labels == nil && p.Checks == "" && p.ReviewDecision == "" && p.MergedInto == "" && p.Comments == nil && p.Ahead == nil {
			errs = append(errs, errors.New("pull without a fact to check"))
		}
	case "issue":
		i := e.Issue
		errs = append(errs, needRepo(i.Repo))
		if i.Number <= 0 {
			errs = append(errs, errors.New("issue without a number"))
		}
		if i.State != "" && i.State != "open" && i.State != "closed" {
			errs = append(errs, fmt.Errorf("issue: state %q is neither open nor closed", i.State))
		}
		if i.State == "" && i.Labels == nil && i.Board == "" && i.Archived == nil && i.Comments == nil && i.OpenPulls == nil && i.Pulls == nil {
			errs = append(errs, errors.New("issue without a fact to check"))
		}
	}
	return errors.Join(errs...)
}

// matches says whether a call is one the expectation counts.
func (c *CallsExpect) matches(call Call) bool {
	for _, pair := range [][2]string{{c.Method, call.Method}, {c.Path, call.Path}, {c.Caller, call.Caller}} {
		if pair[0] == "" {
			continue
		}
		re, err := whole(pair[0])
		if err != nil || !re.MatchString(pair[1]) {
			return false
		}
	}
	if c.Operation != "" {
		re, err := whole(c.Operation)
		if err != nil {
			return false
		}
		if !(call.Operation != "" && re.MatchString(call.Operation)) && !slices.ContainsFunc(call.Fields, re.MatchString) {
			return false
		}
	}
	switch {
	case c.Via != "" && c.Via != call.Via:
		return false
	case c.Status != nil && *c.Status != call.Status:
		return false
	case c.Conditional != nil && *c.Conditional != call.Conditional:
		return false
	case c.Fault != "" && c.Fault != call.Fault:
		return false
	}
	return true
}

func (c *CallsExpect) describe() string {
	var parts []string
	for name, v := range map[string]string{"method": c.Method, "path": c.Path, "operation": c.Operation, "caller": c.Caller, "via": c.Via, "fault": c.Fault} {
		if v != "" {
			parts = append(parts, name+" "+v)
		}
	}
	if c.Status != nil {
		parts = append(parts, fmt.Sprintf("status %d", *c.Status))
	}
	if c.Conditional != nil {
		parts = append(parts, fmt.Sprintf("conditional %t", *c.Conditional))
	}
	slices.Sort(parts)
	if len(parts) == 0 {
		return "any call"
	}
	return strings.Join(parts, ", ")
}

func sameLabels(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	return hasAll(got, want)
}

// Check checks a github expectation against the server now.
func (s *Server) Check(e *Expect) error {
	switch e.Kind() {
	case "calls":
		c := e.Calls
		calls := s.Calls()
		n := 0
		var seen []string
		for _, call := range calls {
			if c.matches(call) {
				n++
			}
			seen = append(seen, call.Line())
		}
		if len(seen) > 15 {
			seen = append([]string{fmt.Sprintf("… the last 15 of %d:", len(seen))}, seen[len(seen)-15:]...)
		}
		switch {
		case c.Min != nil && n < *c.Min:
			return fmt.Errorf("github calls with %s: %d, at least %d; GitHub saw:\n  %s", c.describe(), n, *c.Min, cmpOr(strings.Join(seen, "\n  "), "no call"))
		case c.Max != nil && n > *c.Max:
			return fmt.Errorf("github calls with %s: %d, at most %d; GitHub saw:\n  %s", c.describe(), n, *c.Max, strings.Join(seen, "\n  "))
		}
		return nil
	case "budget":
		b := e.Budget
		var errs []error
		if b.Spent != nil {
			spent := 0
			var by []string
			for _, sp := range s.Spent() {
				if sp.Bucket != b.Bucket {
					continue
				}
				by = append(by, fmt.Sprintf("%s %d", cmpOr(sp.Caller, "nobody"), sp.Points))
				if b.Caller == "" || b.Caller == sp.Caller {
					spent += sp.Points
				}
			}
			who := cmpOr(b.Caller, "every caller")
			if err := b.Spent.check(fmt.Sprintf("github budget %s spent by %s", b.Bucket, who), spent); err != nil {
				errs = append(errs, fmt.Errorf("%w (spent: %s)", err, cmpOr(strings.Join(by, ", "), "nothing")))
			}
		}
		if b.Remaining != nil {
			state, _ := s.Bucket(b.Bucket)
			if err := b.Remaining.check(fmt.Sprintf("github budget %s remaining", b.Bucket), state.Remaining); err != nil {
				errs = append(errs, err)
			}
		}
		return errors.Join(errs...)
	case "pull":
		return s.Model(func(m *Model) error { return m.checkPull(e.Pull) })
	case "issue":
		return s.Model(func(m *Model) error { return m.checkIssue(e.Issue) })
	}
	return errors.New("a github expectation with exactly one kind")
}

func (m *Model) checkPull(want *PullExpect) error {
	r, err := m.repoOf(want.Repo)
	if err != nil {
		return err
	}
	var p *Pull
	if want.Number > 0 {
		if p, err = m.pullOf(want.Repo, want.Number); err != nil {
			return err
		}
	} else {
		for _, candidate := range r.Pulls {
			if candidate.Head == want.Head {
				p = candidate
			}
		}
		if p == nil {
			return fmt.Errorf("%s has no pull request of branch %s", want.Repo, want.Head)
		}
	}
	var errs []error
	bad := func(format string, args ...any) { errs = append(errs, fmt.Errorf(p.Ref()+": "+format, args...)) }
	if want.State != "" && !strings.EqualFold(want.State, p.State) {
		bad("state %s, want %s", strings.ToLower(p.State), want.State)
	}
	if want.Draft != nil && *want.Draft != p.Draft {
		bad("draft %t, want %t", p.Draft, *want.Draft)
	}
	if want.Labels != nil && !sameLabels(p.Labels, want.Labels) {
		bad("labels %v, want %v", p.Labels, want.Labels)
	}
	if want.Checks != "" {
		got := map[string]string{"": "none", statusPending: "pending", statusSuccess: "green", statusFailure: "red"}[r.Rollup(m.HeadSHA(p))]
		if got != want.Checks {
			bad("checks %s, want %s", got, want.Checks)
		}
	}
	if want.ReviewDecision != "" {
		got := cmpOr(strings.ToLower(p.ReviewDecision()), "none")
		if got != want.ReviewDecision {
			bad("review decision %s, want %s", got, want.ReviewDecision)
		}
	}
	if want.MergedInto != "" {
		head, _ := m.Branch(r, want.MergedInto)
		if p.State != stateMerged || p.MergeCommit == "" || p.MergeCommit != head {
			bad("merge commit %q is not the head %q of %s (state %s)", p.MergeCommit, head, want.MergedInto, strings.ToLower(p.State))
		}
	}
	if want.Comments != nil {
		if err := want.Comments.check(p.Ref()+" comments", len(p.Comments)); err != nil {
			errs = append(errs, err)
		}
	}
	if want.Ahead != nil {
		switch {
		case p.State != stateOpen:
			bad("state %s, ahead is an open pull request's", strings.ToLower(p.State))
		case !m.Origins.Has(p.Repo):
			bad("ahead: %s has no origin", want.Repo)
		default:
			ahead, _, err := m.Origins.AheadBehind(p.Repo, m.BaseSHA(p), m.HeadSHA(p))
			if err != nil {
				bad("ahead: %v", err)
			} else if ahead != *want.Ahead {
				bad("head %d commit(s) ahead of %s, want %d", ahead, p.Base, *want.Ahead)
			}
		}
	}
	return errors.Join(errs...)
}

func (m *Model) checkIssue(want *IssueExpect) error {
	i, err := m.issueOf(want.Repo, want.Number)
	if err != nil {
		return err
	}
	var errs []error
	bad := func(format string, args ...any) { errs = append(errs, fmt.Errorf(i.Ref()+": "+format, args...)) }
	if want.State != "" && !strings.EqualFold(want.State, i.State) {
		bad("state %s, want %s", strings.ToLower(i.State), want.State)
	}
	if want.Labels != nil && !sameLabels(i.Labels, want.Labels) {
		bad("labels %v, want %v", i.Labels, want.Labels)
	}
	if want.Board != "" {
		got := "none"
		for _, it := range m.Items(i, nil) {
			if it.Archived {
				continue
			}
			got = ""
			if v, ok := it.Value(statusField); ok {
				got = v.Option
			}
			break
		}
		if !strings.EqualFold(got, want.Board) {
			bad("board status %q, want %q", got, want.Board)
		}
	}
	if want.Archived != nil {
		items := m.Items(i, nil)
		switch {
		case len(items) == 0:
			bad("not on the board, want an item archived %t", *want.Archived)
		case items[0].Archived != *want.Archived:
			bad("board item archived %t, want %t", items[0].Archived, *want.Archived)
		}
	}
	if want.Comments != nil {
		if err := want.Comments.check(i.Ref()+" comments", len(i.Comments)); err != nil {
			errs = append(errs, err)
		}
	}
	pulls := m.ClosedByPulls(i)
	if want.Pulls != nil {
		if err := want.Pulls.check(i.Ref()+" pull requests that close it", len(pulls)); err != nil {
			errs = append(errs, err)
		}
	}
	if want.OpenPulls != nil {
		open := 0
		for _, p := range pulls {
			if p.State == stateOpen {
				open++
			}
		}
		if err := want.OpenPulls.check(i.Ref()+" open pull requests that close it", open); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
