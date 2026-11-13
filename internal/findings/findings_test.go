package findings

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

const (
	findingTitle = "Queue stalls"
	findingBody  = "Facts"
	findingURL   = "https://github.com/example/app/issues/2"
	apiCommand   = "api"
)

func TestPlaceReadsAllPagesAndOnlyRewritesNewFacts(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		wantWrite  bool
	}{
		{"known", "The queue stalls.", false},
		{"new fact", "The queue stalls when the lease expires.", true},
		{"case is a fact", "The QUEUE stalls.", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls [][]string
			gh := func(ctx context.Context, args ...string) ([]byte, error) {
				calls = append(calls, args)
				if len(calls) == 1 {
					return []byte(`[[{"number":1,"title":"Unrelated","body":"Other"}],[{"number":2,"title":"Queue stalls","body":"The queue stalls."}]]`), nil
				}
				return []byte(`{}`), nil
			}
			url, err := Place(context.Background(), gh, Report{Repo: "example/app", Title: findingTitle, Body: tc.body})
			if err != nil || url != findingURL {
				t.Fatalf("%s: %v", url, err)
			}
			want := 1
			if tc.wantWrite {
				want++
			}
			if len(calls) != want {
				t.Fatalf("calls: %v", calls)
			}
			if !reflect.DeepEqual(calls[0], []string{apiCommand, "repos/example/app/issues?state=open&per_page=100", "--paginate", "--slurp"}) {
				t.Fatal(calls[0])
			}
			if tc.wantWrite && !reflect.DeepEqual(calls[1], []string{apiCommand, "repos/example/app/issues/2", "--method", "PATCH", "-f", "body=" + tc.body}) {
				t.Fatal(calls[1])
			}
		})
	}
}

func TestPlaceNeverCreatesOnUnreadOrAmbiguousInventory(t *testing.T) {
	for _, raw := range []string{`broken`, `null`, `[]`, `[null]`, `[[{"number":1,"title":"Queue stalls"},{"number":2,"title":"Queue stalls"}]]`} {
		count := 0
		_, err := Place(context.Background(), func(context.Context, ...string) ([]byte, error) { count++; return []byte(raw), nil }, Report{Repo: "example/app", Title: findingTitle, Body: findingBody})
		if err == nil || count != 1 {
			t.Fatalf("%q: calls=%d error=%v", raw, count, err)
		}
	}
	_, err := Place(context.Background(), func(context.Context, ...string) ([]byte, error) { return []byte(`[[]]`), nil }, Report{Repo: "example/app", Number: 9, Title: findingTitle, Body: findingBody})
	if !errors.Is(err, ErrUnplaced) {
		t.Fatal(err)
	}
}

func TestPlaceCreatesOneIssueAndIgnoresPulls(t *testing.T) {
	calls := 0
	url, err := Place(context.Background(), func(_ context.Context, args ...string) ([]byte, error) {
		calls++
		if calls == 1 {
			return []byte(`[[{"number":1,"title":"Queue stalls","pull_request":{}}]]`), nil
		}
		if !reflect.DeepEqual(args, []string{apiCommand, "repos/example/app/issues", "--method", "POST", "-f", "title=Queue stalls", "-f", "body=Facts"}) {
			t.Fatal(args)
		}
		return []byte(`{"number":2}`), nil
	}, Report{Repo: "example/app", Title: findingTitle, Body: findingBody})
	if err != nil || calls != 2 || url != findingURL {
		t.Fatalf("%s %v calls=%d", url, err, calls)
	}
}

func TestParseRequiresExplicitOwnerAndCurrentDescription(t *testing.T) {
	for _, text := range []string{"example/app: Queue stalls\n\nFacts", "https://github.com/example/app/issues/2: Queue stalls\n\nFacts"} {
		r, ok := Parse(text)
		if !ok || r.Repo != "example/app" || r.Body != findingBody || r.Title != findingTitle {
			t.Fatalf("%+v %v", r, ok)
		}
		if strings.Contains(text, "issues/2") && r.Number != 2 {
			t.Fatal(r)
		}
	}
	for _, text := range []string{"queue stalls in example/app", "example/app: queue stalls", "https://github.com/example/app/pull/2: Queue stalls\nFacts"} {
		if r, ok := Parse(text); ok {
			t.Fatalf("accepted ambiguous report: %+v", r)
		}
	}
}

func TestPlacementRetryRecoversAnIssueWhoseCreateResponseWasLost(t *testing.T) {
	created := false
	posts := 0
	gh := func(_ context.Context, args ...string) ([]byte, error) {
		if len(args) > 3 && args[3] == "POST" {
			posts++
			created = true
			return nil, errors.New("connection reset after create")
		}
		if created {
			return []byte(`[[{"number":2,"title":"Queue stalls","body":"Facts"}]]`), nil
		}
		return []byte(`[[]]`), nil
	}
	report := Report{Repo: "example/app", Title: findingTitle, Body: findingBody}
	if _, err := Place(context.Background(), gh, report); err == nil {
		t.Fatal("lost response was ignored")
	}
	url, err := Place(context.Background(), gh, report)
	if err != nil || posts != 1 || url != findingURL {
		t.Fatalf("retry: %s %v, creates=%d", url, err, posts)
	}
}
