package mailbox_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/mailbox"
	"github.com/giantswarm/beekeeper/internal/mailbox/mailboxtest"
)

const (
	ana = "ana@example.com"
	bob = "Bob@example.com"
	// bobKey is bob's mailbox key.
	bobKey = "bob@example.com"
)

var t0 = time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)

func open(t *testing.T, url string) *mailbox.Store {
	t.Helper()
	s, err := mailbox.Open(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s
}

func msg(to, from, id string) mailbox.Message {
	env, _ := json.Marshal(map[string]any{"messageId": id, "role": "user", "parts": []any{map[string]any{"text": id}}})
	return mailbox.Message{Mailbox: to, Sender: from, ID: id, Envelope: env}
}

func send(t *testing.T, s *mailbox.Store, m mailbox.Message, now time.Time) mailbox.Sent {
	t.Helper()
	out, err := s.Send(context.Background(), m, now)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func ids(ds []mailbox.Delivery) []string {
	out := []string{}
	for _, d := range ds {
		out = append(out, d.MessageID)
	}
	return out
}

func receive(t *testing.T, s *mailbox.Store, mb string, now time.Time) []mailbox.Delivery {
	t.Helper()
	ds, err := s.Receive(context.Background(), mb, 0, now)
	if err != nil {
		t.Fatal(err)
	}
	return ds
}

func TestMailboxOrderAckAndRestart(t *testing.T) {
	url := mailboxtest.URL(t)
	ctx := context.Background()
	s := open(t, url)
	for _, id := range []string{"m1", "m2", "m3"} {
		send(t, s, msg(bob, ana, id), t0)
	}
	ds := receive(t, s, bobKey, t0)
	if got := fmt.Sprint(ids(ds)); got != "[m1 m2 m3]" {
		t.Fatalf("received %s, want the sender's order", got)
	}
	if ds[0].Sender != ana || ds[0].Kind != mailbox.KindMessage {
		t.Fatalf("delivery %+v", ds[0])
	}
	if n, err := s.Ack(ctx, bob, []int64{ds[0].Seq}, t0); err != nil || n != 1 {
		t.Fatalf("ack: %d, %v", n, err)
	}
	// Another person's ack of bob's delivery acks nothing.
	if n, err := s.Ack(ctx, ana, []int64{ds[1].Seq}, t0); err != nil || n != 0 {
		t.Fatalf("ana acked bob's delivery: %d, %v", n, err)
	}

	// A restart: a fresh store over the same database, the schema migrated
	// again, keeps what was not acked.
	s.Close()
	s = open(t, url)
	if got := fmt.Sprint(ids(receive(t, s, bob, t0))); got != "[m2 m3]" {
		t.Fatalf("after a restart %s, want [m2 m3]", got)
	}
}

func TestMailboxCap(t *testing.T) {
	s := open(t, mailboxtest.URL(t))
	for i := range mailbox.Cap {
		send(t, s, msg(bob, ana, fmt.Sprintf("m%d", i)), t0)
	}
	_, err := s.Send(context.Background(), msg(bob, ana, "one-too-many"), t0)
	if !errors.Is(err, mailbox.ErrFull) {
		t.Fatalf("the 51st: %v, want ErrFull", err)
	}
	// An ack makes room.
	ds := receive(t, s, bob, t0)
	if _, err := s.Ack(context.Background(), bob, []int64{ds[0].Seq}, t0); err != nil {
		t.Fatal(err)
	}
	send(t, s, msg(bob, ana, "one-too-many"), t0)
}

func TestMailboxDedupe(t *testing.T) {
	s := open(t, mailboxtest.URL(t))
	ctx := context.Background()
	first := send(t, s, msg(bob, ana, "m1"), t0)
	again := send(t, s, msg(bob, ana, "m1"), t0.Add(time.Minute))
	if !again.Duplicate || again.Seq != first.Seq {
		t.Fatalf("resend %+v, want a duplicate of %+v", again, first)
	}
	// Another sender's message of the same id is its own.
	if other := send(t, s, msg(bob, "carl@example.com", "m1"), t0); other.Duplicate {
		t.Fatal("another sender's id was taken for a duplicate")
	}
	ds := receive(t, s, bob, t0)
	if len(ds) != 2 {
		t.Fatalf("%d deliveries, want 2", len(ds))
	}
	// Acked and resent within the window: still a duplicate, nothing queued.
	if _, err := s.Ack(ctx, bob, []int64{first.Seq}, t0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Expire(ctx, t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if again := send(t, s, msg(bob, ana, "m1"), t0.Add(2*time.Hour)); !again.Duplicate {
		t.Fatal("an acked message was queued again within the dedupe window")
	}
	// Past the window the id is new.
	later := t0.Add(mailbox.DedupeWindow + time.Hour)
	if _, err := s.Expire(ctx, later); err != nil {
		t.Fatal(err)
	}
	if again := send(t, s, msg(bob, ana, "m1"), later); again.Duplicate {
		t.Fatal("an id older than the window was taken for a duplicate")
	}
}

func TestMailboxExpiry(t *testing.T) {
	s := open(t, mailboxtest.URL(t))
	ctx := context.Background()
	short := msg(bob, ana, "short")
	short.Deadline = t0.Add(time.Hour)
	send(t, s, short, t0)
	send(t, s, msg(bob, ana, "default"), t0)

	// Past its deadline a message is no longer delivered, even before the
	// loop takes it out.
	if got := fmt.Sprint(ids(receive(t, s, bob, t0.Add(2*time.Hour)))); got != "[default]" {
		t.Fatalf("received %s past the deadline", got)
	}
	exp, err := s.Expire(ctx, t0.Add(2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(exp) != 1 || exp[0].MessageID != "short" || exp[0].Sender != ana || exp[0].Mailbox != bobKey {
		t.Fatalf("expired %+v", exp)
	}
	notices := receive(t, s, ana, t0.Add(2*time.Hour))
	if len(notices) != 1 || notices[0].Kind != mailbox.KindExpired {
		t.Fatalf("ana's mailbox %+v, want the expired notice", notices)
	}
	var env map[string]any
	if err := json.Unmarshal(notices[0].Envelope, &env); err != nil || env["messageId"] != "short" || env["to"] != bobKey {
		t.Fatalf("notice %s", notices[0].Envelope)
	}
	// Without a deadline a message lives TTL; the notice expires without a
	// notice of its own.
	exp, err = s.Expire(ctx, t0.Add(mailbox.TTL+3*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(exp) != 1 || exp[0].MessageID != "default" {
		t.Fatalf("expired %+v", exp)
	}
	if n, err := s.Pending(ctx, ana, t0.Add(mailbox.TTL+3*time.Hour)); err != nil || n != 1 {
		t.Fatalf("ana's pending %d, %v: want only the second notice", n, err)
	}
	// An expired notice does not count against the cap.
	for i := range mailbox.Cap {
		send(t, s, msg(ana, bob, fmt.Sprintf("m%d", i)), t0.Add(mailbox.TTL+3*time.Hour))
	}
}

func TestMailboxRefusesAPassedDeadline(t *testing.T) {
	s := open(t, mailboxtest.URL(t))
	m := msg(bob, ana, "late")
	m.Deadline = t0.Add(-time.Minute)
	if _, err := s.Send(context.Background(), m, t0); err == nil {
		t.Fatal("a message past its deadline was queued")
	}
}

func TestMailboxListen(t *testing.T) {
	s := open(t, mailboxtest.URL(t))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	changed := make(chan string, 10)
	ready := make(chan struct{}, 1)
	go s.Listen(ctx, func(mb string) { changed <- mb }, func() { ready <- struct{}{} }, func(err error) { t.Log("listen:", err) })
	<-ready
	sent := send(t, s, msg(bob, ana, "m1"), t0)
	expect := func(what string) {
		t.Helper()
		select {
		case mb := <-changed:
			if mb != bobKey {
				t.Fatalf("%s announced %q", what, mb)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s was not announced", what)
		}
	}
	expect("the send")
	if _, err := s.Ack(context.Background(), bob, []int64{sent.Seq}, t0); err != nil {
		t.Fatal(err)
	}
	expect("the ack")
}
