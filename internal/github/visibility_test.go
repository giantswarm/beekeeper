package github

import (
	"context"
	"errors"
	"testing"
)

func TestVisibilityPolicy(t *testing.T) {
	for _, tc := range []struct {
		name, repo, visibility string
		unreadable, want       bool
	}{
		{VisibilityPublic, "TEAM/repo", VisibilityPublic, false, true},
		{VisibilityPrivate, "team/repo", VisibilityPrivate, false, false},
		{VisibilityInternal, "team/repo", VisibilityInternal, false, false},
		{"unknown", "team/repo", "", false, false},
		{"unreadable", "team/repo", VisibilityPublic, true, false},
		{"unresolved", "team/*", VisibilityPublic, false, false},
		{"organization", "team/", VisibilityPublic, false, false},
		{"other owner", "other/repo", VisibilityPrivate, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := VisibilityPolicy{PublicOnlyOwners: []string{"Team"}, Read: func(string) (string, error) {
				if tc.unreadable {
					return "", errors.New("unreadable")
				}
				return tc.visibility, nil
			}}
			if got := p.Check(tc.repo) == nil; got != tc.want {
				t.Errorf("allowed = %v, want %v", got, tc.want)
			}
		})
	}
	if (VisibilityPolicy{PublicOnlyOwners: []string{"team"}}).Check("team/repo") == nil {
		t.Fatal("missing reader allowed restricted repository")
	}
}

func TestRepositoryVisibilityRequiresExplicitMetadata(t *testing.T) {
	for _, tc := range []struct{ raw, want string }{
		{`{"visibility":VisibilityPublic}`, VisibilityPublic},
		{`{"visibility":VisibilityPrivate}`, VisibilityPrivate},
		{`{"visibility":VisibilityInternal}`, VisibilityInternal},
		{`{VisibilityPrivate:false}`, ""},
		{`{}`, ""},
		{`null`, ""},
		{`{"visibility":"unknown"}`, ""},
		{`invalid`, ""},
	} {
		t.Run(tc.raw, func(t *testing.T) {
			got, err := RepositoryVisibility(context.Background(), func(ctx context.Context, args ...string) ([]byte, error) {
				if Repo(ctx) != "team/repo" || len(args) != 2 || args[0] != "api" || args[1] != "repos/team/repo" {
					t.Fatalf("incorrect routing or arguments: %v", args)
				}
				return []byte(tc.raw), nil
			}, "team/repo")
			if got != tc.want || (err == nil) != (tc.want != "") {
				t.Errorf("visibility = %q, error = %v", got, err)
			}
		})
	}
}
