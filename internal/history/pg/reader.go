package historypg

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/giantswarm/beekeeper/internal/history"
)

// Events returns the events matching f, oldest first (by time, then by id).
func (w *Writer) Events(ctx context.Context, f history.EventFilter) ([]history.Event, error) {
	var where []string
	var args []any
	add := func(col string, v any) {
		args = append(args, v)
		where = append(where, fmt.Sprintf("%s = $%d", col, len(args)))
	}
	if f.Item != "" {
		add("item", f.Item)
	}
	if f.Change != "" {
		add("change", f.Change)
	}
	if f.Type != "" {
		add("type", f.Type)
	}
	if f.Source != "" {
		add("source", f.Source)
	}
	if f.AfterID > 0 {
		args = append(args, f.AfterID)
		where = append(where, fmt.Sprintf("id > $%d", len(args)))
	}
	rows, err := w.pool.Query(ctx, `SELECT id, at, team, item, change, type, source, data FROM events`+
		whereClause(where)+` ORDER BY at, id`, args...)
	if err != nil {
		return nil, fmt.Errorf("history: events: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (history.Event, error) {
		var e history.Event
		var data []byte
		if err := r.Scan(&e.ID, &e.At, &e.Team, &e.Item, &e.Change, &e.Type, &e.Source, &data); err != nil {
			return e, err
		}
		e.At = e.At.UTC()
		return e, json.Unmarshal(data, &e.Data)
	})
	if err != nil {
		return nil, fmt.Errorf("history: events: %w", err)
	}
	return out, nil
}

// Decisions returns the decisions matching f, oldest first (by time, then by id).
func (w *Writer) Decisions(ctx context.Context, f history.DecisionFilter) ([]history.Decision, error) {
	var where []string
	var args []any
	if f.Point != "" {
		args = append(args, f.Point)
		where = append(where, fmt.Sprintf("point = $%d", len(args)))
	}
	if f.Subject != "" {
		args = append(args, f.Subject)
		where = append(where, fmt.Sprintf("subject = $%d", len(args)))
	}
	if f.AfterID > 0 {
		args = append(args, f.AfterID)
		where = append(where, fmt.Sprintf("id > $%d", len(args)))
	}
	rows, err := w.pool.Query(ctx, `SELECT id, at, point, subject, other, inputs, inputs_hash, rule, jev, jev_confidence,
		jev_error, chosen, mode, outcome, team FROM decisions`+whereClause(where)+` ORDER BY at, id`, args...)
	if err != nil {
		return nil, fmt.Errorf("history: decisions: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (history.Decision, error) {
		var d history.Decision
		var inputs []byte
		if err := r.Scan(&d.ID, &d.At, &d.Point, &d.Subject, &d.Other, &inputs, &d.InputsHash, &d.Rule, &d.JEV,
			&d.JEVConfidence, &d.JEVError, &d.Chosen, &d.Mode, &d.Outcome, &d.Team); err != nil {
			return d, err
		}
		d.At = d.At.UTC()
		return d, json.Unmarshal(inputs, &d.Inputs)
	})
	if err != nil {
		return nil, fmt.Errorf("history: decisions: %w", err)
	}
	return out, nil
}

// Durations returns the recorded durations of kind and key, oldest first.
func (w *Writer) Durations(ctx context.Context, kind, key string) ([]time.Duration, error) {
	rows, err := w.pool.Query(ctx, `SELECT value FROM durations WHERE kind = $1 AND key = $2 ORDER BY at, id`, kind, key)
	if err != nil {
		return nil, fmt.Errorf("history: durations: %w", err)
	}
	out, err := pgx.CollectRows(rows, pgx.RowTo[time.Duration])
	if err != nil {
		return nil, fmt.Errorf("history: durations: %w", err)
	}
	return out, nil
}

// MaxIDs returns the largest event and decision ids written so far (0 for none).
func (w *Writer) MaxIDs(ctx context.Context) (events, decisions int64, err error) {
	err = w.pool.QueryRow(ctx, `SELECT (SELECT coalesce(max(id), 0) FROM events), (SELECT coalesce(max(id), 0) FROM decisions)`).
		Scan(&events, &decisions)
	if err != nil {
		return 0, 0, fmt.Errorf("history: max ids: %w", err)
	}
	return events, decisions, nil
}

// DecisionKeys returns every recorded decision key.
func (w *Writer) DecisionKeys(ctx context.Context) ([]history.Key, error) {
	rows, err := w.pool.Query(ctx, `SELECT point, subject, inputs_hash FROM decisions`)
	if err != nil {
		return nil, fmt.Errorf("history: decision keys: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (history.Key, error) {
		var k history.Key
		return k, r.Scan(&k.Point, &k.Subject, &k.InputsHash)
	})
	if err != nil {
		return nil, fmt.Errorf("history: decision keys: %w", err)
	}
	return out, nil
}

func whereClause(conds []string) string {
	if len(conds) == 0 {
		return ""
	}
	return " WHERE " + strings.Join(conds, " AND ")
}
