package cmd

import (
	"reflect"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/superloop"
)

func TestTakenFoldFoldsOnlyAppendedHistory(t *testing.T) {
	at := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	ev := func(i int, verb, detail string) state.Event {
		return state.Event{At: at.Add(time.Duration(i) * time.Second), By: loopParty, Verb: verb, Subject: reserveIssue, Detail: detail}
	}
	history := []state.Event{
		ev(0, loopClaimVerb, "[go o/r#1] o/r#1"),
		ev(1, loopStageVerb, "[release o/r] tagged"),
		ev(2, "supervisor."+superloop.KindGo, "[go o/r#2] worker: merged"),
		ev(3, loopStageVerb, "[intake o/r#3] read"),
	}
	appended := []state.Event{
		ev(4, loopUnstageVerb, "[release o/r] rolled back"),
		ev(5, loopClaimEndVerb, "[go o/r#2] done"),
		ev(6, loopClaimEndVerb, "[go o/r#1] done"),
	}
	all := append(append([]state.Event{}, history...), appended...)

	var whole takenFold
	want := whole.fold(all)

	var f takenFold
	first := f.fold(history)
	if len(first.taken) != 1 || !first.stages["release o/r"] || first.claims["go o/r#1"].target != reserveIssue {
		t.Fatalf("first fold = %+v", first)
	}
	// A pass changes its copy; the fold keeps what the history says.
	delete(first.claims, "go o/r#1")
	first.stages["changed"] = true
	if got := f.fold(all); !reflect.DeepEqual(got, want) {
		t.Fatalf("incremental fold = %+v, want %+v", got, want)
	}
	if len(want.taken) != 0 || len(want.claims) != 0 || want.stages["release o/r"] || !want.stages["intake o/r#3"] {
		t.Fatalf("fold = %+v", want)
	}

	// A history that is not the one folded is folded from the start.
	other := append([]state.Event{ev(0, loopStageVerb, "[other] x")}, appended...)
	var fresh takenFold
	if got, want := f.fold(other), fresh.fold(other); !reflect.DeepEqual(got, want) {
		t.Fatalf("fold of another history = %+v, want %+v", got, want)
	}
	if got := f.fold(history[:2]); len(got.stages) != 1 || !got.stages["release o/r"] || got.claims["go o/r#1"].target != reserveIssue {
		t.Fatalf("fold of a shorter history = %+v", got)
	}
}
