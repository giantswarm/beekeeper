package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"
)

// The answers of an org membership question.
const (
	Member    = "member"
	NotMember = "not a member"
)

// ErrPermission is a token that may not read an org's whole membership: it
// sees the public members only, so its "not a member" would be a guess.
var ErrPermission = errors.New("permission missing")

// Roster is an org's members as a token that may read them all saw them.
type Roster struct {
	Org string `json:"org"`
	// Members are the logins, lower case and sorted.
	Members []string  `json:"members"`
	At      time.Time `json:"at"`
}

// Answer is login's membership by the roster: Member or NotMember.
func (r Roster) Answer(login string) string {
	if _, ok := slices.BinarySearch(r.Members, strings.ToLower(login)); ok {
		return Member
	}
	return NotMember
}

// ReadRoster reads org's members with token. The token's own membership
// comes first: only an active member's token, holding the read:org scope
// (or the App's members permission), lists the private members too; any
// other answer is ErrPermission, never an empty or public-only roster.
func ReadRoster(ctx context.Context, client *http.Client, token, org string, now time.Time) (Roster, error) {
	var own struct {
		State string `json:"state"`
	}
	status, _, err := get(ctx, client, token, apiURL()+"/user/memberships/orgs/"+url.PathEscape(org), &own)
	switch {
	case err != nil:
		return Roster{}, err
	case status == http.StatusUnauthorized || status == http.StatusForbidden || status == http.StatusNotFound:
		return Roster{}, fmt.Errorf("%w: the token may not read %s's membership (GitHub answered %d %s)", ErrPermission, org, status, http.StatusText(status))
	case status != http.StatusOK:
		return Roster{}, fmt.Errorf("GitHub answered %d %s for the token's membership in %s", status, http.StatusText(status), org)
	case own.State != "active":
		return Roster{}, fmt.Errorf("%w: the token's own membership in %s is %q, not active", ErrPermission, org, own.State)
	}
	r := Roster{Org: org, At: now}
	next := apiURL() + "/orgs/" + url.PathEscape(org) + "/members?per_page=100"
	for next != "" {
		var page []struct {
			Login string `json:"login"`
		}
		status, link, err := get(ctx, client, token, next, &page)
		if err != nil {
			return Roster{}, err
		}
		if status != http.StatusOK {
			return Roster{}, fmt.Errorf("GitHub answered %d %s for %s's members", status, http.StatusText(status), org)
		}
		for _, m := range page {
			r.Members = append(r.Members, strings.ToLower(m.Login))
		}
		next = nextLink(link)
	}
	slices.Sort(r.Members)
	return r, nil
}

// get reads u with token into v when GitHub answers 200, and returns the
// status and the Link header.
func get(ctx context.Context, client *http.Client, token, u string, v any) (int, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return 0, "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	resp, err := client.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, resp.Body)
		return resp.StatusCode, "", nil
	}
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		return 0, "", fmt.Errorf("%s: %w", req.URL.Path, err)
	}
	return resp.StatusCode, resp.Header.Get("Link"), nil
}

// nextRel is the next page's URL in a Link header.
var nextRel = regexp.MustCompile(`<([^>]+)>;\s*rel="next"`)

func nextLink(link string) string {
	if m := nextRel.FindStringSubmatch(link); m != nil {
		return m[1]
	}
	return ""
}
