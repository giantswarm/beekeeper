package cmd

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/state"
)

const (
	reserveIssue = "o/r#1"
	reservePull  = "o/r#2"
	reserveHead  = "h1"
	reserveFixer = "delivery fixer"
	reserveAsked = "1 review already requested"
)

// A delivery reserves a slot only for a start the loop will make, and the
// reservations never take more than half the room.
func TestDeliveryReservationsReserveOnlyStartsTheLoopMakes(t *testing.T) {
	a, out := supervisorApp(t)
	now := a.timeNow()
	a.cfg.Supervisor.Ownership.Never = []string{"o/never"}
	young := now.Add(-10 * time.Minute)
	queued := func(f func(d *state.Delivery)) state.Delivery {
		d := state.Delivery{Ref: reserveIssue, Pulls: []string{reservePull}, Heads: map[string]string{reservePull: reserveHead}, Step: state.DeliveryReviewQueued, Since: young}
		if f != nil {
			f(&d)
		}
		return d
	}
	many := func(n int) []state.Delivery {
		var ds []state.Delivery
		for i := range n {
			ds = append(ds, state.Delivery{Ref: fmt.Sprintf("o/r#%d", 10+i), Step: state.DeliveryFixing, Since: young})
		}
		return ds
	}
	for _, tc := range []struct {
		name       string
		st         state.State
		room, want int
		skipped    string
	}{
		{name: "owned young review-queued reserves", st: state.State{Deliveries: []state.Delivery{queued(nil)}}, room: 4, want: 3},
		{name: "fixing without a live agent reserves", st: state.State{Deliveries: []state.Delivery{queued(func(d *state.Delivery) { d.Step = state.DeliveryFixing; d.Agent = reserveFixer })}}, room: 4, want: 3},
		{name: "never owned skips", st: state.State{Deliveries: []state.Delivery{queued(func(d *state.Delivery) { d.Ref = "o/never#3"; d.Pulls = nil })}}, room: 4, want: 4, skipped: "1 outside the desk"},
		{name: "never owned pull skips", st: state.State{Deliveries: []state.Delivery{queued(func(d *state.Delivery) { d.Pulls = []string{"o/never#4"} })}}, room: 4, want: 4, skipped: "1 outside the desk"},
		{name: "held repository skips", st: state.State{Deliveries: []state.Delivery{queued(nil)}, Holds: []state.Hold{{Target: repo, At: now}}}, room: 4, want: 4, skipped: "1 under a hold"},
		{name: "lifted hold reserves", st: state.State{Deliveries: []state.Delivery{queued(nil)}, Holds: []state.Hold{{Target: repo, At: now.Add(-time.Hour), Until: now.Add(-time.Minute)}}}, room: 4, want: 3},
		{name: "review requested at head skips", st: state.State{Deliveries: []state.Delivery{queued(nil)}, ReviewRequests: []state.ReviewRequest{{Ref: reservePull, Head: reserveHead, Round: 1}}}, room: 4, want: 4, skipped: reserveAsked},
		{name: "desk review at head skips", st: state.State{Deliveries: []state.Delivery{queued(nil)}, DeskReviews: []state.DeskReview{{Ref: reservePull, Head: reserveHead, Round: 1}}}, room: 4, want: 4, skipped: reserveAsked},
		{name: "review requested at an older head reserves", st: state.State{Deliveries: []state.Delivery{queued(nil)}, ReviewRequests: []state.ReviewRequest{{Ref: reservePull, Head: "h0", Round: 1}}}, room: 4, want: 3},
		{name: "unknown head and any request skips", st: state.State{Deliveries: []state.Delivery{queued(func(d *state.Delivery) { d.Heads = nil })}, ReviewRequests: []state.ReviewRequest{{Ref: reservePull, Head: "h0", Round: 1}}}, room: 4, want: 4, skipped: reserveAsked},
		{name: "old skips", st: state.State{Deliveries: []state.Delivery{queued(func(d *state.Delivery) { d.Since = now.Add(-2 * time.Hour) })}}, room: 4, want: 4, skipped: "1 older than 1h"},
		{name: "fixing with live agent skips", st: state.State{Deliveries: []state.Delivery{queued(func(d *state.Delivery) { d.Step = state.DeliveryFixing; d.Agent = reserveFixer })}, Agents: []state.Agent{{Party: state.Party{Name: reserveFixer}, Task: "fix"}}}, room: 4, want: 4},
		{name: "cap at half the room", st: state.State{Deliveries: many(5)}, room: 4, want: 2},
		{name: "cap rounds down", st: state.State{Deliveries: many(5)}, room: 5, want: 3},
		{name: "room one keeps its slot", st: state.State{Deliveries: many(1)}, room: 1, want: 1},
		{name: "room 0 stays 0", st: state.State{Deliveries: many(3)}, room: 0, want: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out.Reset()
			l := &Loop{Host: a, Said: map[string]bool{}}
			if got := l.DeliveryRoom(tc.room, &tc.st); got != tc.want {
				t.Fatalf("room %d, want %d: %s", got, tc.want, out)
			}
			if tc.skipped != "" && !strings.Contains(out.String(), "not reserved: "+tc.skipped) {
				t.Fatalf("no skip %q: %s", tc.skipped, out)
			}
			if tc.skipped == "" && strings.Contains(out.String(), "not reserved") {
				t.Fatalf("unexpected skip: %s", out)
			}
		})
	}
}

// The reservation line is said once per set of reservations and skips, and
// again when the skips change.
func TestDeliveryReservationsSayOncePerSet(t *testing.T) {
	a, out := supervisorApp(t)
	now := a.timeNow()
	st := &state.State{Deliveries: []state.Delivery{
		{Ref: reserveIssue, Step: state.DeliveryFixing, Since: now},
		{Ref: reservePull, Step: state.DeliveryFixing, Since: now.Add(-2 * time.Hour)},
	}}
	l := &Loop{Host: a, Said: map[string]bool{}}
	_ = l.DeliveryRoom(2, st)
	_ = l.DeliveryRoom(2, st)
	if strings.Count(out.String(), "NEXT RESERVES") != 1 || !strings.Contains(out.String(), "not reserved: 1 older than 1h") {
		t.Fatal(out)
	}
	st.Deliveries[0].Since = now.Add(-2 * time.Hour)
	_ = l.DeliveryRoom(2, st)
	if strings.Count(out.String(), "NEXT RESERVES") != 2 || !strings.Contains(out.String(), "not reserved: 2 older than 1h") {
		t.Fatal(out)
	}
}
