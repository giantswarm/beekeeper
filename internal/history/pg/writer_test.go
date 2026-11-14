package historypg_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/go-logr/logr"

	"github.com/giantswarm/beekeeper/internal/clock"
	"github.com/giantswarm/beekeeper/internal/history"
	historypg "github.com/giantswarm/beekeeper/internal/history/pg"
	"github.com/giantswarm/beekeeper/internal/settle"
	"github.com/giantswarm/beekeeper/internal/testing/pgtest"
)

func TestMain(m *testing.M) { pgtest.Main(m) }

var t0 = time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)

// Fixtures of the rows written and read back.
const (
	team          = "bumblebee"
	alice         = "alice"
	refRoadmap101 = "giantswarm/roadmap#101"
	refOR1        = "o/r#1"
	refOR2        = "o/r#2"

	typeObserved  = "observed"
	sourceWebhook = "webhook"
	sourceResync  = "resync"
	pointReady    = "ready?"
	pointNext     = "next?"
	answerReady   = "ready"
)

type env struct {
	db      *pgtest.DB
	clock   *clock.Fake
	tracker *settle.Tracker
	w       *historypg.Writer
}

func newEnv(t *testing.T, opts historypg.Options) *env {
	t.Helper()
	e := &env{db: pgtest.NewDB(t), clock: clock.NewFake(t0), tracker: settle.New()}
	opts.Tracker = e.tracker
	if opts.RetryInterval == 0 {
		// Long, so tests prove the settle probe (not the retry timer) writes once Postgres is back.
		opts.RetryInterval = time.Hour
	}
	e.w = historypg.NewWriter(e.db.Pool(), e.clock, logr.Discard(), opts)
	t.Cleanup(func() { _ = e.w.Close(context.Background()) })
	return e
}

func (e *env) settle(t *testing.T) {
	t.Helper()
	if err := e.tracker.Wait(context.Background(), 5*time.Second); err != nil {
		t.Fatal(err)
	}
}

func TestEventsWrittenInOrderWithTheirTimes(t *testing.T) {
	t.Parallel()
	e := newEnv(t, historypg.Options{BatchSize: 7})
	var want []history.Event
	for i := range 50 {
		ev := history.Event{
			At:     t0.Add(time.Duration(i) * time.Minute),
			Team:   team,
			Item:   refRoadmap101,
			Type:   typeObserved,
			Source: sourceWebhook,
			Data:   map[string]any{"n": float64(i), "fields": []any{"board.status"}},
		}
		want = append(want, ev)
		e.w.Append(ev)
	}
	e.clock.Advance(time.Hour)
	e.w.Append(history.Event{Change: "giantswarm/klaus#55", Type: "wait.started", Source: "engine"}) // no time: the clock's
	e.settle(t)
	if p := e.w.Pending(); p != 0 {
		t.Fatalf("pending = %d after settling", p)
	}
	got, err := e.w.Events(context.Background(), history.EventFilter{Item: refRoadmap101})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Fatalf("got %d events, want %d", len(got), len(want))
	}
	for i, g := range got {
		if !g.At.Equal(want[i].At) || g.Data["n"] != want[i].Data["n"] || g.Team != team || g.Source != sourceWebhook {
			t.Fatalf("event %d = %+v, want %+v", i, g, want[i])
		}
		if i > 0 && g.ID <= got[i-1].ID {
			t.Fatalf("ids not ascending: %d after %d", g.ID, got[i-1].ID)
		}
	}
	stamped, err := e.w.Events(context.Background(), history.EventFilter{Change: "giantswarm/klaus#55"})
	if err != nil {
		t.Fatal(err)
	}
	if len(stamped) != 1 || !stamped[0].At.Equal(t0.Add(time.Hour)) || stamped[0].Data == nil {
		t.Fatalf("unstamped event = %+v, want at %s from the clock", stamped, t0.Add(time.Hour))
	}
}

func TestFilters(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := newEnv(t, historypg.Options{})
	e.w.Append(
		history.Event{At: t0, Item: refOR1, Type: typeObserved, Source: sourceWebhook},
		history.Event{At: t0, Change: refOR2, Type: typeObserved, Source: sourceResync},
		history.Event{At: t0, Item: refOR1, Type: "phase.changed", Source: "engine"},
	)
	e.w.Decide(history.Decision{At: t0, Point: pointReady, Subject: refOR1, InputsHash: "h1"})
	e.w.Decide(history.Decision{At: t0, Point: pointNext, Subject: alice, InputsHash: "h1"})
	e.settle(t)
	evMark, decMark, err := e.w.MaxIDs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if evMark == 0 || decMark == 0 {
		t.Fatalf("max ids = %d, %d", evMark, decMark)
	}
	e.w.Append(history.Event{At: t0.Add(time.Minute), Item: refOR1, Type: typeObserved, Source: sourceResync})
	e.w.Decide(history.Decision{At: t0.Add(time.Minute), Point: pointReady, Subject: refOR1, InputsHash: "h2"})
	e.settle(t)

	events := []struct {
		f    history.EventFilter
		want int
	}{
		{history.EventFilter{}, 4},
		{history.EventFilter{Item: refOR1}, 3},
		{history.EventFilter{Change: refOR2}, 1},
		{history.EventFilter{Type: typeObserved}, 3},
		{history.EventFilter{Source: sourceResync}, 2},
		{history.EventFilter{Item: refOR1, Type: typeObserved, Source: sourceResync}, 1},
		{history.EventFilter{AfterID: evMark}, 1},
		{history.EventFilter{Item: refOR1, AfterID: evMark}, 1},
		{history.EventFilter{Change: refOR2, AfterID: evMark}, 0},
	}
	for _, c := range events {
		got, err := e.w.Events(ctx, c.f)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != c.want {
			t.Errorf("Events(%+v) = %d rows, want %d", c.f, len(got), c.want)
		}
	}
	decisions := []struct {
		f    history.DecisionFilter
		want int
	}{
		{history.DecisionFilter{}, 3},
		{history.DecisionFilter{Point: pointReady}, 2},
		{history.DecisionFilter{Subject: alice}, 1},
		{history.DecisionFilter{AfterID: decMark}, 1},
		{history.DecisionFilter{Point: pointNext, AfterID: decMark}, 0},
	}
	for _, c := range decisions {
		got, err := e.w.Decisions(ctx, c.f)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != c.want {
			t.Errorf("Decisions(%+v) = %d rows, want %d", c.f, len(got), c.want)
		}
	}
}

func TestDecideOncePerKeyAndAnswer(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := newEnv(t, historypg.Options{})
	d := history.Decision{
		At: t0, Point: pointReady, Subject: refRoadmap101, InputsHash: "abc",
		Inputs: map[string]any{"body": "x"}, Rule: answerReady, Chosen: answerReady, Mode: "shadow", Team: team,
	}
	if !e.w.Decide(d) {
		t.Fatal("first Decide reported a known key")
	}
	if e.w.Decide(d) {
		t.Fatal("second Decide of the same key reported a new one")
	}
	other := d
	other.InputsHash = "def"
	if !e.w.Decide(other) {
		t.Fatal("other inputs are a new key")
	}
	e.w.Answer(d.Key(), "not-ready", 0.9, "", "disagree")
	e.settle(t)
	got, err := e.w.Decisions(ctx, history.DecisionFilter{Point: pointReady})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d decisions, want 2", len(got))
	}
	g := got[0]
	if g.InputsHash != "abc" || g.JEV != "not-ready" || g.JEVConfidence != 0.9 || g.Outcome != "disagree" ||
		g.Rule != answerReady || g.Chosen != answerReady || g.Mode != "shadow" || g.Team != team || g.Inputs["body"] != "x" ||
		!g.At.Equal(t0) {
		t.Fatalf("decision = %+v", g)
	}
	if got[1].Outcome != "" || got[1].JEV != "" {
		t.Fatalf("unanswered decision = %+v", got[1])
	}

	// A writer started later (another replica, a restart) knows the recorded keys.
	w2 := historypg.NewWriter(e.db.Pool(), e.clock, logr.Discard(), historypg.Options{})
	defer func() { _ = w2.Close(ctx) }() // what it couldn't write is not what this test checks
	if w2.Decide(d) || w2.Decide(other) {
		t.Fatal("a new writer decided a recorded key again")
	}
	keys, err := w2.DecisionKeys(ctx)
	if err != nil || len(keys) != 2 {
		t.Fatalf("DecisionKeys = %v, %v", keys, err)
	}
}

// A writer that couldn't read the keys at startup reads them before its first write, and the unique key
// keeps a decision from being recorded twice anyway.
func TestDecideRecordedOnceWhenKeysUnknownAtStart(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := newEnv(t, historypg.Options{})
	d := history.Decision{At: t0, Point: pointReady, Subject: refOR1, InputsHash: "h"}
	e.w.Decide(d)
	e.settle(t)

	if err := e.db.Down(); err != nil {
		t.Fatal(err)
	}
	tr := settle.New()
	w2 := historypg.NewWriter(e.db.Pool(), e.clock, logr.Discard(), historypg.Options{Tracker: tr, RetryInterval: time.Hour})
	defer func() { _ = w2.Close(ctx) }() // what it couldn't write is not what this test checks
	if !w2.Decide(d) {
		t.Fatal("without the keys the writer can't know the decision")
	}
	if err := tr.Wait(ctx, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := e.db.Up(); err != nil {
		t.Fatal(err)
	}
	if err := tr.Wait(ctx, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	got, err := e.w.Decisions(ctx, history.DecisionFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d decisions, want 1", len(got))
	}
	if w2.Pending() != 0 {
		t.Fatalf("pending = %d", w2.Pending())
	}
}

func TestDurationsOldestFirst(t *testing.T) {
	t.Parallel()
	e := newEnv(t, historypg.Options{})
	e.w.AddDuration(history.DurationCI, "giantswarm/klaus", 30*time.Minute, t0.Add(2*time.Hour))
	e.w.AddDuration(history.DurationCI, "giantswarm/klaus", 20*time.Minute, t0)
	e.w.AddDuration(history.DurationCI, "giantswarm/klaus", 25*time.Minute, t0.Add(time.Hour))
	e.w.AddDuration(history.DurationStage, "bumblebee/Implementing", 2*time.Hour, t0)
	e.settle(t)
	got, err := e.w.Durations(context.Background(), history.DurationCI, "giantswarm/klaus")
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(got) != "[20m0s 25m0s 30m0s]" {
		t.Fatalf("durations = %v", got)
	}
}

// While Postgres is down nothing blocks, rows are buffered up to 10,000 and the rest dropped and counted;
// buffered rows count as idle; once Postgres is back the buffer is written in order.
func TestBufferWhileDownDropAt10000FlushAfterUp(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := newEnv(t, historypg.Options{})
	e.w.Append(history.Event{At: t0, Type: "before"})
	e.settle(t)

	if err := e.db.Down(); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	for i := range historypg.DefaultMaxBuffered + 5 {
		e.w.Append(history.Event{At: t0.Add(time.Duration(i) * time.Second), Type: "during", Data: map[string]any{"i": float64(i)}})
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("appending while down took %s", d)
	}
	if !e.w.Decide(history.Decision{At: t0, Point: pointNext, Subject: alice, InputsHash: "x"}) {
		t.Fatal("decide")
	}
	e.settle(t) // idle although rows are buffered: they wait for Postgres
	if got := e.w.Dropped(); got != 6 {
		t.Fatalf("dropped = %d, want 6 (5 events and the decision past 10,000)", got)
	}
	if got := e.w.Pending(); got != historypg.DefaultMaxBuffered {
		t.Fatalf("pending = %d, want %d", got, historypg.DefaultMaxBuffered)
	}

	if err := e.db.Up(); err != nil {
		t.Fatal(err)
	}
	e.settle(t) // busy until the buffer is written
	if got := e.w.Pending(); got != 0 {
		t.Fatalf("pending after up = %d", got)
	}
	rows, err := e.w.Events(ctx, history.EventFilter{Type: "during"})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != historypg.DefaultMaxBuffered {
		t.Fatalf("wrote %d buffered rows, want %d", len(rows), historypg.DefaultMaxBuffered)
	}
	for i, r := range rows {
		if r.Data["i"] != float64(i) || !r.At.Equal(t0.Add(time.Duration(i)*time.Second)) {
			t.Fatalf("row %d = %+v", i, r)
		}
		if i > 0 && r.ID <= rows[i-1].ID {
			t.Fatal("buffered rows written out of order")
		}
	}
	if e.w.Dropped() != 6 {
		t.Fatalf("dropped = %d after up", e.w.Dropped())
	}
}

// The real-time retry writes the buffer once Postgres is back, without any probe (production).
func TestRetryWithoutTracker(t *testing.T) {
	t.Parallel()
	db := pgtest.NewDB(t)
	w := historypg.NewWriter(db.Pool(), clock.NewFake(t0), logr.Discard(), historypg.Options{RetryInterval: 10 * time.Millisecond})
	defer func() { _ = w.Close(context.Background()) }() // what it couldn't write is not what this test checks
	if err := db.Down(); err != nil {
		t.Fatal(err)
	}
	w.Append(history.Event{At: t0, Type: "x"})
	time.Sleep(50 * time.Millisecond)
	if w.Pending() != 1 {
		t.Fatalf("pending = %d while down", w.Pending())
	}
	if err := db.Up(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for w.Pending() > 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if w.Pending() != 0 {
		t.Fatal("buffer not written after Postgres came back")
	}
}

// A row Postgres refuses (here: a NUL character jsonb can't hold) is dropped and counted; the rest of its
// batch is written.
func TestRefusedRowDropped(t *testing.T) {
	t.Parallel()
	e := newEnv(t, historypg.Options{})
	e.w.Append(
		history.Event{At: t0, Type: "a"},
		history.Event{At: t0, Type: "bad", Data: map[string]any{"s": "nul\x00"}},
		history.Event{At: t0, Type: "c"},
	)
	e.settle(t)
	got, err := e.w.Events(context.Background(), history.EventFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Type != "a" || got[1].Type != "c" {
		t.Fatalf("events = %+v", got)
	}
	if e.w.Dropped() != 1 {
		t.Fatalf("dropped = %d, want 1", e.w.Dropped())
	}
}

func TestCloseWritesBuffer(t *testing.T) {
	t.Parallel()
	db := pgtest.NewDB(t)
	w := historypg.NewWriter(db.Pool(), clock.NewFake(t0), logr.Discard(), historypg.Options{})
	for range 100 {
		w.Append(history.Event{At: t0, Type: "x"})
	}
	if err := w.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.Pool().QueryRow(context.Background(), "SELECT count(*) FROM events").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 100 {
		t.Fatalf("%d rows written before Close returned, want 100", n)
	}
}
