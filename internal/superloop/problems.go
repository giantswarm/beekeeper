package superloop

import (
	"cmp"
	"crypto/sha256"
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/giantswarm/beekeeper/internal/findings"
)

const problemPullWait = 5 * time.Minute

var (
	deliveryProblemRef       = regexp.MustCompile(`https://github\.com/([\w.-]+/[\w.-]+)/(issues|pull)/(\d+)|\b([\w.-]+/[\w.-]+)#(\d+)\b`)
	deliveryGitHubURL        = regexp.MustCompile(`(?i)https?://github\.com/([\w.-]+/[\w.-]+)`)
	deliveryAnyURL           = regexp.MustCompile(`(?i)https?://[^\s)>\]"']+`)
	deliveryTokenPart        = regexp.MustCompile(`^[\w.-]+$`)
	deliveryMajorVersionPart = regexp.MustCompile(`^v\d+$`)
	workerContinuationSuffix = regexp.MustCompile(`(?:\s+next-[0-9a-f]+)+$`)
)

func progressKey(e Event) string {
	sum := sha256.Sum256([]byte(problemText(e)))
	return fmt.Sprintf("%s %x", eventKey(KindProgress, e), sum[:4])
}

func isReviewerAgent(name string) bool {
	return strings.HasPrefix(name, "Review ")
}

func baseWorkerName(name string) string {
	return strings.TrimSpace(workerContinuationSuffix.ReplaceAllString(strings.TrimSpace(name), ""))
}

func sameDeliveryAgent(a, b string) bool {
	if strings.EqualFold(a, b) {
		return true
	}
	if a == "" || b == "" || isReviewerAgent(a) || isReviewerAgent(b) {
		return false
	}
	return strings.EqualFold(baseWorkerName(a), baseWorkerName(b))
}

func matchDeliveryWorkers(workers []Worker, agent string) []Worker {
	if agent == "" || isReviewerAgent(agent) {
		return nil
	}
	var matched []Worker
	for _, w := range workers {
		if !isReviewerAgent(w.Name) && sameDeliveryAgent(w.Name, agent) {
			matched = append(matched, w)
		}
	}
	return matched
}

func normalizeProblemRef(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if strings.Contains(s, "://") {
		if m := deliveryProblemRef.FindStringSubmatch(s); m != nil && m[1] != "" {
			return strings.ToLower(m[1] + "#" + m[3])
		}
		return ""
	}
	if IsPull(s) {
		return strings.ToLower(s)
	}
	return ""
}

func problemMatchRef(match []string) (repo, kind, number string) {
	if match[1] != "" {
		return strings.ToLower(match[1]), match[2], match[3]
	}
	return strings.ToLower(match[4]), "", match[5]
}

func allProblemPulls(in Input) map[string]Pull {
	out := make(map[string]Pull, len(in.ProblemPulls)+len(in.Pulls))
	for rawRef, p := range in.ProblemPulls {
		if ref := normalizeProblemRef(cmp.Or(rawRef, p.Ref)); ref != "" {
			out[ref] = p
		}
	}
	for rawRef, p := range in.Pulls {
		if ref := normalizeProblemRef(cmp.Or(rawRef, p.Ref)); ref != "" {
			out[ref] = p
		}
	}
	return out
}

func pullReadAt(in Input, ref string) time.Time {
	if in.PullReadAt == nil {
		return time.Time{}
	}
	if at, ok := in.PullReadAt[ref]; ok && !at.IsZero() {
		return at
	}
	for k, at := range in.PullReadAt {
		if !at.IsZero() && strings.EqualFold(normalizeProblemRef(k), ref) {
			return at
		}
	}
	return time.Time{}
}

func closedProblemIssue(in Input, ref string) bool {
	if len(in.ClosedIssues) == 0 {
		return false
	}
	if in.ClosedIssues[ref] || in.ClosedIssues[strings.ToLower(ref)] {
		return true
	}
	for k, closed := range in.ClosedIssues {
		if closed && strings.EqualFold(normalizeProblemRef(k), ref) {
			return true
		}
	}
	return false
}

func problemAlreadyFiled(l ledger, agentEvents []Event, e Event, near func(a, b Event) bool, isPaired bool) bool {
	if l.has(findingKey(e)) {
		return true
	}
	if Filed(e.Detail) {
		return false
	}
	if isPaired {
		for _, d := range agentEvents {
			if d.Verb == VerbDone && near(d, e) && l.has(eventKey(KindDone, d)) {
				return true
			}
		}
		return false
	}
	if !l.has(eventKey(KindFile, e)) {
		return false
	}
	sameAt := 0
	for _, o := range agentEvents {
		if o.Verb == VerbProblem && o.At.Equal(e.At) && !Filed(o.Detail) {
			sameAt++
		}
	}
	return sameAt <= 1
}

// ownDeliveryProblem leaves progress in the original agents.problem timeline.
// Require positive ownership and an explicit subject; uncertainty still reaches
// the guide. Task mentions alone are not ownership of someone else's defect.
func ownDeliveryProblem(in Input, l ledger, e Event, isPaired bool) bool {
	matched := matchDeliveryWorkers(in.Workers, e.Agent)
	if len(matched) == 0 {
		return false
	}
	pulls := allProblemPulls(in)
	owned := map[string]bool{}
	for _, w := range matched {
		for _, raw := range slices.Concat([]string{w.Serves}, w.Issues, w.Pulls) {
			ref := normalizeProblemRef(raw)
			if ref == "" || closedProblemIssue(in, ref) {
				continue
			}
			if p, ok := pulls[ref]; ok && p.State != Open {
				continue
			}
			owned[ref] = true
		}
	}
	checkPrefixes := map[string]bool{}
	for ref, p := range pulls {
		if p.State != Open {
			continue
		}
		owns := owned[ref] || strings.EqualFold(baseWorkerName(e.Agent), PullWorkerName(ref))
		for _, w := range matched {
			owns = owns || normalizeProblemRef(w.Serves) == ref || ownsParkedPull(in, l, ref, w)
		}
		for _, issue := range PullIssues(ref, p) {
			owns = owns || owned[normalizeProblemRef(issue)]
		}
		if !owns {
			for _, t := range in.Taken {
				if normalizeProblemRef(t.Ref) == ref && sameDeliveryAgent(t.Agent, e.Agent) {
					owns = true
					break
				}
			}
		}
		if !owns {
			for _, ev := range in.Events {
				if ev.Verb != VerbPark || !sameDeliveryAgent(ev.Agent, e.Agent) {
					continue
				}
				if on, waits := ParkedOn(ev.Detail); ownsParkedPull(in, l, ref, Worker{Name: ev.Agent, ParkedOn: on, Waits: waits}) {
					owns = true
					break
				}
			}
		}
		if owns {
			owned[ref] = true
			for _, check := range p.Checks {
				recordCheckPrefix(checkPrefixes, check)
			}
			for _, red := range p.Red {
				recordCheckPrefix(checkPrefixes, red.Name)
			}
		}
	}
	body := problemText(e)
	refs := deliveryProblemRef.FindAllStringSubmatch(body, -1)
	if len(refs) == 0 {
		return false
	}
	ownedRepos := map[string]bool{}
	hasOwned := false
	for _, match := range refs {
		repo, _, number := problemMatchRef(match)
		if owned[repo+"#"+number] {
			hasOwned = true
			ownedRepos[repo] = true
		}
	}
	if !hasOwned {
		return false
	}
	repos := map[string]bool{}
	for _, match := range refs {
		repo, _, number := problemMatchRef(match)
		ref := repo + "#" + number
		if owned[ref] {
			repos[repo] = true
			continue
		}
		if isPaired || !ownedRepos[repo] || !terminalProblemContextRef(in, pulls, ref) {
			return false
		}
		repos[repo] = true
	}
	for _, match := range deliveryGitHubURL.FindAllStringSubmatch(body, -1) {
		if !repos[strings.ToLower(match[1])] {
			return false
		}
	}
	// A report can mention its own PR as evidence for another repository's
	// defect. Do not hide that finding, including a bare owner/repo reference.
	stripped := deliveryAnyURL.ReplaceAllString(deliveryProblemRef.ReplaceAllString(body, " "), " ")
	for _, field := range strings.Fields(stripped) {
		tok := strings.Trim(field, "()[]{}\"'`,;:!?")
		tok = strings.TrimRight(tok, ".")
		if i := strings.IndexByte(tok, ':'); i >= 0 {
			tok = tok[:i]
		}
		if strings.Count(tok, "/") != 1 {
			continue
		}
		owner, name, _ := strings.Cut(tok, "/")
		if !deliveryTokenPart.MatchString(owner) || !deliveryTokenPart.MatchString(name) {
			continue
		}
		if !hasAlpha(owner) || !hasAlpha(name) || isNonRepoSlashToken(owner, name, checkPrefixes) {
			continue
		}
		if !repos[strings.ToLower(owner+"/"+name)] {
			return false
		}
	}
	return true
}

func terminalProblemContextRef(in Input, pulls map[string]Pull, ref string) bool {
	if closedProblemIssue(in, ref) {
		return true
	}
	p, ok := pulls[ref]
	return ok && (p.State == Merged || p.State == Closed)
}

func workerTouchesRepo(in Input, matched []Worker, agent, ref string) bool {
	repo := repoOf(strings.ToLower(ref))
	if repo == "" {
		return false
	}
	if !isReviewerAgent(agent) && strings.EqualFold(baseWorkerName(agent), PullWorkerName(ref)) {
		return true
	}
	hasRepo := func(s string) bool {
		r := normalizeProblemRef(s)
		return r != "" && repoOf(r) == repo
	}
	for _, w := range matched {
		if hasRepo(w.Serves) || hasRepo(w.ParkedOn) ||
			slices.ContainsFunc(w.Issues, hasRepo) ||
			slices.ContainsFunc(w.Pulls, hasRepo) ||
			slices.ContainsFunc(w.Mentions, hasRepo) {
			return true
		}
	}
	for _, ev := range in.Events {
		if ev.Verb != VerbPark || !sameDeliveryAgent(ev.Agent, agent) {
			continue
		}
		if on, _ := ParkedOn(ev.Detail); hasRepo(on) {
			return true
		}
	}
	return false
}

func candidateProblemPullRefs(in Input, e Event) []string {
	matched := matchDeliveryWorkers(in.Workers, e.Agent)
	if len(matched) == 0 {
		return nil
	}
	var out []string
	for _, match := range deliveryProblemRef.FindAllStringSubmatch(problemText(e), -1) {
		repo, kind, number := problemMatchRef(match)
		if kind == "issues" {
			continue
		}
		ref := repo + "#" + number
		if closedProblemIssue(in, ref) {
			continue
		}
		if reason, _ := in.Desk.unread(ref); reason != "" {
			continue
		}
		if !workerTouchesRepo(in, matched, e.Agent, ref) {
			continue
		}
		if !slices.Contains(out, ref) {
			out = append(out, ref)
		}
	}
	return out
}

func problemAwaitsPull(in Input, l ledger, e Event, isPaired bool) bool {
	if in.PullReadAt == nil || e.Agent == "" || isReviewerAgent(e.Agent) {
		return false
	}
	if _, explicit := findings.Parse(problemText(e)); explicit {
		return false
	}
	if !in.Now.IsZero() && !e.At.IsZero() && in.Now.Sub(e.At) >= problemPullWait {
		return false
	}
	pulls := allProblemPulls(in)
	var unread []string
	for _, ref := range candidateProblemPullRefs(in, e) {
		if _, ok := pulls[ref]; ok {
			continue
		}
		if !pullReadAt(in, ref).IsZero() {
			continue
		}
		unread = append(unread, ref)
	}
	if len(unread) == 0 {
		return false
	}
	tryIn := in
	tryPulls := make(map[string]Pull, len(in.ProblemPulls)+len(unread))
	for k, v := range in.ProblemPulls {
		tryPulls[k] = v
	}
	for _, ref := range unread {
		tryPulls[ref] = Pull{Ref: ref, State: Open}
	}
	tryIn.ProblemPulls = tryPulls
	tryWorkers := make([]Worker, len(in.Workers))
	copy(tryWorkers, in.Workers)
	for i := range tryWorkers {
		if sameDeliveryAgent(tryWorkers[i].Name, e.Agent) {
			tryWorkers[i].Pulls = slices.Concat(tryWorkers[i].Pulls, unread)
		}
	}
	tryIn.Workers = tryWorkers
	return ownDeliveryProblem(tryIn, l, e, isPaired)
}

// UnsettledProblemPulls returns candidate pull request references (owner/repo#n)
// named in unsettled non-reviewer problem reports that the desk may read, so the
// pull watcher observes newly opened or referenced pull requests before Plan
// settles the report.
func UnsettledProblemPulls(in Input) []string {
	l := newLedger(in.Taken, in.Answers)
	eventsByAgent := make(map[string][]Event)
	for _, e := range in.Events {
		eventsByAgent[e.Agent] = append(eventsByAgent[e.Agent], e)
	}
	near := func(a, b Event) bool {
		d := a.At.Sub(b.At)
		return a.Agent == b.Agent && d > -pairWithin && d < pairWithin
	}
	var set map[string]bool
	for _, e := range in.Events {
		if e.Verb != VerbProblem || e.Agent == "" || isReviewerAgent(e.Agent) {
			continue
		}
		if _, explicit := findings.Parse(problemText(e)); explicit {
			continue
		}
		if l.has(progressKey(e)) || l.has(eventKey(KindProgress, e)) || l.has(findingKey(e)) {
			continue
		}
		isPaired := slices.ContainsFunc(eventsByAgent[e.Agent], func(d Event) bool { return d.Verb == VerbDone && near(d, e) })
		if problemAlreadyFiled(l, eventsByAgent[e.Agent], e, near, isPaired) {
			continue
		}
		for _, ref := range candidateProblemPullRefs(in, e) {
			if set == nil {
				set = map[string]bool{}
			}
			set[ref] = true
		}
	}
	if len(set) == 0 {
		return nil
	}
	out := make([]string, 0, len(set))
	for ref := range set {
		out = append(out, ref)
	}
	sort.Strings(out)
	return out
}

// PendingProblemAgents returns roster agent names whose unsettled problem or
// done reports are waiting for pull request observations, keeping their roster
// entries until the observation settles them.
func PendingProblemAgents(in Input) []string {
	if in.PullReadAt == nil {
		return nil
	}
	l := newLedger(in.Taken, in.Answers)
	eventsByAgent := make(map[string][]Event)
	for _, e := range in.Events {
		eventsByAgent[e.Agent] = append(eventsByAgent[e.Agent], e)
	}
	near := func(a, b Event) bool {
		d := a.At.Sub(b.At)
		return a.Agent == b.Agent && d > -pairWithin && d < pairWithin
	}
	var set map[string]bool
	for _, e := range in.Events {
		if e.Verb != VerbProblem || e.Agent == "" || isReviewerAgent(e.Agent) {
			continue
		}
		if l.has(progressKey(e)) || l.has(eventKey(KindProgress, e)) || l.has(findingKey(e)) {
			continue
		}
		isPaired := slices.ContainsFunc(eventsByAgent[e.Agent], func(d Event) bool { return d.Verb == VerbDone && near(d, e) })
		if problemAlreadyFiled(l, eventsByAgent[e.Agent], e, near, isPaired) {
			continue
		}
		if problemAwaitsPull(in, l, e, isPaired) {
			if set == nil {
				set = map[string]bool{}
			}
			set[e.Agent] = true
		}
	}
	if len(set) == 0 {
		return nil
	}
	out := make([]string, 0, len(set))
	for agent := range set {
		out = append(out, agent)
	}
	sort.Strings(out)
	return out
}

func isOwnPullCheckWait(waits string) bool {
	w := strings.ToLower(strings.TrimSpace(waits))
	if strings.Contains(w, "overlap") {
		return false
	}
	return w == "ci" || strings.HasPrefix(w, "ci ") || strings.HasPrefix(w, "ci:")
}

func ownsParkedPull(in Input, l ledger, ref string, w Worker) bool {
	if normalizeProblemRef(w.ParkedOn) != ref || isReviewerAgent(w.Name) {
		return false
	}
	if w.ReviewRequested || (w.Waits != "" && (ownPullWait(w.Waits) || isOwnPullCheckWait(w.Waits))) {
		return true
	}
	for r, owner := range l.reviewer {
		if normalizeProblemRef(r) == ref && owner != "" {
			return sameDeliveryAgent(owner, w.Name)
		}
	}
	if s := normalizeProblemRef(w.Serves); s != "" {
		return s == ref
	}
	if w.Waits != "" {
		return false
	}
	for _, other := range in.Workers {
		if sameDeliveryAgent(other.Name, w.Name) {
			continue
		}
		if (normalizeProblemRef(other.ParkedOn) == ref && other.ReviewRequested) ||
			normalizeProblemRef(other.Serves) == ref ||
			slices.ContainsFunc(other.Pulls, func(pr string) bool { return normalizeProblemRef(pr) == ref }) ||
			(!isReviewerAgent(other.Name) && strings.EqualFold(baseWorkerName(other.Name), PullWorkerName(ref))) {
			return false
		}
	}
	return true
}

func recordCheckPrefix(prefixes map[string]bool, name string) {
	prefix, _, ok := strings.Cut(strings.TrimSpace(name), "/")
	if !ok {
		return
	}
	prefix, _, _ = strings.Cut(prefix, ":")
	prefix = strings.ToLower(strings.TrimSpace(prefix))
	if prefix != "" {
		prefixes[prefix] = true
	}
}

func hasAlpha(s string) bool {
	for _, r := range s {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' {
			return true
		}
	}
	return false
}

func isNonRepoSlashToken(owner, name string, checkPrefixes map[string]bool) bool {
	lowOwner := strings.ToLower(owner)
	lowName := strings.ToLower(name)
	if checkPrefixes[lowOwner] {
		return true
	}
	switch lowOwner + "/" + lowName {
	case "i/o", "n/a", "w/o", "r/w", "r/o", "ci/cd", "tcp/ip",
		"os/exec", "os/signal", "os/user", "io/fs", "io/ioutil",
		"net/http", "net/url", "net/netip", "path/filepath",
		"encoding/json", "encoding/xml", "encoding/csv", "encoding/hex", "encoding/base64",
		"crypto/tls", "crypto/sha256", "crypto/rand", "crypto/x509",
		"sync/atomic", "runtime/debug", "runtime/pprof",
		"text/template", "html/template", "database/sql",
		"archive/tar", "archive/zip", "compress/gzip",
		"go/ast", "go/parser", "go/token", "go/types":
		return true
	}
	switch lowOwner {
	case "origin", "upstream", "heads", "tags", "refs", "remotes",
		"pre-commit", "ci", "batch-ci", "chart-tests",
		"check-readme-links", "check-values-schema",
		"gitleaks", "semantic-pull-request", "golangci-lint",
		"circleci", "github-actions", "dependabot", "renovate", "codecov",
		"cmd", "internal", "pkg", "src", "lib", "bin", "scripts", "hack",
		"test", "tests", "testdata", "docs", "doc", "charts", "crates",
		"regression", "recovery", "smoke", "merge-result", "duplicate-pr", "selfupdate":
		return true
	case "x":
		switch lowName {
		case "crypto", "net", "sys", "sync", "text", "tools", "mod",
			"exp", "time", "term", "oauth2", "image", "arch", "vuln", "telemetry":
			return true
		}
	case "yaml":
		if deliveryMajorVersionPart.MatchString(lowName) {
			return true
		}
	}
	switch strings.ToLower(filepath.Ext(name)) {
	case ".go", ".md", ".yaml", ".yml", ".json", ".toml", ".mod", ".sum",
		".sh", ".py", ".ts", ".tsx", ".js", ".jsx", ".css", ".html",
		".txt", ".sql", ".proto", ".lock", ".patch", ".cfg", ".ini", ".xml", ".csv",
		".rs", ".c", ".h", ".cc", ".cpp", ".hpp", ".java", ".kt", ".swift",
		".rb", ".php", ".lua", ".zig", ".nix", ".tf", ".hcl", ".mk",
		".vue", ".svelte", ".scss", ".graphql", ".gql", ".tmpl", ".gotmpl",
		".golden", ".dockerfile", ".env", ".conf", ".service", ".timer", ".socket":
		return true
	}
	return false
}

// A final report can contain several findings at the same timestamp.
func findingKey(e Event) string {
	sum := sha256.Sum256([]byte(problemText(e)))
	return fmt.Sprintf("%s %x", eventKey(KindFile, e), sum)
}
