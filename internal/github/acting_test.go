package github

import (
	"context"
	"errors"
	"slices"
	"strings"
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

// The repositories of an organisation the App reaches and of a personal
// account it does not.
const (
	orgRepo      = "giantswarm/beekeeper"
	personalRepo = "teemow/lab"
)

func TestByOwnerGroupsByOwnerInFirstSeenOrder(t *testing.T) {
	refs := []PR{{orgRepo, 1}, {personalRepo, 2}, {"GiantSwarm/devctl", 3}, {"teemow/notes", 4}}
	got := ByOwner(refs, func(r PR) string { return r.Repo })
	want := [][]PR{{refs[0], refs[2]}, {refs[1], refs[3]}}
	if !slices.EqualFunc(got, want, slices.Equal) {
		t.Errorf("ByOwner = %v, want %v", got, want)
	}
	if got := ByOwner(nil, func(r PR) string { return r.Repo }); got != nil {
		t.Errorf("ByOwner of nothing = %v", got)
	}
}

// ownerLink is a gh run that answers like devctl's agent link: a repository
// of the personal account teemow resolves only when GH_REPO (ActingOn) names
// an owner outside the App's installations, so the call runs on the person's
// own login; it records the GH_REPO of every call.
func ownerLink(acting *[]string, answer func(query string) string) GH {
	return func(ctx context.Context, args ...string) ([]byte, error) {
		repo := Repo(ctx)
		*acting = append(*acting, repo)
		query := args[len(args)-1]
		if strings.Contains(query, `owner:"teemow"`) && !strings.HasPrefix(repo, "teemow/") {
			return []byte(`{"data":null,"errors":[{"type":"NOT_FOUND","message":"Could not resolve to a Repository with the name 'teemow/lab'."}]}`), errors.New("exit status 1")
		}
		return []byte(answer(query)), nil
	}
}
