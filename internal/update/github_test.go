package update

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

type releaseTransport func(*http.Request) (*http.Response, error)

func (f releaseTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestGitHubReleasePagination(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")
	old := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = old })
	pages := 0
	http.DefaultTransport = releaseTransport(func(r *http.Request) (*http.Response, error) {
		header := make(http.Header)
		if strings.HasSuffix(r.URL.Path, "/latest") {
			return &http.Response{StatusCode: 404, Header: header, Body: io.NopCloser(strings.NewReader(`{}`)), Request: r}, nil
		}
		pages++
		tag := "v0.171.0-rc.99"
		if r.URL.Query().Get("page") == "2" {
			tag = "v0.171.0-rc.100"
		} else {
			header.Set("Link", `<https://api.github.com/repos/giantswarm/beekeeper/releases?page=2>; rel="next"`)
		}
		body := fmt.Sprintf(`[{"id":%d,"tag_name":%q,"prerelease":true,"assets":[{"id":1,"name":%q},{"id":2,"name":%q}]}]`, pages, tag, binaryName(), binaryName()+".bundle")
		return &http.Response{StatusCode: 200, Header: header, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	})
	setup(t, "v0.171.0-rc.98", newSource())
	res, err := Run(context.Background(), io.Discard, true)
	if !errors.Is(err, ErrOutdated) {
		t.Fatalf("check error=%v; want ErrOutdated", err)
	}
	if res.Latest != "v0.171.0-rc.100" || pages != 2 {
		t.Fatalf("latest=%s pages=%d; want rc.100 across two pages", res.Latest, pages)
	}
}

func TestGitHubPaginationFailureDoesNotReportAnOlderRelease(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")
	old := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = old })
	http.DefaultTransport = releaseTransport(func(r *http.Request) (*http.Response, error) {
		header := make(http.Header)
		status, body := http.StatusOK, `[{"id":1,"tag_name":"v0.171.0-rc.99","prerelease":true}]`
		if r.URL.Query().Get("page") == "2" {
			status, body = http.StatusInternalServerError, `{}`
		} else {
			header.Set("Link", `<https://api.github.com/repos/giantswarm/beekeeper/releases?page=2>; rel="next"`)
		}
		return &http.Response{StatusCode: status, Header: header, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	})
	setup(t, "v0.171.0-rc.98", newSource())
	res, err := Run(context.Background(), io.Discard, true)
	if err == nil || res.Latest != "" {
		t.Fatalf("result=%+v error=%v; want lookup failure", res, err)
	}
}
