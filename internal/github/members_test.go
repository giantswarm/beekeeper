package github

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"
)

// orgServer answers the token's own membership with own (a status, and the
// state for 200) and lists the members two pages long.
func orgServer(t *testing.T, ownStatus int, ownState string) {
	t.Helper()
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			t.Errorf("%s without the token", r.URL.Path)
		}
		switch {
		case r.URL.Path == "/user/memberships/orgs/acme":
			w.WriteHeader(ownStatus)
			_, _ = w.Write([]byte(`{"state":"` + ownState + `"}`))
		case r.URL.Path == "/orgs/acme/members" && r.URL.Query().Get("page") == "":
			w.Header().Set("Link", `<`+srv.URL+`/orgs/acme/members?per_page=100&page=2>; rel="next", <`+srv.URL+`/orgs/acme/members?per_page=100&page=2>; rel="last"`)
			_, _ = w.Write([]byte(`[{"login":"Public-One"},{"login":"zed"}]`))
		case r.URL.Path == "/orgs/acme/members" && r.URL.Query().Get("page") == "2":
			_, _ = w.Write([]byte(`[{"login":"private-two"}]`))
		default:
			t.Errorf("unexpected request %s", r.URL)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	t.Setenv("BEEKEEPER_GITHUB_API", srv.URL)
}

// An active member's token reads every page of the roster, which answers
// members in any case and outsiders as not members.
func TestReadRoster(t *testing.T) {
	orgServer(t, http.StatusOK, "active")
	now := time.Date(2026, 10, 7, 5, 0, 0, 0, time.UTC)
	r, err := ReadRoster(context.Background(), http.DefaultClient, "tok", "acme", now)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"private-two", "public-one", "zed"}; !slices.Equal(r.Members, want) || r.Org != "acme" || !r.At.Equal(now) {
		t.Fatalf("roster = %+v, want members %v", r, want)
	}
	for login, want := range map[string]string{"Private-Two": Member, "public-one": Member, "outsider": NotMember} {
		if got := r.Answer(login); got != want {
			t.Errorf("Answer(%s) = %q, want %q", login, got, want)
		}
	}
}

// A token that may not read the org's membership, or whose own membership
// is not active, is ErrPermission: never a roster of the public members.
func TestReadRosterPermissionMissing(t *testing.T) {
	for name, tc := range map[string]struct {
		status int
		state  string
	}{
		"integration refused": {http.StatusForbidden, ""},
		"no read:org":         {http.StatusNotFound, ""},
		"invitation pending":  {http.StatusOK, "pending"},
	} {
		t.Run(name, func(t *testing.T) {
			orgServer(t, tc.status, tc.state)
			_, err := ReadRoster(context.Background(), http.DefaultClient, "tok", "acme", time.Now())
			if !errors.Is(err, ErrPermission) {
				t.Fatalf("ReadRoster = %v, want ErrPermission", err)
			}
		})
	}
}

// A server error is an error, not a missing permission.
func TestReadRosterServerError(t *testing.T) {
	orgServer(t, http.StatusBadGateway, "")
	_, err := ReadRoster(context.Background(), http.DefaultClient, "tok", "acme", time.Now())
	if err == nil || errors.Is(err, ErrPermission) {
		t.Fatalf("ReadRoster = %v, want a plain error", err)
	}
}
