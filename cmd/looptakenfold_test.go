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
		return state.Event{At: at.Add(time.Duration(i) * time.Second), By: LoopParty, Verb: verb, Subject: reserveIssue, Detail: detail}
	}
	history := []state.Event{
		ev(0, LoopClaimVerb, "[go o/r#1] o/r#1"),
		ev(1, LoopStageVerb, "[release o/r] tagged"),
		ev(2, "supervisor."+superloop.KindGo, "[go o/r#2] worker: merged"),
		ev(3, LoopStageVerb, "[intake o/r#3] read"),
	}
	appended := []state.Event{
		ev(4, LoopUnstageVerb, "[release o/r] rolled back"),
		ev(5, LoopClaimEndVerb, "[go o/r#2] done"),
		ev(6, LoopClaimEndVerb, "[go o/r#1] done"),
	}
	all := append(append([]state.Event{}, history...), appended...)

	var whole TakenFold
	want := whole.Fold(all)

	var f TakenFold
	first := f.Fold(history)
	if len(first.Taken) != 1 || !first.Stages["release o/r"] || first.Claims["go o/r#1"].Target != reserveIssue {
		t.Fatalf("first fold = %+v", first)
	}
	// A pass changes its copy; the fold keeps what the history says.
	delete(first.Claims, "go o/r#1")
	first.Stages["changed"] = true
	if got := f.Fold(all); !reflect.DeepEqual(got, want) {
		t.Fatalf("incremental fold = %+v, want %+v", got, want)
	}
	if len(want.Taken) != 0 || len(want.Claims) != 0 || want.Stages["release o/r"] || !want.Stages["intake o/r#3"] {
		t.Fatalf("fold = %+v", want)
	}

	// A history that is not the one folded is folded from the start.
	other := append([]state.Event{ev(0, LoopStageVerb, "[other] x")}, appended...)
	var fresh TakenFold
	if got, want := f.Fold(other), fresh.Fold(other); !reflect.DeepEqual(got, want) {
		t.Fatalf("fold of another history = %+v, want %+v", got, want)
	}
	if got := f.Fold(history[:2]); len(got.Stages) != 1 || !got.Stages["release o/r"] || got.Claims["go o/r#1"].Target != reserveIssue {
		t.Fatalf("fold of a shorter history = %+v", got)
	}
}
