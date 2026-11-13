package loop

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/findings"
	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/superloop"
)

type findingHost struct {
	Host
	store state.Store
	cfg   *config.Config
}

func (h findingHost) Cfg() *config.Config { return h.cfg }

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

func TestFindingWithoutAConfiguredProjectStaysWithTheGuide(t *testing.T) {
	cfg := &config.Config{}
	cfg.Board.Owner = "example"
	cfg.GitHub.AllowedOrgs = []string{"example"}
	l := Loop{Host: findingHost{cfg: cfg}}
	const report = "example/app: Queue stalls\n\nThe queue stalls."
	line, err := l.toFile(context.Background(), &state.State{}, superloop.Action{Message: report, Event: &superloop.Event{Verb: superloop.VerbProblem, Agent: "reporter"}})
	if err != nil || !strings.Contains(line, FileReportSeparator+report) {
		t.Fatalf("lost unplaceable finding: %q %v", line, err)
	}
}

func TestJoinedFinalReportFindingsStayWithTheGuide(t *testing.T) {
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{}
	cfg.Board.Owner = "acme"
	cfg.Board.Project = 1
	cfg.GitHub.AllowedOrgs = []string{"acme"}
	previous := github.RunGH
	t.Cleanup(func() { github.RunGH = previous })
	calls := 0
	github.RunGH = func(context.Context, ...string) ([]byte, error) {
		calls++
		return []byte(`[[]]`), nil
	}
	l := Loop{Host: findingHost{store: store, cfg: cfg}}
	// A done's file action joins its report's unfiled one-line findings. The
	// first one's repository prefix must not turn the rest into an issue body.
	const joined = "acme/app: Queue stalls when the lease expires\nacme/app: Heartbeat lost on reconnect"
	done := superloop.Event{Verb: superloop.VerbDone, Agent: "finder"}
	line, err := l.toFile(context.Background(), &state.State{}, superloop.Action{Kind: superloop.KindFile, Message: joined, Event: &done})
	if err != nil || calls != 0 || !strings.Contains(line, FileReportSeparator+joined) {
		t.Fatalf("joined findings placed as one issue: %q %v, GitHub calls=%d", line, err, calls)
	}
}
