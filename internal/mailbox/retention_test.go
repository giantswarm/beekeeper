package mailbox_test

import (
	"context"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/mailbox"
	"github.com/giantswarm/beekeeper/internal/mailbox/mailboxtest"
)

func TestMailboxReusedIDKeepsPendingDelivery(t *testing.T) {
	for _, tc := range []struct {
		name string
		to   string
	}{
		{name: "case 0: same mailbox", to: bob},
		{name: "case 1: another mailbox", to: ana},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := open(t, mailboxtest.URL(t))
			ctx := context.Background()
			original := msg(bob, ana, "reused")
			original.Deadline = t0.Add(3 * mailbox.DedupeWindow)
			first := send(t, s, original, t0)
			later := t0.Add(mailbox.DedupeWindow + time.Hour)
			second := send(t, s, msg(tc.to, ana, original.ID), later)
			if second.Duplicate || second.Seq == first.Seq {
				t.Fatalf("ID outside its window was not new: %+v", second)
			}
			if again := send(t, s, msg(tc.to, ana, original.ID), later); !again.Duplicate || again.Seq != second.Seq {
				t.Fatalf("retry of the new delivery: %+v", again)
			}
			ds := receive(t, s, bob, later)
			want := 1
			if tc.to == bob {
				want = 2
			}
			if len(ds) != want || ds[0].Seq != first.Seq {
				t.Fatalf("original delivery disappeared before its deadline: %+v", ds)
			}
			if n, err := s.Ack(ctx, bob, []int64{first.Seq}, later); err != nil || n != 1 {
				t.Fatalf("ack the original delivery: %d, %v", n, err)
			}
			ds = receive(t, s, tc.to, later)
			if len(ds) != 1 || ds[0].Seq != second.Seq {
				t.Fatalf("acking the original changed the new delivery: %+v", ds)
			}
		})
	}
}

func TestMailboxReusedIDKeepsExpiryNotice(t *testing.T) {
	s := open(t, mailboxtest.URL(t))
	first := send(t, s, msg(bob, ana, "reused-before-expiry-sweep"), t0)
	later := t0.Add(mailbox.DedupeWindow + time.Hour)
	second := send(t, s, msg(bob, ana, "reused-before-expiry-sweep"), later)
	exp, err := s.Expire(context.Background(), later)
	if err != nil || len(exp) != 1 || exp[0].Seq != first.Seq {
		t.Fatalf("the old delivery must expire with a notice: %+v, %v", exp, err)
	}
	if ds := receive(t, s, ana, later); len(ds) != 1 || ds[0].Kind != mailbox.KindExpired {
		t.Fatalf("missing expiry notice: %+v", ds)
	}
	if ds := receive(t, s, bob, later); len(ds) != 1 || ds[0].Seq != second.Seq {
		t.Fatalf("expiry changed the new delivery: %+v", ds)
	}
}
