// Package mailbox is the central instance's message queue: a mailbox per
// person for the local agents that person federates, in the beekeeper
// database of the platform's Postgres.
//
// Delivery is at least once and ordered per sender and receiver: a message
// stays until its receiver acks it or its deadline passes. A mailbox holds
// at most Cap unacked messages, and a sender's message id is accepted once
// within DedupeWindow. Every change of a mailbox is announced on the
// Postgres channel Channel, with the mailbox as its payload, so every
// instance learns of it.
package mailbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	// Cap is how many unacked messages a mailbox holds; the next is refused.
	Cap = 50
	// TTL is a message's life without a deadline of its own.
	TTL = 24 * time.Hour
	// DedupeWindow is how long a sender's message id is remembered.
	DedupeWindow = 24 * time.Hour
	// Channel is the Postgres channel a mailbox's changes are announced on.
	Channel = "beekeeper_mailbox"
)

// The kinds of message: a sender's, and the notice to a sender that its
// message expired unacked.
const (
	KindMessage = "message"
	KindExpired = "expired"
)

// ErrFull is the refusal of a message to a mailbox that holds Cap.
var ErrFull = errors.New("the mailbox is full")

// Message is a message to send.
type Message struct {
	// Mailbox is the receiving person's email.
	Mailbox string
	// Sender is the sending person's email, SenderTeam the person's team.
	Sender     string
	SenderTeam string
	// ID is the sender's message id (the A2A messageId), unique per sender.
	ID string
	// Envelope is the message as the receiver gets it.
	Envelope json.RawMessage
	// Deadline is when it expires unacked; zero: TTL after it is sent.
	Deadline time.Time
}

// Delivery is a message in a mailbox.
type Delivery struct {
	// Seq is the delivery's number, the order of the mailbox: what an ack
	// names.
	Seq       int64           `json:"id"`
	Kind      string          `json:"kind"`
	Sender    string          `json:"sender"`
	MessageID string          `json:"messageId"`
	Envelope  json.RawMessage `json:"envelope"`
	Sent      time.Time       `json:"sent"`
	Deadline  time.Time       `json:"deadline"`
}

// Sent is the outcome of a send: the delivery's number, and whether the
// message id was seen before (nothing was queued again).
type Sent struct {
	Seq       int64 `json:"id"`
	Duplicate bool  `json:"duplicate"`
}

// Store is the mailboxes in Postgres.
type Store struct {
	pool *pgxpool.Pool
}

// Open connects to the database url names and migrates its schema.
func Open(ctx context.Context, url string) (*Store, error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("the mailbox database: %w", err)
	}
	s := &Store{pool: pool}
	if err := s.migrate(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return s, nil
}

// Close closes the connections.
func (s *Store) Close() { s.pool.Close() }

// migrations are the schema's versions, applied in order, each once.
var migrations = []string{
	`CREATE TABLE messages (
		seq        bigserial PRIMARY KEY,
		mailbox    text NOT NULL,
		sender     text NOT NULL,
		sender_team text NOT NULL,
		kind       text NOT NULL,
		message_id text NOT NULL,
		envelope   jsonb NOT NULL,
		sent       timestamptz NOT NULL,
		deadline   timestamptz NOT NULL,
		acked      timestamptz,
		UNIQUE (sender, message_id)
	);
	CREATE INDEX messages_pending ON messages (mailbox, seq) WHERE acked IS NULL;
	CREATE TABLE dedupe (
		sender     text NOT NULL,
		message_id text NOT NULL,
		seq        bigint NOT NULL,
		seen       timestamptz NOT NULL,
		PRIMARY KEY (sender, message_id)
	);`,
}

// migrate applies the migrations the database has not seen, under a lock
// that serialises the instances starting at once.
func (s *Store) migrate(ctx context.Context) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('beekeeper_schema'))`); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_version (version int NOT NULL)`); err != nil {
			return err
		}
		var v int
		if err := tx.QueryRow(ctx, `SELECT coalesce(max(version), 0) FROM schema_version`).Scan(&v); err != nil {
			return err
		}
		if v > len(migrations) {
			return fmt.Errorf("the mailbox schema is at version %d, newer than this beekeeper's %d", v, len(migrations))
		}
		for i := v; i < len(migrations); i++ {
			if _, err := tx.Exec(ctx, migrations[i]); err != nil {
				return fmt.Errorf("migrating the mailbox schema to version %d: %w", i+1, err)
			}
		}
		if v < len(migrations) {
			if _, err := tx.Exec(ctx, `DELETE FROM schema_version`); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `INSERT INTO schema_version VALUES ($1)`, len(migrations)); err != nil {
				return err
			}
		}
		return nil
	})
}

// Key is a person's mailbox key: the email, lower-cased.
func Key(person string) string { return strings.ToLower(strings.TrimSpace(person)) }

// Send queues m. A message id the sender used within DedupeWindow is not
// queued again: the first delivery's number comes back, marked a duplicate.
// A mailbox that holds Cap refuses with ErrFull.
func (s *Store) Send(ctx context.Context, m Message, now time.Time) (Sent, error) {
	m.Mailbox, m.Sender = Key(m.Mailbox), Key(m.Sender)
	if m.Mailbox == "" || m.Sender == "" || m.ID == "" {
		return Sent{}, errors.New("a message needs a mailbox, a sender and an id")
	}
	if m.Deadline.IsZero() {
		m.Deadline = now.Add(TTL)
	}
	if !m.Deadline.After(now) {
		return Sent{}, fmt.Errorf("the deadline %s has passed", m.Deadline.UTC().Format(time.RFC3339))
	}
	var out Sent
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		// One sender's sends and one mailbox's cap are judged one at a time.
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('beekeeper_mailbox:' || $1))`, m.Mailbox); err != nil {
			return err
		}
		err := tx.QueryRow(ctx, `SELECT seq FROM dedupe WHERE sender = $1 AND message_id = $2 AND seen > $3`,
			m.Sender, m.ID, now.Add(-DedupeWindow)).Scan(&out.Seq)
		if err == nil {
			out.Duplicate = true
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM messages WHERE mailbox = $1 AND kind = $2 AND acked IS NULL AND deadline > $3`,
			m.Mailbox, KindMessage, now).Scan(&n); err != nil {
			return err
		}
		if n >= Cap {
			return fmt.Errorf("%w: %s holds %d unacked messages", ErrFull, m.Mailbox, n)
		}
		// An id seen outside the window is a new message: its old row goes.
		if _, err := tx.Exec(ctx, `DELETE FROM messages WHERE sender = $1 AND message_id = $2`, m.Sender, m.ID); err != nil {
			return err
		}
		out.Seq, err = insert(ctx, tx, m, KindMessage, now)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO dedupe (sender, message_id, seq, seen) VALUES ($1, $2, $3, $4)
			ON CONFLICT (sender, message_id) DO UPDATE SET seq = EXCLUDED.seq, seen = EXCLUDED.seen`, m.Sender, m.ID, out.Seq, now)
		return err
	})
	return out, err
}

// insert adds a message of kind to its mailbox and announces the change at
// the commit.
func insert(ctx context.Context, tx pgx.Tx, m Message, kind string, now time.Time) (int64, error) {
	var seq int64
	if err := tx.QueryRow(ctx, `INSERT INTO messages (mailbox, sender, sender_team, kind, message_id, envelope, sent, deadline)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8) RETURNING seq`,
		m.Mailbox, m.Sender, m.SenderTeam, kind, m.ID, []byte(m.Envelope), now, m.Deadline).Scan(&seq); err != nil {
		return 0, err
	}
	_, err := tx.Exec(ctx, `SELECT pg_notify($1, $2)`, Channel, m.Mailbox)
	return seq, err
}

// Receive returns the oldest unacked, unexpired deliveries of mailbox, at
// most limit, in order.
func (s *Store) Receive(ctx context.Context, mailbox string, limit int, now time.Time) ([]Delivery, error) {
	if limit <= 0 || limit > Cap {
		limit = Cap
	}
	rows, err := s.pool.Query(ctx, `SELECT seq, kind, sender, message_id, envelope, sent, deadline FROM messages
		WHERE mailbox = $1 AND acked IS NULL AND deadline > $2 ORDER BY seq LIMIT $3`, Key(mailbox), now, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Delivery, error) {
		var d Delivery
		var env []byte
		err := r.Scan(&d.Seq, &d.Kind, &d.Sender, &d.MessageID, &env, &d.Sent, &d.Deadline)
		d.Envelope, d.Sent, d.Deadline = env, d.Sent.UTC(), d.Deadline.UTC()
		return d, err
	})
}

// Pending is how many unacked, unexpired deliveries mailbox holds.
func (s *Store) Pending(ctx context.Context, mailbox string, now time.Time) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `SELECT count(*) FROM messages WHERE mailbox = $1 AND acked IS NULL AND deadline > $2`, Key(mailbox), now).Scan(&n)
	return n, err
}

// Ack marks mailbox's deliveries seqs received and returns how many it
// acked; a number not pending in mailbox is none of its.
func (s *Store) Ack(ctx context.Context, mailbox string, seqs []int64, now time.Time) (int, error) {
	var n int64
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE messages SET acked = $1 WHERE mailbox = $2 AND seq = ANY($3) AND acked IS NULL`, now, Key(mailbox), seqs)
		if err != nil {
			return err
		}
		if n = tag.RowsAffected(); n > 0 {
			_, err = tx.Exec(ctx, `SELECT pg_notify($1, $2)`, Channel, Key(mailbox))
		}
		return err
	})
	return int(n), err
}

// Expired is a sender's message that expired unacked.
type Expired struct {
	Seq        int64     `json:"id"`
	Mailbox    string    `json:"mailbox"`
	Sender     string    `json:"sender"`
	SenderTeam string    `json:"senderTeam"`
	MessageID  string    `json:"messageId"`
	Deadline   time.Time `json:"deadline"`
}

// Expire takes the messages past their deadline out of their mailboxes,
// puts a notice of each sender's into the sender's mailbox (kind expired,
// outside the cap), forgets the acked messages and the message ids older
// than DedupeWindow, and returns the senders' expired messages.
func (s *Store) Expire(ctx context.Context, now time.Time) ([]Expired, error) {
	var out []Expired
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `DELETE FROM messages WHERE acked IS NULL AND deadline <= $1
			RETURNING seq, kind, mailbox, sender, sender_team, message_id, deadline`, now)
		if err != nil {
			return err
		}
		var kinds []string
		out, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (Expired, error) {
			var e Expired
			var kind string
			err := r.Scan(&e.Seq, &kind, &e.Mailbox, &e.Sender, &e.SenderTeam, &e.MessageID, &e.Deadline)
			kinds = append(kinds, kind)
			e.Deadline = e.Deadline.UTC()
			return e, err
		})
		if err != nil {
			return err
		}
		changed := map[string]bool{}
		senders := out[:0]
		for i, e := range out {
			changed[e.Mailbox] = true
			if kinds[i] != KindMessage {
				continue
			}
			senders = append(senders, e)
			env, err := json.Marshal(map[string]any{"kind": KindExpired, "messageId": e.MessageID, "to": e.Mailbox, "deadline": e.Deadline})
			if err != nil {
				return err
			}
			notice := Message{Mailbox: e.Sender, Sender: "beekeeper", ID: fmt.Sprintf("expired-%d", e.Seq), Envelope: env, Deadline: now.Add(TTL)}
			if _, err := insert(ctx, tx, notice, KindExpired, now); err != nil {
				return err
			}
		}
		out = senders
		for mb := range changed {
			if _, err := tx.Exec(ctx, `SELECT pg_notify($1, $2)`, Channel, mb); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, `DELETE FROM messages WHERE acked IS NOT NULL`); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `DELETE FROM dedupe WHERE seen <= $1`, now.Add(-DedupeWindow))
		return err
	})
	return out, err
}

// Listen calls changed with the mailbox of every change announced on
// Channel until ctx ends, reconnecting after a lost connection. ready, if
// set, is called each time the listener is in place.
func (s *Store) Listen(ctx context.Context, changed func(mailbox string), ready func(), lost func(error)) {
	for ctx.Err() == nil {
		err := s.listen(ctx, changed, ready)
		if ctx.Err() != nil {
			return
		}
		if lost != nil {
			lost(err)
		}
		select {
		case <-ctx.Done():
		case <-time.After(time.Second):
		}
	}
}

func (s *Store) listen(ctx context.Context, changed func(string), ready func()) error {
	// A connection of its own: a pooled one would keep listening after.
	conn, err := pgx.ConnectConfig(ctx, s.pool.Config().ConnConfig.Copy())
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close(context.WithoutCancel(ctx)) }()
	if _, err := conn.Exec(ctx, "LISTEN "+Channel); err != nil {
		return err
	}
	if ready != nil {
		ready()
	}
	for {
		n, err := conn.WaitForNotification(ctx)
		if err != nil {
			return err
		}
		changed(n.Payload)
	}
}
