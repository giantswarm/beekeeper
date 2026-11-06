package issuemonitor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/github"
)

const (
	testStaff   = "staff"
	testVisitor = "visitor"
	sourceIssue = "issue"
	keyLogin    = "login"
	keyNodes    = "nodes"
)

func TestClassifyLive(t *testing.T) {
	for _, tc := range []struct {
		name, author, response string
		fail                   bool
		want                   Class
	}{
		{"member", testStaff, `{"state":"active"}`, false, Employee},
		{"non-member", testVisitor, `{"state":"pending"}`, false, External},
		{"bot", "renovate[bot]", `{"state":"active"}`, false, Bot},
		{"align-files", "align-files", `{}`, true, Bot},
		{"failure", testStaff, ``, true, External},
		{"malformed", testStaff, `broken`, false, External},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			gh := func(_ context.Context, args ...string) ([]byte, error) {
				calls++
				if !strings.Contains(strings.Join(args, " "), "/teams/employees/memberships/"+tc.author) {
					t.Fatal(args)
				}
				if tc.fail {
					return nil, errors.New("unavailable")
				}
				return []byte(tc.response), nil
			}
			for range 2 {
				if got := Classify(context.Background(), gh, "org", tc.author); got != tc.want {
					t.Fatalf("got %s want %s", got, tc.want)
				}
			}
			if calls != 2 {
				t.Fatal("membership cached")
			}
		})
	}
}

func TestQueueRefusesExternalAndIgnoresCallerClaims(t *testing.T) {
	for _, member := range []bool{false, true} {
		var s State
		gh := func(_ context.Context, args ...string) ([]byte, error) {
			if strings.Contains(strings.Join(args, " "), "IssueMonitorIssue") {
				if strings.Contains(strings.Join(args, " "), "body") || strings.Contains(strings.Join(args, " "), "comments") || strings.Contains(strings.Join(args, " "), "title") {
					t.Fatal(args)
				}
				return []byte(`{"data":{"repository":{"issueOrPullRequest":{"number":7,"state":"OPEN","author":{"login":"visitor"},"__typename":"Issue"}}}}`), nil
			}
			if member {
				return []byte(`{"state":"active"}`), nil
			}
			return nil, errors.New("not a member")
		}
		err := s.Enqueue(context.Background(), gh, "org/repo", 7)
		if !member {
			if !errors.Is(err, ErrExternal) || len(s.Queue) != 0 {
				t.Fatalf("external admitted: %v %+v", err, s.Queue)
			}
		} else {
			if err != nil || len(s.Queue) != 1 || s.Queue[0].Source != sourceIssue {
				t.Fatalf("employee refused: %v", err)
			}
		}
	}
}

func TestPollReconcilesAllPagesAndNotifiesOnce(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	s := State{}
	messages := []string{}
	reads := 0
	saves := 0
	labels := 0
	gh := func(_ context.Context, args ...string) ([]byte, error) {
		text := strings.Join(args, " ")
		if strings.Contains(text, "body") || strings.Contains(text, "comments") {
			t.Fatal("requested issue content", args)
		}
		switch {
		case args[0] == "label":
			return []byte(`{}`), nil
		case args[0] == sourceIssue && args[1] == "edit":
			labels++
			return []byte(`{}`), nil
		case strings.Contains(strings.Join(args, " "), "IssueMonitorIssue"):
			return []byte(`{"data":{"repository":{"issueOrPullRequest":{"state":"OPEN","author":{"login":"staff"},"__typename":"Issue"}}}}`), nil
		case strings.Contains(text, "memberships/staff"):
			return []byte(`{"state":"active"}`), nil
		case strings.Contains(text, "memberships/"):
			return nil, errors.New("404")
		case strings.Contains(text, "graphql"):
			reads++
			nodes := []map[string]any{}
			next := !strings.Contains(text, "cursor=next")
			if next {
				nodes = append(nodes, map[string]any{"number": 1, "title": "external title <@all>", "author": map[string]string{keyLogin: testVisitor}, "updatedAt": now.Add(-200 * 24 * time.Hour)})
			} else {
				for n, author := range map[int]string{2: testStaff, 3: "renovate[bot]", 4: "owner"} {
					i := map[string]any{"number": n, "author": map[string]string{keyLogin: author}, "updatedAt": now, "assignees": map[string]any{keyNodes: []map[string]string{{keyLogin: "assigned"}}}}
					if n != 4 {
						i["assignees"] = map[string]any{keyNodes: []any{}}
					}
					nodes = append(nodes, i)
				}
			}
			return json.Marshal(map[string]any{"data": map[string]any{"repository": map[string]any{"issues": map[string]any{keyNodes: nodes, "pageInfo": map[string]any{"hasNextPage": next, "endCursor": "next"}}}}})
		}
		return nil, fmt.Errorf("unexpected call %v", args)
	}
	m := Monitor{GH: gh, Now: now, Save: func(*State) error { saves++; return nil }, Notify: func(_ context.Context, team, message string) error {
		if team != "team-bumblebee" {
			t.Fatal(team)
		}
		messages = append(messages, message)
		return nil
	}}
	for range 2 {
		if err := m.Poll(ctx, &s, map[string]string{"org/repo": "team-bumblebee"}); err != nil {
			t.Fatal(err)
		}
	}
	if reads != 4 || len(messages) != 2 || labels != 4 || saves < 8 {
		t.Fatalf("reads %d notifications %d labels %d saves %d", reads, len(messages), labels, saves)
	}
	// Both external authors (including an assigned issue) require human triage.
	if len(s.Queue) != 1 || s.Queue[0].Ref != "org/repo#2" {
		t.Fatal(s.Queue)
	}
	if !slices.Contains(s.Contributors["org/repo"], testVisitor) {
		t.Fatal(s.Contributors)
	}
	for ref, r := range s.Issues {
		if !slices.Contains([]string{"queued", "owned", "triage-flagged", "reported-for-cleanup"}, r.State) {
			t.Fatalf("%s has no state", ref)
		}
	}
	if s.Issues["org/repo#1"].Cleanup != "stale" {
		t.Fatal(s.Issues)
	}
	raw, _ := json.Marshal(s)
	if strings.Contains(string(raw), "external title") {
		t.Fatal("external title persisted")
	}
	if !strings.Contains(strings.Join(messages, " "), "https://github.com/org/repo/issues/1") {
		t.Fatal(messages)
	}
}

func TestReclassificationRevokesQueue(t *testing.T) {
	s := State{Queue: []Pointer{{Ref: "org/repo#1", Source: sourceIssue}}}
	s.Init()
	m := Monitor{GH: github.GH(func(context.Context, ...string) ([]byte, error) { return []byte(`{}`), nil }), Save: func(*State) error { return nil }, Notify: func(context.Context, string, string) error { return nil }}
	i := Issue{Number: 1}
	i.Author.Login = testVisitor
	if err := m.reconcile(context.Background(), &s, "org/repo", "team", i, false); err != nil {
		t.Fatal(err)
	}
	if len(s.Queue) != 0 || s.Issues["org/repo#1"].State != "triage-flagged" {
		t.Fatal(s)
	}
}

func TestStorePersists(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	if err := WithState(ctx, dir, func(s *State, save func(*State) error) error {
		s.Contributors["org/repo"] = []string{testVisitor}
		return save(s)
	}); err != nil {
		t.Fatal(err)
	}
	if err := WithState(ctx, dir, func(s *State, _ func(*State) error) error {
		if len(s.Contributors["org/repo"]) != 1 {
			t.Fatal(s)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestStoreRejectsCorruption(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte("broken"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := WithState(context.Background(), dir, func(*State, func(*State) error) error { t.Fatal("corrupt state accepted"); return nil }); err == nil {
		t.Fatal("missing error")
	}
}

func TestReopenedEmployeeIssueReturnsToQueue(t *testing.T) {
	s := State{}
	s.Init()
	s.Issues["org/repo#1"] = Record{State: "owned", Class: Employee}
	m := Monitor{GH: func(_ context.Context, args ...string) ([]byte, error) {
		if strings.Contains(strings.Join(args, " "), "IssueMonitorIssue") {
			return []byte(`{"data":{"repository":{"issueOrPullRequest":{"state":"OPEN","author":{"login":"staff"},"__typename":"Issue"}}}}`), nil
		}
		return []byte(`{"state":"active"}`), nil
	}, Save: func(*State) error { return nil }}
	i := Issue{Number: 1, ReopenedAt: time.Now()}
	i.Author.Login = testStaff
	if err := m.reconcile(context.Background(), &s, "org/repo", "team", i, false); err != nil {
		t.Fatal(err)
	}
	if len(s.Queue) != 1 || s.Issues["org/repo#1"].State != "queued" {
		t.Fatal(s)
	}
}

func TestFailedNotificationIsRetriedWithoutAdmittingExternal(t *testing.T) {
	s := State{}
	s.Init()
	calls := 0
	fail := true
	m := Monitor{GH: func(context.Context, ...string) ([]byte, error) { return []byte(`{}`), nil }, Save: func(*State) error { return nil }, Notify: func(context.Context, string, string) error {
		calls++
		if fail {
			return errors.New("delivery failed")
		}
		return nil
	}}
	i := Issue{Number: 1}
	i.Author.Login = testVisitor
	if err := m.reconcile(context.Background(), &s, "org/repo", "team", i, false); err == nil {
		t.Fatal("delivery failure hidden")
	}
	if s.Issues["org/repo#1"].Notified || len(s.Queue) != 0 {
		t.Fatal(s)
	}
	fail = false
	for range 2 {
		if err := m.reconcile(context.Background(), &s, "org/repo", "team", i, false); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 2 {
		t.Fatalf("notification attempts=%d", calls)
	}
}

func TestQueueRefusesPullRequest(t *testing.T) {
	var s State
	gh := func(_ context.Context, args ...string) ([]byte, error) {
		return []byte(`{"data":{"repository":{"issueOrPullRequest":{"__typename":"PullRequest"}}}}`), nil
	}
	if err := s.Enqueue(context.Background(), gh, "org/repo", 1); err == nil || len(s.Queue) != 0 {
		t.Fatalf("pull request admitted: %v", err)
	}
}
