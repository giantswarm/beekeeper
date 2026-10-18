package board

import (
	"context"
	"fmt"
	"strings"

	"github.com/giantswarm/beekeeper/internal/github"
)

// HeldStatus is the Status a hold on a person sets (see Resolve): the item
// waits on the person's answer to a decision.
const HeldStatus = "Blocked / Waiting"

// Hold sets the Status of the board item of the issue ref to HeldStatus and
// returns the move, Moved.From the Status it had. An issue without an item
// on the board, or whose item has no Status to return to, is refused before
// anything is written: a hold never adds an item to the board.
func (c *Client) Hold(ctx context.Context, ref string) (*Moved, error) {
	return c.setHeld(ctx, ref, func(m *meta, mv *Moved) (string, error) {
		to, err := Resolve(StatusField, m.Fields[StatusField].names(), HeldStatus)
		if err != nil {
			return "", &Refusal{err.Error()}
		}
		if mv.From == "" {
			return "", &Refusal{fmt.Sprintf("%s has no Status on the board %s/%d to return to once the hold is released", mv.Ref, c.Board.Owner, c.Board.Project)}
		}
		return to, nil
	})
}

// Release returns the board item of the issue ref from the Status held to
// back, the Status it had before the hold. An item whose Status is no
// longer held was moved since the hold was set: it is left as it is,
// Moved.To its Status and Moved.Kept true.
func (c *Client) Release(ctx context.Context, ref, held, back string) (*Moved, error) {
	var kept bool
	mv, err := c.setHeld(ctx, ref, func(m *meta, mv *Moved) (string, error) {
		if !strings.EqualFold(mv.From, held) {
			kept = true
			return mv.From, nil
		}
		to, err := Resolve(StatusField, m.Fields[StatusField].names(), back)
		if err != nil {
			return "", &Refusal{err.Error()}
		}
		return to, nil
	})
	if mv != nil {
		mv.Kept = kept
	}
	return mv, err
}

// setHeld sets the Status of the board item of the issue ref to what to
// answers, given the move with its current Status: no Team, no epic gate,
// nothing added to the board. An issue without an item on the board is
// refused.
func (c *Client) setHeld(ctx context.Context, ref string, to func(*meta, *Moved) (string, error)) (*Moved, error) {
	owner, repo, n, err := Ref(ref)
	if err != nil {
		return nil, err
	}
	ctx = github.ActingOn(ctx, owner+"/"+repo)
	m, err := c.meta(ctx)
	if err != nil {
		return nil, err
	}
	mv := &Moved{Ref: fmt.Sprintf("%s/%s#%d", owner, repo, n)}
	_, item, _, err := c.lookup(ctx, m, mv.Ref, owner, repo, n)
	if err != nil {
		return nil, err
	}
	if item == nil {
		return nil, &Refusal{fmt.Sprintf("%s is not on the board %s/%d: a hold sets a board item's Status", mv.Ref, c.Board.Owner, c.Board.Project)}
	}
	if item.Status != nil {
		mv.From = item.Status.Name
	}
	if mv.To, err = to(m, mv); err != nil {
		return nil, err
	}
	if mv.From == mv.To {
		return mv, nil
	}
	sf := m.Fields[StatusField]
	if err := c.set(ctx, m.ID, item.ID, sf.ID, sf.option(mv.To)); err != nil {
		return nil, err
	}
	return mv, nil
}
