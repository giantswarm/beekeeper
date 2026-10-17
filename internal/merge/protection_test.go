package merge

import (
	"strings"
	"testing"
)

// classicDoc is devctl pr merge's document of a merge GitHub declined on a
// base whose classic branch protection requires a review.
const classicDoc = `{"exitCode":3,"verdict":"not_applicable","repository":"giantswarm/gitsemver","number":292,"reason":"merge declined error: ` +
	`GitHub declined the merge of giantswarm/gitsemver#292: At least 1 approving review is required by reviewers with write access. ` +
	`devctl acts as pat, who has no bypass on the review rule of main, which no ruleset carries: classic branch protection requires the review, ` +
	`and such a repository is aligned first, never merged past it: align: true on the entry gitsemver in repositories/team-honeybadger.yaml of ` +
	`giantswarm/github@main gives it the devctl ruleset, whose bypass lets the owning team (team-honeybadger) and the repository admins merge ` +
	`their own green pull requests; devctl never lifts enforce_admins, and its token carries no Administration permission to do so"}`

// A merge declined on classic protection is one line naming the cause and
// the alignment that fixes it, the team-file entry devctl names included.
func TestClassicProtectionNamesTheCauseAndTheAlignment(t *testing.T) {
	got := ClassicProtection([]byte(classicDoc))
	for _, want := range []string{
		"classic branch protection of giantswarm/gitsemver requires a review",
		"enforce_admins does not lift for it (no token of ours may change it: HTTP 403)",
		"align: true on the entry gitsemver in repositories/team-honeybadger.yaml of giantswarm/github",
		"(devctl repo status giantswarm/gitsemver)",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("ClassicProtection = %q, want it to contain %q", got, want)
		}
	}
	if strings.Contains(got, "\n") {
		t.Errorf("ClassicProtection = %q, want one line", got)
	}
}

// A decline that names no team-file entry still names the fix; any other
// document is not a classic-protection decline.
func TestClassicProtectionOtherDocuments(t *testing.T) {
	bare := `{"exitCode":3,"repository":"o/r","reason":"merge declined error: classic branch protection requires the review"}`
	if got := ClassicProtection([]byte(bare)); !strings.Contains(got, "align: true on the repository's entry in its team file of giantswarm/github (devctl repo status o/r)") {
		t.Errorf("ClassicProtection(bare) = %q", got)
	}
	for _, doc := range []string{
		`{"exitCode":3,"reason":"pull request is behind its base"}`,
		`{"exitCode":1,"verdict":"red","reason":"classic branch protection"}`,
		`{`,
	} {
		if got := ClassicProtection([]byte(doc)); got != "" {
			t.Errorf("ClassicProtection(%s) = %q, want none", doc, got)
		}
	}
}
