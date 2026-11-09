package cmd

import (
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/state"
)

func TestGuideRecoverySettlesBeforeEitherFeed(t *testing.T) {
	st := &state.State{Guide: &state.Role{Holder: &state.Supervisor{Party: state.Party{Session: "guide"}}}}
	report := "report" + fileReportSeparator + "DELIVERY STALLED fixture"
	events := []state.Event{
		{At: relayNow, Detail: "[delivery fixture] " + report},
		{At: relayNow.Add(time.Minute), Detail: "[delivery recovered fixture] report" + fileReportSeparator + "RECOVERED: DELIVERY STALLED fixture"},
	}
	lines, _ := guideProblemLines(st, events)
	if len(lines) != 0 || len(st.GuideWork) != 0 {
		t.Fatalf("recovered batch announced: %q %+v", lines, st.GuideWork)
	}
	retainGuideReport(st, "delivery another", report, relayNow, state.Party{})
	retainGuideReport(st, "delivery recovered another", "report"+fileReportSeparator+"RECOVERED: DELIVERY STALLED fixture", relayNow, state.Party{})
	if len(st.GuideWork) != 0 {
		t.Fatal("recovery left due work")
	}
}

func TestGuideReportHasOneDeliveryAcrossFeedsAndRearm(t *testing.T) {
	st := &state.State{Guide: &state.Role{Holder: &state.Supervisor{Party: state.Party{Session: "guide"}}}}
	ev := state.Event{At: relayNow, Detail: "[fixture] report" + fileReportSeparator + "new defect"}
	key, text, _ := keyed(ev.Detail)
	if !retainGuideReport(st, key, text, ev.At, state.Party{}) {
		t.Fatal("monitor did not retain work")
	}
	for range 3 {
		lines, _ := guideProblemLines(st, []state.Event{ev})
		if len(lines) != 0 {
			t.Fatalf("foreground duplicated monitor: %q", lines)
		}
		st.Guide.Fed = nil // a new watch must still respect durable report ownership
	}
	if len(st.GuideWork) != 1 {
		t.Fatal("duplicate work")
	}
}

func TestGuideKnownFindingTalliesWithoutNewWork(t *testing.T) {
	st := &state.State{Guide: &state.Role{}}
	text := "report at now" + fileReportSeparator + "same defect"
	fingerprint := guideFindingFingerprint(text)
	st.GuideFindings = map[string]state.GuideFinding{fingerprint: {Issue: "https://github.com/o/r/issues/1", Count: 1}}
	if retainGuideReport(st, "new observation", "another reporter"+fileReportSeparator+"same defect", relayNow, state.Party{}) {
		t.Fatal("known issue announced")
	}
	if len(st.GuideWork) != 0 || st.GuideFindings[fingerprint].Count != 2 {
		t.Fatalf("unexpected finding state: %+v", st)
	}
	if !strings.Contains(guideWorkText(st), "observations: 2") {
		t.Fatal("tally not visible")
	}
}

func TestDeliveryDoesNotDiagnoseSecondsOldRelease(t *testing.T) {
	a, _ := supervisorApp(t)
	now := a.timeNow()
	seed(t, a, func(st *state.State) {
		st.Deliveries = []state.Delivery{{Ref: refOne, Step: state.DeliveryReleasing, Since: now.Add(-4 * time.Second)}}
	})
	if line, err := a.checkDeliveries(now); err != nil || line != "" {
		t.Fatalf("premature alarm: %q %v", line, err)
	}
}
