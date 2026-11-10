package superloop

import (
	"regexp"
	"strings"
)

var deliveryProblemRef = regexp.MustCompile(`https://github\.com/([\w.-]+/[\w.-]+)/(?:issues|pull)/(\d+)|\b([\w.-]+/[\w.-]+)#(\d+)\b`)
var deliveryProblemRepo = regexp.MustCompile(`\b[\w.-]+/[\w.-]+\b`)

// ownDeliveryProblem leaves progress in the original agents.problem timeline.
// Require positive ownership and an explicit subject; uncertainty still reaches
// the guide. Task mentions alone are not ownership of someone else's defect.
func ownDeliveryProblem(in Input, w Worker, e Event) bool {
	if w.Name == "" || w.Saved {
		return false
	}
	owned := map[string]bool{}
	for _, ref := range append([]string{w.Serves}, w.Issues...) {
		if ref == "" || in.ClosedIssues[ref] {
			continue
		}
		if p, ok := in.Pulls[ref]; ok && p.State != Open {
			continue
		}
		owned[strings.ToLower(ref)] = true
	}
	for ref, p := range in.Pulls {
		if p.State != Open {
			continue
		}
		owns := strings.EqualFold(w.ParkedOn, ref) || strings.EqualFold(w.Serves, ref)
		for _, issue := range p.Closes {
			owns = owns || owned[strings.ToLower(issue)]
		}
		if owns {
			owned[strings.ToLower(ref)] = true
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
	// A report can mention its own PR as evidence for another repository's
	// defect. Do not hide that finding, including a bare owner/repo reference.
	for _, repo := range deliveryProblemRepo.FindAllString(deliveryProblemRef.ReplaceAllString(body, ""), -1) {
		if !repos[strings.ToLower(repo)] {
			return false
		}
	}
	return true
}
