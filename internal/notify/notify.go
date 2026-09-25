// Package notify sends the watch's events that need a person to the desktop
// notification service, once per event however many watches share the
// state directory: each event is claimed in notify.json under notify.lock,
// and only the watch whose claim is first sends it. A lasting condition (the
// machine near its OOM line, the GitHub budget under the floor) is one
// notification per Repeat; quiet hours hold every kind but a critical one
// until they end, then send what they held as one notification.
package notify

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
)

// The kinds of events that notify.
const (
	// Due is a note or a timer falling due.
	Due = "due"
	// OOMLine is the machine near its OOM line: low RAM, swap near the
	// systemd-oomd trigger, memory pressure, the desktop scope near its cap.
	OOMLine = "oom-line"
	// OOMKill is a kernel OOM kill outside a build slot, or a systemd-oomd kill.
	OOMKill = "oom-kill"
	// Budget is the GitHub budget under the floor.
	Budget = "budget"
	// StaleLease is a lease whose holder's session is gone.
	StaleLease = "stale-lease"
	// NoSupervisor is a supervisor whose CLI stayed gone past the restart
	// grace with no successor: claims wait for one.
	NoSupervisor = "no-supervisor"
)

// Kinds are every kind, the default of notify.kinds.
var Kinds = []string{Due, OOMLine, OOMKill, Budget, StaleLease, NoSupervisor}

// lasting are the kinds that are a condition, not an event with an identity:
// one notification per Repeat.
var lasting = map[string]bool{OOMLine: true, Budget: true}

// repeating are the kinds that notify again after Repeat while they last:
// the lasting ones, and a supervisor gone, per supervisor term.
var repeating = map[string]bool{OOMLine: true, Budget: true, NoSupervisor: true}

// The urgency levels of the Desktop Notifications Specification.
const (
	Low      = "low"
	Normal   = "normal"
	Critical = "critical"
)

// Urgencies are the urgency levels, lowest first.
var Urgencies = []string{Low, Normal, Critical}

// DefaultUrgency is the urgency of a kind notify.urgency does not set:
// critical for the two that end sessions and a machine without a supervisor,
// else normal.
func DefaultUrgency(kind string) string {
	if kind == OOMLine || kind == OOMKill || kind == NoSupervisor {
		return Critical
	}
	return Normal
}

// Policy is what notifies, when and how urgently.
type Policy struct {
	Kinds   []string
	Quiet   *QuietHours
	Urgency map[string]string
	// Repeat is how often a lasting condition notifies again.
	Repeat time.Duration
}

func (p Policy) urgency(kind string) string {
	if u := p.Urgency[kind]; u != "" {
		return u
	}
	return DefaultUrgency(kind)
}

// QuietHours is a daily span of local time, "22:00-07:00"; it may wrap
// around midnight.
type QuietHours struct{ from, to int }

// ParseQuietHours reads "HH:MM-HH:MM"; empty is none.
func ParseQuietHours(s string) (*QuietHours, error) {
	if s == "" {
		return nil, nil
	}
	a, b, ok := strings.Cut(s, "-")
	if !ok {
		return nil, fmt.Errorf("quiet hours %q: want HH:MM-HH:MM", s)
	}
	from, err := minutes(a)
	if err != nil {
		return nil, fmt.Errorf("quiet hours %q: %w", s, err)
	}
	to, err := minutes(b)
	if err != nil {
		return nil, fmt.Errorf("quiet hours %q: %w", s, err)
	}
	if from == to {
		return nil, fmt.Errorf("quiet hours %q: an empty span", s)
	}
	return &QuietHours{from, to}, nil
}

func minutes(s string) (int, error) {
	h, m, ok := strings.Cut(strings.TrimSpace(s), ":")
	hh, err1 := strconv.Atoi(h)
	mm, err2 := strconv.Atoi(m)
	if !ok || err1 != nil || err2 != nil || hh < 0 || hh > 23 || mm < 0 || mm > 59 {
		return 0, fmt.Errorf("%q is no HH:MM", s)
	}
	return hh*60 + mm, nil
}

// Covers reports whether t falls in the quiet hours.
func (q *QuietHours) Covers(t time.Time) bool {
	if q == nil {
		return false
	}
	m := t.Hour()*60 + t.Minute()
	if q.from < q.to {
		return m >= q.from && m < q.to
	}
	return m >= q.from || m < q.to
}

// Notifier sends one watch's events.
type Notifier struct {
	policy Policy
	ledger *Ledger
	sender Sender
	// say prints one watch line: the service gone, and back.
	say  func(string)
	down bool
}

// New returns a notifier that claims events in dir and sends them with s.
func New(p Policy, dir string, s Sender, say func(string)) *Notifier {
	return &Notifier{policy: p, ledger: NewLedger(dir), sender: s, say: say}
}

// Claim records the events of kind identified by keys and returns the keys
// no watch had claimed before, or claimed longer than Repeat ago for a
// repeating kind; a lasting kind is claimed as the kind itself and takes no
// keys. A kind notify.kinds leaves out claims
// nothing.
func (n *Notifier) Claim(ctx context.Context, now time.Time, kind string, keys ...string) []string {
	if !slices.Contains(n.policy.Kinds, kind) {
		return nil
	}
	if lasting[kind] {
		keys = []string{kind}
	}
	var fresh []string
	err := n.ledger.update(ctx, func(l *ledger) {
		for _, k := range keys {
			id := kind + ":" + k
			if lasting[kind] {
				id = kind
			}
			if s, ok := l.Sent[id]; ok && (!repeating[kind] || now.Sub(s) < n.policy.Repeat) {
				continue
			}
			l.Sent[id] = now.UTC()
			fresh = append(fresh, k)
		}
	})
	if err != nil {
		n.say(fmt.Sprintf("NOTIFY cannot claim %s: %v", kind, err))
		return nil
	}
	return fresh
}

// Notify claims one event and sends it if the claim is this watch's.
func (n *Notifier) Notify(ctx context.Context, now time.Time, kind, key, summary, body string) {
	if len(n.Claim(ctx, now, kind, key)) > 0 {
		n.Send(ctx, now, kind, summary, body)
	}
}

// Send delivers a claimed event, or holds it in quiet hours unless its
// urgency is critical.
func (n *Notifier) Send(ctx context.Context, now time.Time, kind, summary, body string) {
	m := Message{Summary: summary, Body: body, Urgency: n.policy.urgency(kind)}
	if m.Urgency != Critical && n.policy.Quiet.Covers(now) {
		err := n.ledger.update(ctx, func(l *ledger) {
			l.Held = append(l.Held, Delivery{At: now.UTC(), Kind: kind, Summary: summary, Body: body, Urgency: m.Urgency})
			l.log(Delivery{At: now.UTC(), Kind: kind, Summary: summary, Urgency: m.Urgency, Held: true})
		})
		if err != nil {
			n.say(fmt.Sprintf("NOTIFY cannot hold %s: %v", kind, err))
		}
		return
	}
	n.deliver(ctx, now, kind, m)
}

// Flush sends what the quiet hours held, as one notification, once they
// have ended; the watch that takes the held events first sends them.
func (n *Notifier) Flush(ctx context.Context, now time.Time) {
	if n.policy.Quiet.Covers(now) {
		return
	}
	var held []Delivery
	if err := n.ledger.update(ctx, func(l *ledger) { held, l.Held = l.Held, nil }); err != nil || len(held) == 0 {
		return
	}
	m := Message{Summary: fmt.Sprintf("beekeeper: %d held during quiet hours", len(held)), Urgency: Normal}
	var lines []string
	for i, h := range held {
		if i == maxHeldLines {
			lines = append(lines, fmt.Sprintf("… and %d more: beekeeper log", len(held)-i))
			break
		}
		lines = append(lines, h.At.Local().Format("15:04")+" "+strings.TrimPrefix(h.Summary, "beekeeper: "))
	}
	m.Body = strings.Join(lines, "\n")
	n.deliver(ctx, now, "held", m)
}

const maxHeldLines = 8

// deliver sends m and logs its id, or says once that the service does not
// answer; the watch's lines print either way.
func (n *Notifier) deliver(ctx context.Context, now time.Time, kind string, m Message) {
	sendCtx, cancel := context.WithTimeout(ctx, sendTimeout)
	id, err := n.sender.Send(sendCtx, m)
	cancel()
	d := Delivery{At: now.UTC(), Kind: kind, Summary: m.Summary, Urgency: m.Urgency, ID: id}
	switch {
	case err != nil && !n.down:
		n.down = true
		n.say(fmt.Sprintf("NOTIFY unavailable, printing only (said once until it answers again): %v", err))
	case err == nil && n.down:
		n.down = false
		n.say("NOTIFY the notification service answers again")
	}
	if err != nil {
		d.Error = err.Error()
	}
	_ = n.ledger.update(ctx, func(l *ledger) { l.log(d) })
}

// sendTimeout bounds one notification: a wedged bus must not stall a poll.
const sendTimeout = 5 * time.Second
