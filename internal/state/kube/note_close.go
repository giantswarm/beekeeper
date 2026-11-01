package kube

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/pkg/apis/beekeeper/v1alpha1"
)

const (
	noteClosed         = "closed"
	noteAnswered       = "answered"
	noteAnsweredEvent  = "answer.recorded"
	noteDoneEvent      = "note.done"
	noteDefaultedEvent = "note.defaulted"
)

// closeNotes refuses a silent close and keeps its reason in the same status
// write as its outcome. Closed resources stay out of the open state document.
func (snap *snapshot) closeNotes(want *snapshot, events []state.Event, nextID func(time.Time) (string, error)) error {
	for k, old := range snap.notes {
		if old.Status.State == noteClosed || old.Status.State == noteAnswered {
			want.notes[k] = old.DeepCopy()
			continue
		}
		n := want.notes[k]
		if n != nil && (n.Status.State != noteDefaulted || old.Status.State == noteDefaulted) {
			continue
		}
		var closing *state.Event
		for i := range events {
			e := &events[i]
			detail := e.Detail
			if e.Verb == noteAnsweredEvent {
				_, detail, _ = strings.Cut(detail, ", note #")
				detail = "#" + detail
			}
			m := noteRef.FindStringSubmatch(detail)
			if len(m) < 2 || m[1] != strconv.Itoa(old.Spec.ID) {
				continue
			}
			switch e.Verb {
			case noteDefaultedEvent, noteDoneEvent, "note.overtaken", noteAnsweredEvent:
				closing = e
			}
		}
		if closing == nil && old.Status.State == noteDefaulted && n == nil {
			// Expiry ends the late-answer window; it does not erase the default.
			n = old.DeepCopy()
			n.Status.State = noteClosed
			if n.Status.Closure == nil {
				id, err := nextID(timeOf(old.Spec.Due))
				if err != nil {
					return err
				}
				n.Status.Closure = &v1alpha1.NoteClosure{ID: id, Verb: noteDefaultedEvent, Detail: fmt.Sprintf("#%d defaulted: %s", old.Spec.ID, old.Spec.Default), By: old.Status.By, At: metav1.NewTime(timeOf(old.Spec.Due))}
			}
			want.notes[k] = n
			continue
		}
		if closing == nil {
			return fmt.Errorf("note #%d: refusing to close without a recorded reason", old.Spec.ID)
		}
		if n == nil {
			n = old.DeepCopy()
			n.Status.State = noteClosed
			if closing.Verb == noteAnsweredEvent {
				n.Status.State = noteAnswered
			}
			want.notes[k] = n
		}
		at := closing.At
		if at.IsZero() {
			at = time.Now().UTC()
		}
		id, err := nextID(at)
		if err != nil {
			return err
		}
		n.Status.Closure = &v1alpha1.NoteClosure{ID: id, Verb: closing.Verb, Detail: closing.Detail, By: apiParty(closing.By), At: metav1.NewTime(at)}
	}
	return nil
}

// NoteClosure returns a note's retained close reason, even after its default's
// late-answer window and the Kubernetes Event TTL have expired.
func (s *Store) NoteClosure(id int) (*state.Event, error) {
	ctx, cancel := s.ctx()
	defer cancel()
	snap, err := s.load(ctx)
	if err != nil {
		return nil, err
	}
	for _, n := range snap.notes {
		if n.Spec.ID == id && n.Status.Closure != nil {
			c := n.Status.Closure
			return &state.Event{At: c.At.Time, By: party(c.By), Verb: c.Verb, Detail: c.Detail}, nil
		}
	}
	return nil, nil
}
