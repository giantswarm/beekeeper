package superloop

import (
	"strings"
	"testing"
)

// A COMMENTED verdict is how a reviewer using the author's account asks
// for changes. Moving the branch must not turn that verdict into approval.
func TestCommentedChangesRequestedNeverApprovesMovedHead(t *testing.T) {
	body := "Review: changes requested\nHead reviewed: `" + head1 + "`\n\nFix the merge guard."
	rev := Review{ID: "R1", Head: ReviewedHead(body, head1), Verdict: Verdict("COMMENTED", body), Body: body, At: t1}
	if rev.Verdict != ChangesRequested {
		t.Fatalf("commented review verdict = %q", rev.Verdict)
	}
	for _, head := range []string{head1, head2} {
		in := Input{
			Workers: parked(t0), Pulls: pull(head, true, rev), Rounds: 2,
			Taken: taken(t, t0, Action{Key: reviewKey(pr, head1), Kind: KindReview, Ref: pr, Agent: worker}),
		}
		a := only(t, Plan(in), KindForward)
		if !strings.Contains(a.Message, "asks for changes on head "+head1) || !strings.HasSuffix(a.Message, body) {
			t.Fatalf("findings lost reviewed head: %+v", a)
		}
	}
}
