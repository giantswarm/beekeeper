package cmd

import (
	"cmp"
	"context"
	"strings"

	"github.com/giantswarm/beekeeper/internal/execution"
	"github.com/giantswarm/beekeeper/internal/state"
)

// watchTellDefaulted batches a pass by its current recipients. Each note still
// follows the usual mailbox, role fallback and delivery-status handling.
func watchTellDefaulted(w *Watcher, ctx context.Context, notes []state.Note) {
	st, err := watchApp(w.Host).store.Read()
	if err != nil {
		w.Fail("state-read", "cannot read the state: %v", err)
		return
	}
	// Normalize roster names and session IDs to the same recipient.
	recipient := func(q string) string {
		if ag, _, err := watchApp(w.Host).wakeParty(st, q); err == nil {
			return cmp.Or(wakeID(ag.Party), ag.Name)
		}
		return q
	}
	lines := make(map[string][]string)
	for _, n := range notes {
		p := n.By
		if rl, ok := filedRole(st, p); ok {
			if h := rl.Get(st).Holder; h != nil {
				p = h.Party
			} else {
				continue
			}
		}
		q := recipient(cmp.Or(wakeID(p), p.Name))
		lines[q] = append(lines[q], defaultedLine(&n))
	}
	results := make(map[string]error)
	wake := func(a *app, ctx context.Context, by state.Party, q, line string) error {
		q = recipient(q)
		if err, done := results[q]; done {
			return err
		}
		if batch := lines[q]; len(batch) > 0 {
			line = strings.Join(batch, "\n")
		}
		quiet := *a
		quiet.executionRequestID = "notes/defaulted/" + execution.Digest([]byte(q+"\n"+line))
		err := quiet.quietWake(ctx, by, q, line)
		results[q] = err
		return err
	}
	for _, n := range notes {
		watchApp(w.Host).tellFilerWithWake(ctx, WatchParty, &n, defaultedLine(&n), wake)
	}
}
