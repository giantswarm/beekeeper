package cmd

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync/atomic"
	"time"

	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/mailbox"
	"github.com/giantswarm/beekeeper/internal/state"
)

// mailboxParty is who the mailbox watch's events and deliveries are by.
var mailboxParty = state.Party{Name: "beekeeper mailbox"}

// mailboxKey is the watch's condition of a mailbox subscription that does
// not stand.
const mailboxKey = "mailbox"

// mailboxRetry is how long the mailbox watch waits before it subscribes
// again after the subscription ended.
const mailboxRetry = time.Minute

// watchMailbox delivers the person's mailbox to the machine's agents while
// ctx lasts: it subscribes to beekeeper://mailbox/<person> through muster
// and drains the mailbox once the subscription stands and on each update.
// A subscription that ends is one MAILBOX UNSUBSCRIBED line and is opened
// again after mailboxRetry; meanwhile, and while a message waits for an
// agent that did not take it, the mailbox is drained every watch interval.
func (w *watcher) watchMailbox(ctx context.Context) {
	person := mailbox.Key(w.cfg.Identity.Person)
	if person == "" {
		return
	}
	uri := resourceMailbox + person
	kick := make(chan struct{}, 1)
	signal := func() {
		select {
		case kick <- struct{}{}:
		default:
		}
	}
	var down, waiting atomic.Bool
	down.Store(true)
	done := make(chan struct{})
	go func() {
		defer close(done)
		tick := time.NewTicker(w.cfg.Watch.Interval.Duration)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-kick:
			case <-tick.C:
				if !down.Load() && !waiting.Load() {
					continue
				}
			}
			waiting.Store(w.drainMailbox(ctx))
		}
	}()
	defer func() { <-done }()
	for ctx.Err() == nil {
		err := w.app.hub().Listen(ctx, []string{uri}, func() {
			down.Store(false)
			w.clear(mailboxKey)
			signal()
		}, func(u string) {
			if u == uri {
				signal()
			}
		})
		if ctx.Err() != nil {
			return
		}
		down.Store(true)
		w.emit(mailboxKey, "MAILBOX UNSUBSCRIBED %s: %s; drained every %s until it stands again", uri, unreadableReason(err), w.cfg.Watch.Interval.Duration)
		t := time.NewTimer(mailboxRetry)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
	}
}

// mailEnvelope is what the mailbox watch reads of a delivery's envelope: a
// message's address, sender and A2A Message, or an expiry notice's.
type mailEnvelope struct {
	To   string `json:"to"`
	From struct {
		Person string `json:"person"`
		Agent  string `json:"agent"`
		Host   string `json:"host"`
		Source string `json:"source"`
	} `json:"from"`
	Message   a2aMessage `json:"message"`
	MessageID string     `json:"messageId"`
	Deadline  time.Time  `json:"deadline"`
}

// drainMailbox delivers the person's waiting messages addressed to the
// machine's agents, oldest first, and acks each delivered one: a message
// arrives in its agent's running session at its next tool round, else as
// a wake's turn. A message to an agent that does not take it stays, and so
// do the later ones to that agent, in their order; one to another machine's
// agent is left to that machine. An expiry notice is said and acked. It
// reports whether a message waits for one of the machine's agents.
func (w *watcher) drainMailbox(ctx context.Context) bool {
	host := w.cfg.Identity.Host
	me := state.Party{Name: w.cfg.Identity.Person, Host: host}
	var got struct {
		Messages []mailbox.Delivery `json:"messages"`
	}
	if _, err := w.callCentralRaw(ctx, me, "receive_messages", map[string]any{}, &got); err != nil {
		w.check(mailboxKey+" receive", true, "MAILBOX UNREADABLE %s: %s", w.cfg.Central.Context, unreadableReason(err))
		return true
	}
	w.clear(mailboxKey + " receive")
	quiet := *w.app
	quiet.out = io.Discard
	deliver := w.deliver
	if deliver == nil {
		deliver = func(ctx context.Context, q, msg string) error { return quiet.wakeAgent(ctx, mailboxParty, q, msg, "") }
	}
	var acks []int64
	held := map[string]bool{}
	for _, d := range got.Messages {
		var env mailEnvelope
		_ = json.Unmarshal(d.Envelope, &env)
		if d.Kind == mailbox.KindExpired {
			w.emitNow(mailboxKey, "MAILBOX: the message %s to %s expired unacked at %s", env.MessageID, env.To, env.Deadline.Local().Format(time.DateTime))
			acks = append(acks, d.Seq)
			continue
		}
		at, name, ok := strings.Cut(strings.TrimPrefix(env.To, localPrefix), "/")
		if !ok || !strings.EqualFold(at, host) || held[name] {
			continue
		}
		key := mailboxKey + " " + name
		if err := deliver(ctx, name, mailBody(d, env)); err != nil {
			held[name] = true
			w.check(key, true, "MAILBOX %s waits for %s, delivered once it takes it: %s", d.MessageID, name, unreadableReason(err))
			continue
		}
		w.clear(key)
		acks = append(acks, d.Seq)
		_ = w.store.Log(event(mailboxParty, "mailbox.delivered", "%s: %s from %s%s", name, d.MessageID, d.Sender, via(d.Envelope)))
		w.emitNow(mailboxKey, "MAILBOX to %s: %s from %s%s: %s", name, d.MessageID, d.Sender, via(d.Envelope), firstLine(env.Message.text()))
	}
	if len(acks) > 0 {
		// A watch that stops meanwhile still acks what it delivered.
		if _, err := w.callCentralRaw(context.WithoutCancel(ctx), me, "ack_messages", map[string]any{"ids": acks}, nil); err != nil {
			w.emitNow(mailboxKey, "MAILBOX: %d delivered not acked, delivered again: %s", len(acks), unreadableReason(err))
		}
	}
	return len(held) > 0
}

// mailBody is the turn a delivered message is: its text stamped with its
// person and where they wrote it, and how to answer there. A message one
// of the person's agents sent names that agent: it is not the person's own
// writing, whatever source it claims.
func mailBody(d mailbox.Delivery, env mailEnvelope) string {
	person := cmp.Or(env.From.Person, d.Sender)
	text := env.Message.text()
	if env.From.Agent != "" && !strings.EqualFold(env.From.Agent, person) {
		return fmt.Sprintf("Message from %s's agent %s on %s (%s): %s", person, env.From.Agent, env.From.Host, d.MessageID, text)
	}
	body := fmt.Sprintf("Message from %s%s (%s): %s", person, via(d.Envelope), d.MessageID, text)
	if env.From.Source == viaSlack {
		body += "\n\nThey wrote it in your Slack thread with them: answer there with `beekeeper converse \"<text>\"`."
	}
	return body
}

// converse writes text to the person in Slack as the calling agent, in its
// conversation thread (the central instance's converse).
func (a *app) converse(text string) error {
	if !a.cfg.Central.Enabled() {
		return refused("no central instance is configured (central.context): there is no Slack to converse in")
	}
	me, err := a.caller()
	if err != nil {
		return err
	}
	out, err := a.callCentral(context.Background(), me, "converse", map[string]any{paramText: text}, nil)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(a.out, out)
	return err
}

func (a *app) converseCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "converse <text>",
		Short: "Write to your person in Slack, in your conversation thread with them",
		Long: `Write text (Slack markdown) to the person as the calling agent through
the central instance's converse: the first message opens a direct message
from the agent, later ones go into its thread. The person's replies there
arrive in their mailbox; the standby watch delivers each to the agent it
is addressed to, in its running session at its next tool round, stamped
with the person and "via slack".`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error { return a.converse(strings.Join(args, " ")) },
	}
}
