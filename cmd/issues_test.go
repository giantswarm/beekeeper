package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/internal/issuemonitor"
)

// testIssueTriageWebhook has the shape of a Slack incoming webhook.
const testIssueTriageWebhook = "https://hooks.slack.com/services/T000/B000/XXXX"

type issueTriageRoundTrip func(*http.Request) (*http.Response, error)

func (f issueTriageRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func stubIssueTriageTransport(t *testing.T, f issueTriageRoundTrip) {
	t.Helper()
	old := issueTriageTransport
	t.Cleanup(func() { issueTriageTransport = old })
	issueTriageTransport = f
}

func TestIssueMonitorSlackIsPlainAndDoesNotLeakErrors(t *testing.T) {
	calls := 0
	stubIssueTriageTransport(t, func(r *http.Request) (*http.Response, error) {
		calls++
		if got := r.URL.String(); got != testIssueTriageWebhook {
			t.Fatal(got)
		}
		raw, _ := io.ReadAll(r.Body)
		var v map[string]any
		if err := json.Unmarshal(raw, &v); err != nil {
			t.Fatal(err)
		}
		if v["mrkdwn"] != false || v["unfurl_links"] != false || v["unfurl_media"] != false {
			t.Fatal(v)
		}
		if v["text"] != "title <@everyone>" {
			t.Fatal(v)
		}
		return &http.Response{StatusCode: http.StatusBadRequest, Body: io.NopCloser(strings.NewReader("private response")), Request: r}, nil
	})
	err := postIssueTriage(context.Background(), testIssueTriageWebhook, "title <@everyone>")
	if err == nil || strings.Contains(err.Error(), testIssueTriageWebhook) || strings.Contains(err.Error(), "private response") {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("calls = %d", calls)
	}
}

func TestIssueMonitorSlackAcceptsDelivery(t *testing.T) {
	stubIssueTriageTransport(t, func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody, Request: r}, nil
	})
	if err := postIssueTriage(context.Background(), testIssueTriageWebhook, "title"); err != nil {
		t.Fatal(err)
	}
}

func TestIssueMonitorSlackRefusesOtherDestinations(t *testing.T) {
	stubIssueTriageTransport(t, func(r *http.Request) (*http.Response, error) {
		t.Fatalf("request sent to %s", r.URL)
		return nil, nil
	})
	for name, webhook := range map[string]string{
		"empty":      "",
		"unparsable": "https://hooks.slack.com:port/services/T000/B000/XXXX",
		"plain http": "http://hooks.slack.com/services/T000/B000/XXXX",
		"other host": "https://hooks.example.invalid/services/T000/B000/XXXX",
		"subdomain":  "https://hooks.slack.com.example.invalid/services/T000/B000/XXXX",
		"port":       "https://hooks.slack.com:8443/services/T000/B000/XXXX",
		"userinfo":   "https://user@hooks.slack.com/services/T000/B000/XXXX",
		"query":      "https://hooks.slack.com/services/T000/B000/XXXX?next=1",
		"fragment":   "https://hooks.slack.com/services/T000/B000/XXXX#next",
		"other path": "https://hooks.slack.com/api/chat.postMessage",
		"traversal":  "https://hooks.slack.com/services/T000/../../api/XXXX",
	} {
		t.Run(name, func(t *testing.T) {
			err := postIssueTriage(context.Background(), webhook, "title")
			if err == nil {
				t.Fatal("missing error")
			}
			if webhook != "" && strings.Contains(err.Error(), webhook) {
				t.Fatal(err)
			}
		})
	}
}

func TestIssueMonitorDispatchRechecksLiveMembership(t *testing.T) {
	for _, member := range []bool{true, false} {
		t.Run(map[bool]string{true: "member", false: "revoked"}[member], func(t *testing.T) {
			a, _ := noteApp(t)
			a.cfg.StateDir = t.TempDir()
			ctx := context.Background()
			err := issuemonitor.WithState(ctx, a.issueMonitorDir(), func(s *issuemonitor.State, _ func(*issuemonitor.State) error) error {
				s.Queue = []issuemonitor.Pointer{{Ref: "org/repo#1", Source: workerGitHubIssueGroup}}
				s.Issues["org/repo#1"] = issuemonitor.Record{Class: issuemonitor.Employee, State: reviewedMergeQueued}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			old := issueMonitorGH
			t.Cleanup(func() { issueMonitorGH = old })
			calls := 0
			issueMonitorGH = func(_ context.Context, args ...string) ([]byte, error) {
				if strings.Contains(strings.Join(args, " "), "IssueMonitorIssue") {
					if strings.Contains(strings.Join(args, " "), "title") {
						t.Fatal(args)
					}
					return []byte(`{"data":{"repository":{"issueOrPullRequest":{"state":"OPEN","author":{"login":"staff"},"__typename":"Issue"}}}}`), nil
				}
				calls++
				if member {
					return []byte(`{"state":"active"}`), nil
				}
				return nil, errors.New("API unavailable")
			}
			next, err := a.nextMonitoredIssue(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if (next != nil) != member || calls != 1 {
				t.Fatalf("next=%v membership calls=%d", next, calls)
			}
			if member {
				if err := a.monitoredIssueStarted(ctx, next.Ref); err != nil {
					t.Fatal(err)
				}
			}
			if err := issuemonitor.WithState(ctx, a.issueMonitorDir(), func(s *issuemonitor.State, _ func(*issuemonitor.State) error) error {
				if len(s.Queue) != 0 {
					t.Fatal(s.Queue)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestIssueMonitorBoardCannotBypassExternalClassification(t *testing.T) {
	a, _ := noteApp(t)
	a.cfg.StateDir = t.TempDir()
	ctx := context.Background()
	if err := issuemonitor.WithState(ctx, a.issueMonitorDir(), func(s *issuemonitor.State, _ func(*issuemonitor.State) error) error {
		s.Issues["org/repo#7"] = issuemonitor.Record{Class: issuemonitor.External, State: "triage-flagged"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	old := issueMonitorGH
	t.Cleanup(func() { issueMonitorGH = old })
	issueMonitorGH = func(_ context.Context, args ...string) ([]byte, error) {
		if strings.Contains(strings.Join(args, " "), "IssueMonitorIssue") {
			return []byte(`{"data":{"repository":{"issueOrPullRequest":{"state":"OPEN","author":{"login":"visitor"},"__typename":"Issue"}}}}`), nil
		}
		return nil, errors.New("non-member")
	}
	if err := a.allowMonitoredIssue(ctx, "org/repo#7"); !errors.Is(err, issuemonitor.ErrExternal) {
		t.Fatalf("external board issue allowed: %v", err)
	}
}
