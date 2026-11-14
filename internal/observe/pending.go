package observe

import (
	"sync"

	"github.com/giantswarm/beekeeper/api/v1alpha1"
	"github.com/giantswarm/beekeeper/internal/decide"
	"github.com/giantswarm/beekeeper/internal/forge"
)

// Pending is what an object's next reconcile has to observe, and why.
type Pending struct {
	Trigger decide.Trigger
	// Source is how what the reads find is attributed (webhook, resync, reobserve, startup).
	Source string

	// ReadBoard re-reads an Item's board item by node id.
	ReadBoard bool
	// Board is an Item's board item as a resync or resolve already read it; Found is false when the
	// board no longer has it.
	Board *forge.BoardItem
	Found bool

	// Reads are a Change's conditional reads.
	Reads ReadSet

	// Events are the webhook events that asked for these reads. A webhook vouches only for what its own
	// event covers: a change the reads find that another event would have announced is still missed.
	Events []string
	// Deliveries are the inbox deliveries that asked for these reads; each is acknowledged once every
	// read it asked for ran (a leader that stops before leaves them for the next one).
	Deliveries []string
}

// sourceRank orders sources for attribution: a change seen through a webhook is never missed, so
// webhook wins over every other source.
var sourceRank = map[string]int{
	v1alpha1.SourceWebhook:   0,
	v1alpha1.SourceStartup:   1,
	v1alpha1.SourceResync:    2,
	v1alpha1.SourceReobserve: 3,
	v1alpha1.SourceDeadline:  4,
	v1alpha1.SourceEngine:    5,
}

func rank(source string) int {
	if r, ok := sourceRank[source]; ok {
		return r
	}
	return len(sourceRank)
}

// Merge adds q to p: reads add up, the board data read last wins, and the trigger and source of the
// better-ranked source are kept, except that a wake asking for no reads never takes the attribution of
// reads another source asked for.
func (p *Pending) Merge(q Pending) {
	switch {
	case p.Source == "":
		p.Source, p.Trigger = q.Source, q.Trigger
	case p.Empty() != q.Empty():
		if p.Empty() {
			p.Source, p.Trigger = q.Source, q.Trigger
		}
	case rank(q.Source) < rank(p.Source):
		p.Source, p.Trigger = q.Source, q.Trigger
	}
	p.ReadBoard = p.ReadBoard || q.ReadBoard
	if q.Board != nil {
		p.Board, p.Found = q.Board, q.Found
	}
	p.Reads.Add(q.Reads)
	p.Events = union(p.Events, q.Events)
	p.Deliveries = union(p.Deliveries, q.Deliveries)
}

// union adds the entries of b missing from a, keeping a's order; the result never aliases b.
func union(a, b []string) []string {
	for _, v := range b {
		if !contains(a, v) {
			a = append(a, v)
		}
	}
	return a
}

// Empty reports whether nothing is to be observed.
func (p *Pending) Empty() bool { return !p.ReadBoard && p.Board == nil && p.Reads.Empty() }

// PendingSet holds the pending observations of objects, keyed by kind/name.
type PendingSet struct {
	mu sync.Mutex
	m  map[string]*Pending
}

// Add merges p into key's pending observation.
func (s *PendingSet) Add(key string, p Pending) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.m == nil {
		s.m = map[string]*Pending{}
	}
	cur, ok := s.m[key]
	if !ok {
		cp := p
		cp.Reads.SHAs = append([]string(nil), p.Reads.SHAs...)
		cp.Events = append([]string(nil), p.Events...)
		cp.Deliveries = append([]string(nil), p.Deliveries...)
		s.m[key] = &cp
		return
	}
	cur.Merge(p)
}

// HasDelivery reports whether key's pending observation was asked for by delivery id.
func (s *PendingSet) HasDelivery(key, id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.m[key]
	return ok && contains(p.Deliveries, id)
}

// Take removes and returns key's pending observation (nil when there is none).
func (s *PendingSet) Take(key string) *Pending {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.m[key]
	delete(s.m, key)
	return p
}

// Has reports whether key has a pending observation.
func (s *PendingSet) Has(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.m[key]
	return ok
}

// Drop forgets key's pending observation and returns it (nil when there was none).
func (s *PendingSet) Drop(key string) *Pending {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.m[key]
	delete(s.m, key)
	return p
}

// Clear forgets every pending observation (on losing leadership).
func (s *PendingSet) Clear() {
	s.mu.Lock()
	s.m = nil
	s.mu.Unlock()
}
