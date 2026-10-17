package github

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestPublishedCandidate(t *testing.T) {
	const linux = "devctl-linux-amd64"
	const doc = `[
{"tag_name":"v8.1.0-rc.3","draft":true,"prerelease":true,"published_at":"2026-10-17T09:40:00Z","assets":[{"name":"devctl-linux-amd64","state":"uploaded"}]},
{"tag_name":"v8.1.0-rc.2","prerelease":true,"published_at":"2026-10-17T09:30:00Z","assets":[{"name":"devctl-linux-amd64","state":"new"}]},
{"tag_name":"v8.1.0-rc.1","prerelease":true,"published_at":"2026-10-17T09:20:00Z","assets":[{"name":"devctl-linux-amd64","state":"uploaded"}]},
{"tag_name":"v8.0.0","published_at":"2026-10-17T09:25:00Z","assets":[{"name":"devctl-linux-amd64","state":"uploaded"}]},
{"tag_name":"v8.0.0-rc.9","prerelease":true,"published_at":"2026-10-17T08:00:00Z","assets":[{"name":"devctl-linux-amd64","state":"uploaded"}]}
]`
	at := func(s string) time.Time { v, _ := time.Parse(time.RFC3339, s); return v }
	for _, c := range []struct {
		name  string
		since time.Time
		asset string
		err   error
		want  string
	}{
		{"the newest uploaded candidate", at("2026-10-17T09:00:00Z"), linux, nil, "v8.1.0-rc.1"},
		{"none since", at("2026-10-17T09:21:00Z"), linux, nil, ""},
		{"another platform", at("2026-10-17T07:00:00Z"), "devctl-darwin-arm64", nil, ""},
		{"GitHub fails", time.Time{}, linux, errors.New("connection reset"), ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			run := func(_ context.Context, args ...string) ([]byte, error) {
				if got := strings.Join(args, " "); got != "api repos/o/r/releases?per_page=10" {
					t.Errorf("asked %q", got)
				}
				return []byte(doc), c.err
			}
			got, err := PublishedCandidate(context.Background(), run, "o/r", c.asset, c.since)
			if got != c.want || (err != nil) != (c.err != nil) {
				t.Errorf("got %q, %v; want %q", got, err, c.want)
			}
		})
	}
}
