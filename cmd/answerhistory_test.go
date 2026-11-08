package cmd

import (
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/state"
)

func TestDecisionHistorySurvivesAgeAndIncludesVerbatimSource(t *testing.T) {
	a, out := noteApp(t)
	words := "Proceed.\nKeep the existing limit."
	e := answerEvent(state.Party{Name: "recorder"}, &recordedAnswer{About: "o/r#1", By: "owner", Via: viaCLI, Answer: words}, nil)
	e.At = a.now.Add(-365 * 24 * time.Hour)
	if err := a.store.Log(e); err != nil {
		t.Fatal(err)
	}
	other := answerEvent(state.Party{Name: "peer"}, &recordedAnswer{About: "o/r#2", By: "owner", Via: viaSlack, Answer: "Wait."}, nil)
	other.At = a.now
	if err := a.store.Log(other); err != nil {
		t.Fatal(err)
	}
	events, err := a.decisionHistory("o/r#1")
	if err != nil || len(events) != 1 {
		t.Fatalf("history: %v, %v", events, err)
	}
	text := decisionHistoryText(events)
	for _, want := range []string{words, "recorder", "owner", "via cli", e.At.Format(time.RFC3339)} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in %s", want, text)
		}
	}
	if err := a.printGuidePrompt(state.Role{}, nil, "", nil, &state.State{}); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); !strings.Contains(got, words) || !strings.Contains(got, "Wait.") {
		t.Fatalf("fresh guide lost recorded decisions: %s", got)
	}
}
