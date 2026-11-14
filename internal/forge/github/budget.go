package github

import (
	"net/http"
	"strconv"
	"time"

	"github.com/giantswarm/beekeeper/internal/forge"
	"github.com/giantswarm/beekeeper/internal/forge/github/wire"
)

// bucket is one API's rate limit as GitHub last reported it.
type bucket struct {
	known     bool
	limit     int
	remaining int
	used      int
	resetAt   time.Time
	// stoppedUntil is set by a primary rate limit: no call to the API before it.
	stoppedUntil time.Time
}

// roll starts a fresh window once the reset time has passed: GitHub restores the full limit then.
func (b *bucket) roll(now time.Time) {
	if b.known && !b.resetAt.IsZero() && !now.Before(b.resetAt) && (b.remaining < b.limit || b.used > 0) {
		b.remaining, b.used = b.limit, 0
	}
	if !b.stoppedUntil.IsZero() && !now.Before(b.stoppedUntil) {
		b.stoppedUntil = time.Time{}
	}
}

// pause reports whether a call of class on api must not be made now, why, and until when.
func (c *Client) pauseLocked(api, class string, infra bool, now time.Time) (forge.ErrorKind, time.Time) {
	b := c.buckets[api]
	b.roll(now)
	if c.secondaryUntil.After(now) {
		return forge.SecondaryRateLimited, c.secondaryUntil
	}
	if b.stoppedUntil.After(now) {
		return forge.RateLimited, b.stoppedUntil
	}
	if b.known && b.remaining <= 0 && now.Before(b.resetAt) {
		return forge.RateLimited, b.resetAt
	}
	if !infra && class == forge.ClassObservation && c.currentReserve() > 0 && b.known && now.Before(b.resetAt) &&
		float64(b.remaining) <= c.currentReserve()*float64(b.limit) {
		return forge.Deferred, b.resetAt
	}
	return "", time.Time{}
}

// admit refuses a call while its API is stopped or observation is deferred; no request is made then.
func (c *Client) admit(cl call) error {
	now := c.cfg.Clock.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	kind, until := c.pauseLocked(cl.api, cl.class, cl.infra, now)
	if kind == "" {
		return nil
	}
	fe := &forge.Error{Kind: kind, API: cl.api, Operation: cl.op, ResetAt: until}
	if kind == forge.SecondaryRateLimited {
		fe.RetryAfter = until.Sub(now)
	}
	if kind == forge.Deferred {
		c.deferred[cl.api]++
	}
	return fe
}

// updateFromHeaders follows the rate-limit headers of an answer (a 304 carries them too).
func (c *Client) updateFromHeaders(api string, h http.Header, now time.Time) *forge.APILimit {
	limit, okL := headerInt(h, wire.HeaderRateLimit)
	remaining, okR := headerInt(h, wire.HeaderRateRemaining)
	if !okL || !okR {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	b := c.buckets[api]
	b.known = true
	b.limit, b.remaining = limit, remaining
	if used, ok := headerInt(h, wire.HeaderRateUsed); ok {
		b.used = used
	} else {
		b.used = limit - remaining
	}
	if t, ok := headerTime(h, wire.HeaderRateReset); ok {
		b.resetAt = t
	}
	l := c.limitLocked(api, now)
	return &l
}

func (c *Client) limitSnapshot(api string, now time.Time) *forge.APILimit {
	c.mu.Lock()
	defer c.mu.Unlock()
	l := c.limitLocked(api, now)
	return &l
}

func (c *Client) limitLocked(api string, now time.Time) forge.APILimit {
	kind, until := c.pauseLocked(api, forge.ClassObservation, false, now)
	b := c.buckets[api]
	return forge.APILimit{
		Limit:       b.limit,
		Remaining:   b.remaining,
		ResetAt:     b.resetAt,
		Used:        b.used,
		Known:       b.known,
		Paused:      kind != "",
		PausedUntil: until,
	}
}

// Budget returns the rate limits as GitHub last reported them (a window whose reset has passed is
// full again), whether observation is paused on each API, and the reads the reserve deferred.
func (c *Client) Budget() forge.Budget {
	now := c.cfg.Clock.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	return forge.Budget{
		REST:            c.limitLocked(forge.APIREST, now),
		GraphQL:         c.limitLocked(forge.APIGraphQL, now),
		App:             c.limitLocked(forge.APIApp, now),
		DeferredREST:    c.deferred[forge.APIREST],
		DeferredGraphQL: c.deferred[forge.APIGraphQL],
		DeferredApp:     c.deferred[forge.APIApp],
	}
}

func headerInt(h http.Header, name string) (int, bool) {
	v := h.Get(name)
	if v == "" {
		return 0, false
	}
	n, err := strconv.Atoi(v)
	return n, err == nil
}
