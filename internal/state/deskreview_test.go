package state

import "testing"

func TestReviewNitCheckIsBoundToSupervisorAndBothHeads(t *testing.T) {
	const nitRef, nitHead, nitApproved = "o/r#1", "new", "approved"
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	check := ReviewNitCheck{Ref: nitRef, Head: nitHead, ApprovedHead: nitApproved, By: Party{Name: "supervisor"}}
	if err := store.Update(func(st *State) ([]Event, error) {
		st.ReviewNitChecks = []ReviewNitCheck{check}
		st.ReviewRequests = []ReviewRequest{{Ref: check.Ref, Head: check.Head, Round: 2}}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	st, err := store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if !st.ReviewNitsChecked(check.Ref, check.Head, check.ApprovedHead) || len(st.ReviewRequests) != 1 {
		t.Fatal("review state did not survive reopening")
	}
	for _, tt := range []struct{ ref, head, approved string }{
		{"o/r#2", nitHead, nitApproved}, {nitRef, "later", nitApproved}, {nitRef, nitHead, "other"},
	} {
		if st.ReviewNitsChecked(tt.ref, tt.head, tt.approved) {
			t.Fatalf("attestation covered unrelated delta: %+v", tt)
		}
	}
	for _, by := range []Party{{Name: "worker", Session: "worker-session"}, {Name: "worker", HostSession: "worker-host-session"}} {
		st.ReviewNitChecks[0].By = by
		if st.ReviewNitsChecked(check.Ref, check.Head, check.ApprovedHead) {
			t.Fatal("worker attestation accepted")
		}
	}
}
