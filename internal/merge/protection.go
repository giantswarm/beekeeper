package merge

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// classicDecline is the cause in devctl's reason for a merge it declined
// because the base's classic branch protection requires a review and grants
// no bypass, which no ruleset carries.
const classicDecline = "classic branch protection"

// alignEntry finds the team-file entry devctl's decline names for the
// alignment: the repository's entry and the team file it lives in.
var alignEntry = regexp.MustCompile(`align: true on the entry (\S+) in (\S+) of giantswarm/github\b`)

// ClassicProtection is the gate's one line for a merge devctl declined on
// classic branch protection (exit 3): the cause and the fix, the
// repository aligned through giantswarm/github, whose devctl ruleset grants
// the owning team and the repository admins their bypass: no token the gate
// runs with may lift enforce_admins (HTTP 403). "" for any other document.
func ClassicProtection(doc []byte) string {
	var d struct {
		ExitCode   int    `json:"exitCode"`
		Reason     string `json:"reason"`
		Repository string `json:"repository"`
	}
	if json.Unmarshal(doc, &d) != nil || d.ExitCode != 3 || !strings.Contains(d.Reason, classicDecline) {
		return ""
	}
	repo := d.Repository
	if repo == "" {
		repo = "<owner/repo>"
	}
	entry := "the repository's entry in its team file of giantswarm/github"
	if m := alignEntry.FindStringSubmatch(d.Reason); m != nil {
		entry = fmt.Sprintf("the entry %s in %s of giantswarm/github", m[1], m[2])
	}
	return fmt.Sprintf("classic branch protection of %s requires a review the merge has no bypass for, and enforce_admins does not lift for it (no token of ours may change it: HTTP 403): align the repository, align: true on %s (devctl repo status %s)",
		repo, entry, repo)
}
