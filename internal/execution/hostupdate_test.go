package execution

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// updateRelease is the release the coordinator updates the fixture host to.
const updateRelease = "v0.6.0"

func TestUpdateHostRunsSelfUpdateAndChecksTheAnswer(t *testing.T) {
	f := testFixture(t)
	digest := Digest([]byte("prepared"))
	for _, tc := range []struct {
		name   string
		answer any
		runErr error
		ok     bool
	}{
		{"pinned", HostUpdate{Protocol: Protocol, Identity: fixtureHost, Version: updateRelease, PolicyDigest: digest}, nil, true},
		{"busy", HostUpdate{Protocol: Protocol, Identity: fixtureHost, Version: "v0.5.0", Busy: 1}, nil, true},
		{"failed", HostUpdate{Protocol: Protocol, Identity: fixtureHost, Error: "no signed release"}, nil, false},
		{"other host", HostUpdate{Protocol: Protocol, Identity: "host-z", Version: updateRelease, PolicyDigest: digest}, nil, false},
		{"other release", HostUpdate{Protocol: Protocol, Identity: fixtureHost, Version: "v0.5.0", PolicyDigest: digest}, nil, false},
		{"no digest", HostUpdate{Protocol: Protocol, Identity: fixtureHost, Version: updateRelease}, nil, false},
		{"older host", map[string]any{"error": "unknown command"}, nil, false},
		{"unreachable", nil, errors.New("connection refused"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var argv []string
			run := func(_ context.Context, a []string, stdin []byte) ([]byte, error) {
				argv = a
				if tc.runErr != nil {
					return nil, tc.runErr
				}
				return json.Marshal(tc.answer)
			}
			x, err := UpdateHost(context.Background(), f.host, updateRelease, run)
			if (err == nil) != tc.ok {
				t.Fatalf("UpdateHost() = %+v, %v", x, err)
			}
			want := quote(f.host.Executable) + " --config " + quote(f.host.Config) + " agents hosts self-update --protocol 1 '--version' '" + updateRelease + "'"
			if len(argv) == 0 || argv[len(argv)-1] != want || argv[len(argv)-2] != f.host.SSH {
				t.Errorf("ran %q, want the host's self-update %q", argv, want)
			}
			var down *UnreachableError
			if tc.runErr != nil && !errors.As(err, &down) {
				t.Errorf("an SSH failure is %T, want unreachable", err)
			}
			if tc.name == "failed" && !strings.Contains(err.Error(), "no signed release") {
				t.Errorf("the host's reason is lost: %v", err)
			}
		})
	}
}

func TestQuiesceHoldsAdmissionAndCountsRunningExecutions(t *testing.T) {
	f := testFixture(t)
	ctx := context.Background()
	prev := quiesceWait
	quiesceWait = 50 * time.Millisecond
	t.Cleanup(func() { quiesceWait = prev })
	busy, release, err := f.s.Quiesce(ctx)
	if err != nil || busy != 0 {
		t.Fatalf("Quiesce() = %d, %v", busy, err)
	}
	if _, _, err := f.s.Quiesce(ctx); err == nil {
		t.Fatal("admission was taken twice")
	}
	during, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	if _, err := f.s.Start(during, f.r); err == nil || !strings.Contains(err.Error(), "admission") {
		t.Fatalf("a start during an update: %v", err)
	}
	release()
	if _, err := f.s.Start(ctx, f.r); err != nil {
		t.Fatal(err)
	}
	busy, release, err = f.s.Quiesce(ctx)
	if err != nil || busy != 1 {
		t.Fatalf("Quiesce() with a started execution = %d, %v", busy, err)
	}
	release()
}

// An execution between turns does not hold an update back, while one in a
// turn, with a wake stored or an uncertain delivery does; and no turn begins
// while the update holds the admission.
func TestQuiesceLeavesAnExecutionBetweenTurnsOutAndHoldsItsNextTurn(t *testing.T) {
	f := testFixture(t)
	ctx := context.Background()
	prevQuiesce, prevTurn := quiesceWait, turnAdmissionWait
	quiesceWait, turnAdmissionWait = 50*time.Millisecond, 50*time.Millisecond
	t.Cleanup(func() { quiesceWait, turnAdmissionWait = prevQuiesce, prevTurn })
	if _, err := f.s.Start(ctx, f.r); err != nil {
		t.Fatal(err)
	}
	set := func(change func(*Journal)) {
		t.Helper()
		if err := f.s.update(ctx, f.r.Ref, func(j *Journal) error {
			j.Phase, j.Ready, j.InTurn, j.Messages = Running, true, false, MessageLedger{}
			change(j)
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	wake := Envelope{ID: "b7c3c1de-7d55-4a43-9f0a-0b3c2f1d9e10", Sequence: 1}
	for name, c := range map[string]struct {
		change func(*Journal)
		busy   int
	}{
		"between turns": {func(*Journal) {}, 0},
		"in a turn":     {func(j *Journal) { j.InTurn = true }, 1},
		"not ready":     {func(j *Journal) { j.Ready = false }, 1},
		"a wake stored": {func(j *Journal) { j.Messages.Entries = map[string]Delivery{wake.ID: {Envelope: wake, Stage: Stored}} }, 1},
		"a delivery uncertain": {func(j *Journal) {
			j.Messages.Entries = map[string]Delivery{wake.ID: {Envelope: wake, Stage: Uncertain}}
		}, 1},
		"a wake accepted": {func(j *Journal) { j.Messages.Entries = map[string]Delivery{wake.ID: {Envelope: wake, Stage: Accepted}} }, 0},
	} {
		set(c.change)
		busy, release, err := f.s.Quiesce(ctx)
		if err != nil || busy != c.busy {
			t.Fatalf("%s: Quiesce() = %d, %v, want %d", name, busy, err, c.busy)
		}
		release()
		// The host's capacity still counts every running execution.
		if n, err := f.s.busy(ctx, Ref{}); err != nil || n != 1 {
			t.Fatalf("%s: busy = %d, %v, want 1", name, n, err)
		}
	}
	set(func(*Journal) {})
	_, release, err := f.s.Quiesce(ctx)
	if err != nil {
		t.Fatal(err)
	}
	begin := func(j *Journal) error { j.InTurn = true; return nil }
	begun, err := f.s.beginTurn(ctx, f.r.Ref, begin)
	if err != nil || begun {
		t.Fatalf("a turn began during an update: %v, %v", begun, err)
	}
	if j, _ := f.s.Read(f.r.Ref); j.InTurn {
		t.Fatal("a held turn changed the journal")
	}
	release()
	if begun, err := f.s.beginTurn(ctx, f.r.Ref, begin); err != nil || !begun {
		t.Fatalf("a turn after the update: %v, %v", begun, err)
	}
	if busy, release, err := f.s.Quiesce(ctx); err != nil || busy != 1 {
		t.Fatalf("Quiesce() during the turn = %d, %v", busy, err)
	} else {
		release()
	}
}

func TestHostCatalogMayLeaveTheDigestToTheState(t *testing.T) {
	f := testFixture(t)
	h := f.host
	h.PolicyDigest = ""
	if err := h.ValidateCatalog(); err != nil {
		t.Fatalf("an entry without a digest: %v", err)
	}
	if err := h.Validate(); err == nil {
		t.Fatal("a host without a pinned digest is admitted to a call")
	}
	h.PolicyDigest = "sha256:x"
	if err := h.ValidateCatalog(); err == nil {
		t.Fatal("an entry with an invalid digest")
	}
}
