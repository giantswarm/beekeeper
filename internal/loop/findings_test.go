package loop

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/giantswarm/beekeeper/internal/findings"
	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/state"
)

type findingHost struct {
	Host
	store state.Store
}

func (h findingHost) Store() state.Store                                             { return h.store }
func (h findingHost) LoopGitHub(_ context.Context, _ string, run func() error) error { return run() }

func TestFindingReceiptsRespectExplicitTargetsAcrossRestarts(t *testing.T) {
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	previous := github.RunGH
	t.Cleanup(func() { github.RunGH = previous })
	reads := 0
	github.RunGH = func(context.Context, ...string) ([]byte, error) {
		reads++
		return []byte(`[[{"number":1,"title":"Queue stalls","body":"Facts"},{"number":2,"title":"Queue stalls","body":"Facts"}]]`), nil
	}
	for _, number := range []int{1, 2, 1, 2} {
		// A fresh loop reads the durable receipts on each pass.
		l := Loop{Host: findingHost{store: store}}
		url, err := l.placeFinding(context.Background(), findings.Report{Repo: "example/app", Number: number, Title: "Queue stalls", Body: "Facts"})
		if err != nil || url != fmt.Sprintf("https://github.com/example/app/issues/%d", number) {
			t.Fatalf("target %d: %s %v", number, url, err)
		}
	}
	if reads != 2 {
		t.Fatalf("read %d times, want one per explicit target", reads)
	}
	l := Loop{Host: findingHost{store: store}}
	if _, err := l.placeFinding(context.Background(), findings.Report{Repo: "example/app", Title: "Queue stalls", Body: "Facts"}); !errors.Is(err, findings.ErrUnplaced) {
		t.Fatalf("ambiguous receipt: %v", err)
	}
}
