package cmd

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/internal/sandbox"
)

const (
	protectionTestBranch = "release/v1"
	protectionTestRepo   = "acme/repo"
)

type protectionTransport func(*http.Request) (*http.Response, error)

func (f protectionTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestProtectionArgsRefuseWritesAndOtherOwners(t *testing.T) {
	for _, args := range [][]string{
		nil, {protectionTestRepo}, {rebaseOtherRepo, mainBranch}, {protectionTestRepo, mainBranch, http.MethodDelete},
		{protectionTestRepo, mainBranch, "--method=PUT"}, {"acme/repo?x=y", mainBranch},
		{"acme/..", mainBranch}, {protectionTestRepo, ".."}, {protectionTestRepo, ""},
	} {
		if _, _, err := protectionArgs(args, []string{testOrg}); err == nil {
			t.Errorf("accepted %q", args)
		}
	}
	repo, branch, err := protectionArgs([]string{"ACME/repo", protectionTestBranch}, []string{testOrg})
	if err != nil || repo != "ACME/repo" || branch != protectionTestBranch {
		t.Fatalf("%q %q %v", repo, branch, err)
	}
}

func TestProtectionReadOnlyGET(t *testing.T) {
	for _, status := range []int{200, 403, 404, 302, 500} {
		client := &http.Client{Transport: protectionTransport(func(r *http.Request) (*http.Response, error) {
			if r.Method != http.MethodGet || r.Body != nil {
				t.Fatalf("request could write: %s", r.Method)
			}
			if r.URL.EscapedPath() != "/repos/acme/repo/branches/release%2Fv1/protection" || r.URL.RawQuery != "" {
				t.Fatalf("unexpected URL: %s", r.URL)
			}
			if r.Header.Get("Authorization") != "Bearer test-token" {
				t.Fatal("missing host credential")
			}
			return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(`{"required_status_checks":{"strict":true}}`)), Header: make(http.Header)}, nil
		})}
		out, err := readProtection(context.Background(), client, "test-token", protectionTestRepo, protectionTestBranch)
		if status == 200 {
			if err != nil || !strings.Contains(out, `"strict":true`) {
				t.Fatalf("lost protection: %q %v", out, err)
			}
		} else if err == nil || out != "" || strings.Contains(err.Error(), "test-token") {
			t.Fatalf("failed read: %q %v", out, err)
		}
	}
}

func TestProtectionBrokerRejectsWriteBeforeCredentials(t *testing.T) {
	a, _, _ := secretApp(t)
	for _, args := range [][]string{{dependabotTestRepo, mainBranch, http.MethodDelete}, {"other/example", mainBranch}} {
		if _, err := a.brokeredProtection()(context.Background(), 0, sandbox.Request{Args: args}); err == nil {
			t.Fatalf("broker accepted %q", args)
		}
	}
}
