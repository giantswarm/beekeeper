package notify

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// fakeSender records the messages; err makes every send fail.
type fakeSender struct {
	sent []Message
	id   uint32
	err  error
}

func (f *fakeSender) Send(_ context.Context, m Message) (uint32, error) {
	if f.err != nil {
		return 0, f.err
	}
	f.sent = append(f.sent, m)
	f.id++
	return f.id, nil
}

var (
	ctx  = context.Background()
	noon = time.Date(2026, 9, 25, 12, 0, 0, 0, time.Local)
)

func newNotifier(t *testing.T, dir string, p Policy) (*Notifier, *fakeSender, *[]string) {
	t.Helper()
	if p.Kinds == nil {
		p.Kinds = Kinds
	}
	if p.Repeat == 0 {
		p.Repeat = 30 * time.Minute
	}
	s := &fakeSender{}
	var said []string
	return New(p, dir, s, func(l string) { said = append(said, l) }), s, &said
}

func TestOneNotificationPerEventAcrossWatches(t *testing.T) {
	dir := t.TempDir()
	a, sa, _ := newNotifier(t, dir, Policy{})
	b, sb, _ := newNotifier(t, dir, Policy{})
	for range 3 {
		a.Notify(ctx, noon, StaleLease, "agentlab-1@x", "beekeeper: stale lease agentlab-1", "held")
		b.Notify(ctx, noon, StaleLease, "agentlab-1@x", "beekeeper: stale lease agentlab-1", "held")
	}
	if len(sa.sent)+len(sb.sent) != 1 || len(sa.sent) != 1 {
		t.Fatalf("one event, two watches: a sent %d, b sent %d; want 1 and 0", len(sa.sent), len(sb.sent))
	}
	// A lasting condition: once per repeat, whichever watch sees it.
	b.Notify(ctx, noon, OOMLine, "", "s", "LOW RAM")
	a.Notify(ctx, noon.Add(10*time.Minute), OOMLine, "", "s", "LOW RAM")
	a.Notify(ctx, noon.Add(29*time.Minute), OOMLine, "", "s", "SWAP")
	if len(sa.sent) != 1 || len(sb.sent) != 1 {
		t.Fatalf("oom-line within the repeat: a sent %d, b sent %d; want 1 and 1", len(sa.sent), len(sb.sent))
	}
	a.Notify(ctx, noon.Add(31*time.Minute), OOMLine, "", "s", "LOW RAM")
	if len(sa.sent) != 2 || sa.sent[1].Urgency != "critical" {
		t.Fatalf("oom-line after the repeat: %+v", sa.sent)
	}
	// Claim returns only the keys no watch claimed: kills seen by both.
	if got := b.Claim(ctx, noon, OOMKill, "1@1", "2@1"); len(got) != 2 {
		t.Fatalf("fresh kills: %v", got)
	}
	if got := a.Claim(ctx, noon, OOMKill, "2@1", "3@1"); len(got) != 1 || got[0] != "3@1" {
		t.Fatalf("kills partly claimed by the other watch: %v, want [3@1]", got)
	}
}

func TestKindsLeftOutNeverNotify(t *testing.T) {
	n, s, _ := newNotifier(t, t.TempDir(), Policy{Kinds: []string{Due}})
	n.Notify(ctx, noon, Budget, "", "s", "GITHUB BUDGET")
	n.Notify(ctx, noon, Due, "timer#1", "s", "TIMER")
	if len(s.sent) != 1 || s.sent[0].Body != "TIMER" || s.sent[0].Urgency != "normal" {
		t.Fatalf("only due is configured: %+v", s.sent)
	}
}

func TestQuietHoursHoldAllButCritical(t *testing.T) {
	q, err := ParseQuietHours("11:00-13:00")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	p := Policy{Quiet: q, Urgency: map[string]string{StaleLease: "critical"}}
	a, sa, _ := newNotifier(t, dir, p)
	b, sb, _ := newNotifier(t, dir, p)
	a.Notify(ctx, noon, Due, "timer#1", "beekeeper: timer #1 due: check", "check")
	a.Notify(ctx, noon, Due, "note#2", "beekeeper: note #2 due", "decide")
	a.Notify(ctx, noon, StaleLease, "glean@x", "beekeeper: stale lease glean", "held")
	if len(sa.sent) != 1 || sa.sent[0].Summary != "beekeeper: stale lease glean" {
		t.Fatalf("in quiet hours only the critical one is sent: %+v", sa.sent)
	}
	a.Flush(ctx, noon.Add(30*time.Minute))
	if len(sa.sent) != 1 {
		t.Fatal("quiet hours still cover 12:30, yet the held ones were sent")
	}
	b.Flush(ctx, noon.Add(61*time.Minute))
	a.Flush(ctx, noon.Add(62*time.Minute))
	if len(sb.sent) != 1 || len(sa.sent) != 1 {
		t.Fatalf("the held ones are one notification from one watch: a %d, b %d", len(sa.sent), len(sb.sent))
	}
	m := sb.sent[0]
	if m.Summary != "beekeeper: 2 held during quiet hours" || !strings.Contains(m.Body, "timer #1 due: check") || !strings.Contains(m.Body, "note #2 due") {
		t.Fatalf("held summary: %+v", m)
	}
}

func TestNoServiceIsSaidOnce(t *testing.T) {
	n, s, said := newNotifier(t, t.TempDir(), Policy{})
	s.err = errors.New("dial unix /nonexistent: connect: no such file or directory")
	n.Notify(ctx, noon, Due, "timer#1", "s", "one")
	n.Notify(ctx, noon, Due, "timer#2", "s", "two")
	if len(*said) != 1 || !strings.HasPrefix((*said)[0], "NOTIFY unavailable") {
		t.Fatalf("said %q, want one NOTIFY unavailable line", *said)
	}
	s.err = nil
	n.Notify(ctx, noon, Due, "timer#3", "s", "three")
	if len(*said) != 2 || len(s.sent) != 1 {
		t.Fatalf("back: said %q, sent %d", *said, len(s.sent))
	}
}

func TestQuietHours(t *testing.T) {
	at := func(h, m int) time.Time { return time.Date(2026, 9, 25, h, m, 0, 0, time.Local) }
	q, err := ParseQuietHours("22:00-07:00")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		t    time.Time
		want bool
	}{{at(21, 59), false}, {at(22, 0), true}, {at(3, 0), true}, {at(6, 59), true}, {at(7, 0), false}, {at(12, 0), false}} {
		if got := q.Covers(c.t); got != c.want {
			t.Errorf("22:00-07:00 covers %s: %v, want %v", c.t.Format("15:04"), got, c.want)
		}
	}
	var none *QuietHours
	if none.Covers(at(3, 0)) {
		t.Error("no quiet hours cover nothing")
	}
	for _, bad := range []string{"22:00", "22-07", "25:00-07:00", "07:00-07:00", "ab:cd-07:00"} {
		if _, err := ParseQuietHours(bad); err == nil {
			t.Errorf("%q parsed", bad)
		}
	}
}
