package cmd

import (
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/state"
)

const (
	refillZeroLine          = "SUPERVISOR REFILL started 0: "
	refillFullCase          = "full WIP"
	refillStaleCase         = "stale WIP"
	refillEmptyCase         = "empty"
	refillCapacityErrorCase = "capacity error"
)

// Every unproductive pass explains its current gate, even when sayOnce has
// already emitted its detailed warning. Admission recovery must need no restart.
func TestRefillZeroStartReasons(t *testing.T) {
	for _, tc := range []struct{ name, want string }{
		{LoopCapacity, "no room for a worker"},
		{refillCapacityErrorCase, "state lock is busy"},
		{reviewedHeldCase, "held: starts held"},
		{refillEmptyCase, "no free board item"},
		{"budget", "GitHub budget under github.floor"},
		{refillFullCase, "wip full: 1 of 1"},
		{refillStaleCase, "wip stale"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, busy := fillRig(t)
			now := r.a.timeNow()
			original := r.answer
			r.answer = func(args []string) (string, int) {
				if args[0] == fillCapacityCommand {
					switch tc.name {
					case LoopCapacity:
						return fillNoRoomJSON, 0
					case refillCapacityErrorCase:
						return "the state lock is busy", 1
					case reviewedHeldCase:
						return `{"ceiling":3,"room":0,"stopped":"starts held"}`, 0
					}
				}
				if tc.name == refillEmptyCase && args[0] == LoopBoard {
					return fillEmptyBoardJSON, ExitRefused
				}
				return original(args)
			}
			seed(t, r.a, func(st *state.State) {
				switch tc.name {
				case "budget":
					r.a.cfg.GitHub.Floor = 100
					st.Budget = &state.Budget{Remaining: 0, Limit: 5000, At: now, Reset: now.Add(time.Hour)}
				case refillFullCase, refillStaleCase:
					r.a.cfg.Delivery.WIPLimit = 1
					st.DeliveriesAt = now
					if tc.name == refillStaleCase {
						st.DeliveriesAt = now.Add(-time.Hour)
					}
					st.StartDelivery("o/r#99", "existing worker", "", now)
				}
			})
			l, err := r.a.newSupervisorLoop(false)
			if err != nil {
				t.Fatal(err)
			}
			for range 2 {
				st, err := r.a.store.Read()
				if err != nil {
					t.Fatal(err)
				}
				// A real pass resets the per-pass board cache before dispatch.
				l.BoardNone = nil
				if err := l.Fill(t.Context(), st, nil, nil); err != nil {
					t.Fatal(err)
				}
			}
			if *busy != 0 {
				t.Fatalf("gate bypassed: %d starts", *busy)
			}
			lines := 0
			for _, line := range strings.Split(r.out.String(), "\n") {
				if strings.HasPrefix(line, refillZeroLine) {
					lines++
					if !strings.Contains(line, tc.want) && (tc.name != refillEmptyCase || !strings.Contains(line, "backoff until")) {
						t.Fatalf("missing reason %q: %s", tc.want, line)
					}
				}
			}
			if lines != 2 {
				t.Fatalf("want one summary per pass, got %d: %s", lines, r.out)
			}
			r.answer = original
			seed(t, r.a, func(st *state.State) {
				st.Budget = &state.Budget{
					Remaining: 5000, Limit: 5000, At: now, Reset: now.Add(time.Hour),
					GraphQL: &state.GraphQL{Remaining: 5000, Limit: 5000, At: now, Reset: now.Add(time.Hour)},
				}
				st.DeliveriesAt = now
				for i := range st.Deliveries {
					st.Deliveries[i].Step = state.DeliveryDone
				}
			})
			l.BoardNone = nil
			l.FillPause = FillPause{}
			st, err := r.a.store.Read()
			if err != nil {
				t.Fatal(err)
			}
			r.out.Reset()
			if err := l.Fill(t.Context(), st, nil, nil); err != nil {
				t.Fatal(err)
			}
			if *busy == 0 {
				t.Fatalf("did not resume after gate cleared: %s", r.out)
			}
			if strings.Contains(r.out.String(), refillZeroLine) {
				t.Fatalf("reported zero after a start: %s", r.out)
			}
		})
	}
}

func TestRefillReportsPriorPassStillRunning(t *testing.T) {
	r, busy := fillRig(t)
	l, err := r.a.newSupervisorLoop(false)
	if err != nil {
		t.Fatal(err)
	}
	l.Once = true
	if !l.BeginInFlight(LoopFillStream) {
		t.Fatal("could not reserve refill stream")
	}
	defer l.EndInFlight(LoopFillStream)
	for range 2 {
		st, err := r.a.store.Read()
		if err != nil {
			t.Fatal(err)
		}
		if err := l.RunPassActions(t.Context(), st, nil, nil, nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	if *busy != 0 {
		t.Fatalf("ran overlapping refill: %d", *busy)
	}
	if got := strings.Count(r.out.String(), refillZeroLine+"previous refill still running"); got != 2 {
		t.Fatalf("want one wait per pass, got %d: %s", got, r.out)
	}
}
