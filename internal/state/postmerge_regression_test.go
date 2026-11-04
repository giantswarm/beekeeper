package state

import "testing"

func TestReviewParkPostMergeDistinguishesOmittedAndEmpty(t *testing.T) {
	const on = "o/r#7"
	work, empty, replacement := "verify rollout", "", "check release"
	for _, tc := range []struct {
		name string
		old  *string
		next *string
		want string
	}{
		{name: "omitted inherits work", old: &work, want: work},
		{name: "explicit empty clears work", old: &work, next: &empty},
		{name: "new work replaces old", old: &work, next: &replacement, want: replacement},
		{name: "omitted preserves explicit empty", old: &empty},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := &State{ReviewParks: []ReviewPark{{On: on, ReviewHead: beforeWord, PostMerge: tc.old}}}
			st.UpsertReviewPark(ReviewPark{On: "O/R#7", ReviewHead: "after", PostMerge: tc.next})
			rp, ok := st.ReviewParkFor(on)
			if !ok || len(st.ReviewParks) != 1 || rp.PostMerge == nil || *rp.PostMerge != tc.want || rp.ReviewHead != "after" {
				t.Fatalf("re-park lost the declaration: %+v", st.ReviewParks)
			}
		})
	}
}
