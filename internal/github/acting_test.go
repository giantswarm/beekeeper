package github

import (
	"context"
	"slices"
	"testing"
)

func TestActingOnNamesTheRepositoryInGHRepo(t *testing.T) {
	ctx := context.Background()
	if got := Repo(ctx); got != "" {
		t.Errorf("Repo of a plain context = %q", got)
	}
	if got := Repo(ActingOn(ctx, "teemow/lab")); got != "teemow/lab" {
		t.Errorf("Repo = %q", got)
	}
	environ := []string{"PATH=/bin", "GH_REPO=o/other", "HOME=/h"}
	if got := ghEnv(environ, ""); !slices.Equal(got, environ) {
		t.Errorf("ghEnv without a repository = %q", got)
	}
	if got, want := ghEnv(environ, "teemow/lab"), []string{"PATH=/bin", "HOME=/h", "GH_REPO=teemow/lab"}; !slices.Equal(got, want) {
		t.Errorf("ghEnv = %q, want %q", got, want)
	}
}
