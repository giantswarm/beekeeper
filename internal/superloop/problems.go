package superloop

import (
	"crypto/sha256"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
)

var (
	deliveryProblemRef = regexp.MustCompile(`https://github\.com/([\w.-]+/[\w.-]+)/(?:issues|pull)/(\d+)|\b([\w.-]+/[\w.-]+)#(\d+)\b`)
	deliveryGitHubURL  = regexp.MustCompile(`(?i)https?://github\.com/([\w.-]+/[\w.-]+)`)
	deliveryAnyURL     = regexp.MustCompile(`(?i)https?://[^\s)>\]"']+`)
	deliveryTokenPart  = regexp.MustCompile(`^[\w.-]+$`)
)

func progressKey(e Event) string {
	sum := sha256.Sum256([]byte(problemText(e)))
	return fmt.Sprintf("%s %x", eventKey(KindProgress, e), sum[:4])
}

// ownDeliveryProblem leaves progress in the original agents.problem timeline.
// Require positive ownership and an explicit subject; uncertainty still reaches
// the guide. Task mentions alone are not ownership of someone else's defect.
func ownDeliveryProblem(in Input, l ledger, e Event) bool {
	if e.Agent == "" {
		return false
	}
	var matched []Worker
	for _, w := range in.Workers {
		if strings.EqualFold(w.Name, e.Agent) {
			matched = append(matched, w)
		}
	}
	if len(matched) == 0 {
		return false
	}
	closedIssue := func(ref string) bool {
		return in.ClosedIssues[ref] || in.ClosedIssues[strings.ToLower(ref)]
	}
	owned := map[string]bool{}
	for _, w := range matched {
		for _, ref := range append([]string{w.Serves}, w.Issues...) {
			if ref == "" || closedIssue(ref) {
				continue
			}
			if p, ok := in.Pulls[ref]; ok && p.State != Open {
				continue
			}
			owned[strings.ToLower(ref)] = true
		}
	}
	checkPrefixes := map[string]bool{}
	for ref, p := range in.Pulls {
		if p.State != Open {
			continue
		}
		owns := strings.EqualFold(e.Agent, PullWorkerName(ref))
		for _, w := range matched {
			owns = owns || strings.EqualFold(w.Serves, ref) || ownsParkedPull(in, l, ref, w)
		}
		for _, issue := range p.Closes {
			owns = owns || owned[strings.ToLower(issue)]
		}
		if !owns {
			for _, t := range in.Taken {
				if strings.EqualFold(t.Ref, ref) && strings.EqualFold(t.Agent, e.Agent) {
					owns = true
					break
				}
			}
		}
		if !owns {
			for _, ev := range in.Events {
				if ev.Verb != VerbPark || !strings.EqualFold(ev.Agent, e.Agent) {
					continue
				}
				if on, waits := ParkedOn(ev.Detail); ownsParkedPull(in, l, ref, Worker{Name: e.Agent, ParkedOn: on, Waits: waits}) {
					owns = true
					break
				}
			}
		}
		if owns {
			owned[strings.ToLower(ref)] = true
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
	repos := map[string]bool{}
	for _, match := range refs {
		repo, number := match[1], match[2]
		if repo == "" {
			repo, number = match[3], match[4]
		}
		repo = strings.ToLower(repo)
		if !owned[repo+"#"+number] {
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

func ownsParkedPull(in Input, l ledger, ref string, w Worker) bool {
	if !strings.EqualFold(w.ParkedOn, ref) || strings.HasPrefix(w.Name, "Review ") {
		return false
	}
	if w.ReviewRequested || (w.Waits != "" && ownPullWait(w.Waits)) {
		return true
	}
	if owner := l.reviewer[ref]; owner != "" {
		return strings.EqualFold(owner, w.Name)
	}
	if w.Serves != "" {
		return strings.EqualFold(w.Serves, ref)
	}
	if w.Waits != "" {
		return false
	}
	for _, other := range in.Workers {
		if strings.EqualFold(other.Name, w.Name) {
			continue
		}
		if (strings.EqualFold(other.ParkedOn, ref) && other.ReviewRequested) ||
			strings.EqualFold(other.Serves, ref) ||
			strings.EqualFold(other.Name, PullWorkerName(ref)) {
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
	if checkPrefixes[lowOwner] {
		return true
	}
	switch lowOwner {
	case "origin", "upstream", "heads", "tags",
		"pre-commit", "ci", "batch-ci", "chart-tests",
		"check-readme-links", "check-values-schema",
		"gitleaks", "semantic-pull-request", "golangci-lint":
		return true
	}
	switch strings.ToLower(filepath.Ext(name)) {
	case ".go", ".md", ".yaml", ".yml", ".json", ".toml", ".mod", ".sum",
		".sh", ".py", ".ts", ".tsx", ".js", ".jsx", ".css", ".html",
		".txt", ".sql", ".proto", ".lock", ".patch", ".cfg", ".ini", ".xml", ".csv":
		return true
	}
	return false
}

// A final report can contain several findings at the same timestamp.
func findingKey(e Event) string {
	sum := sha256.Sum256([]byte(problemText(e)))
	return fmt.Sprintf("%s %x", eventKey(KindFile, e), sum)
}
