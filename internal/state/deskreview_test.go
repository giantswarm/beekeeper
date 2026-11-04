package state

import "testing"

func TestUpsertReviewParkPostMergeDeclaration(t *testing.T) {
	for _, tc := range []struct {
		name        string
		declaration *string
		want        string
	}{
		{"omitted", nil, "verify rollout"},
		{"empty", new(string), ""},
		{"replacement", stringPointer("release"), "release"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := State{}
			st.UpsertReviewPark(ReviewPark{On: "o/r#1", ReviewHead: "head", PostMerge: stringPointer("verify rollout")})
			st.UpsertReviewPark(ReviewPark{On: "o/r#1", ReviewHead: "next", PostMerge: tc.declaration})
			got, _ := st.ReviewParkFor("o/r#1")
			if got.PostMerge == nil || *got.PostMerge != tc.want {
				t.Fatalf("post-merge declaration: %+v", got)
			}
		})
	}
}

func stringPointer(s string) *string { return &s }
