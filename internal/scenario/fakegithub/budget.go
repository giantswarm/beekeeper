//go:build scenario

package fakegithub

import (
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"time"
)

// The rate limit buckets, as /rate_limit and X-RateLimit-Resource name
// them.
const (
	bucketCore       = "core"
	bucketSearch     = "search"
	bucketCodeSearch = "code_search"
	bucketGraphQL    = "graphql"
)

// Buckets are the buckets in the order they are reported.
var Buckets = []string{bucketCore, bucketGraphQL, bucketSearch, bucketCodeSearch}

// bucket is one rate limit: so many points per window.
type bucket struct {
	Limit  int
	Used   int
	Reset  time.Time
	window time.Duration
}

func (b *bucket) remaining() int { return max(0, b.Limit-b.Used) }

// secondaryLimit is a secondary rate limit: more than Max requests within
// Per are refused with Retry-After.
type secondaryLimit struct {
	Max        int
	Per        time.Duration
	RetryAfter time.Duration
	seen       []time.Time
}

// budgets are the buckets and what each caller spent of them.
type budgets struct {
	buckets   map[string]*bucket
	spent     map[string]map[string]int
	secondary *secondaryLimit
}

func newBudgets(now time.Time) *budgets {
	b := &budgets{buckets: map[string]*bucket{}, spent: map[string]map[string]int{}}
	for name, limit := range map[string]int{bucketCore: 5000, bucketGraphQL: 5000, bucketSearch: 30, bucketCodeSearch: 10} {
		window := time.Hour
		if name == bucketSearch || name == bucketCodeSearch {
			window = time.Minute
		}
		b.buckets[name] = &bucket{Limit: limit, Reset: now.Add(window), window: window}
	}
	return b
}

// roll starts a new window for every bucket whose reset has come: on the
// scenario's clock, so advancing it by an hour resets the hourly buckets.
func (b *budgets) roll(now time.Time) {
	for _, bk := range b.buckets {
		if !now.Before(bk.Reset) {
			bk.Used, bk.Reset = 0, now.Add(bk.window)
		}
	}
}

// charge takes points from a bucket for a caller; false when the bucket
// does not hold them, and then nothing is taken.
func (b *budgets) charge(name, caller string, points int) bool {
	bk := b.buckets[name]
	if bk.Used+points > bk.Limit {
		return false
	}
	bk.Used += points
	if b.spent[caller] == nil {
		b.spent[caller] = map[string]int{}
	}
	b.spent[caller][name] += points
	return true
}

// headers are a bucket's X-RateLimit headers.
func (b *budgets) headers(h http.Header, name string) {
	bk := b.buckets[name]
	h.Set("X-RateLimit-Limit", strconv.Itoa(bk.Limit))
	h.Set("X-RateLimit-Remaining", strconv.Itoa(bk.remaining()))
	h.Set("X-RateLimit-Used", strconv.Itoa(bk.Used))
	h.Set("X-RateLimit-Reset", strconv.FormatInt(bk.Reset.Unix(), 10))
	h.Set("X-RateLimit-Resource", name)
}

// tooFast counts a request against the secondary limit and says how long
// to wait when it is over it.
func (b *budgets) tooFast(now time.Time) (time.Duration, bool) {
	s := b.secondary
	if s == nil || s.Max <= 0 {
		return 0, false
	}
	s.seen = slices.DeleteFunc(s.seen, func(t time.Time) bool { return !t.After(now.Add(-s.Per)) })
	if len(s.seen) >= s.Max {
		return s.RetryAfter, true
	}
	s.seen = append(s.seen, now)
	return 0, false
}

// rateLimitBody is GET /rate_limit.
func (b *budgets) rateLimitBody() obj {
	resources := obj{}
	for name, bk := range b.buckets {
		resources[name] = obj{"limit": bk.Limit, "remaining": bk.remaining(), "used": bk.Used, "reset": bk.Reset.Unix(), "resource": name}
	}
	core := b.buckets[bucketCore]
	return obj{"resources": resources, "rate": obj{"limit": core.Limit, "remaining": core.remaining(), "used": core.Used, "reset": core.Reset.Unix(), "resource": bucketCore}}
}

// BudgetSpec sets a bucket: its limit, what remains of it and when it
// resets. A zero field keeps what the bucket has.
type BudgetSpec struct {
	Limit     *int
	Remaining *int
	ResetIn   time.Duration
}

// set applies a spec to a bucket.
func (b *budgets) set(name string, s BudgetSpec, now time.Time) error {
	bk, ok := b.buckets[name]
	if !ok {
		return fmt.Errorf("no rate limit bucket %q (%v)", name, Buckets)
	}
	remaining := bk.remaining()
	if s.Limit != nil {
		bk.Limit = *s.Limit
		remaining = min(remaining, bk.Limit)
		if s.Remaining == nil {
			remaining = bk.Limit - min(bk.Used, bk.Limit)
		}
	}
	if s.Remaining != nil {
		remaining = *s.Remaining
	}
	if remaining > bk.Limit || remaining < 0 {
		return fmt.Errorf("bucket %s: remaining %d is outside its limit %d", name, remaining, bk.Limit)
	}
	bk.Used = bk.Limit - remaining
	if s.ResetIn > 0 {
		bk.Reset = now.Add(s.ResetIn)
	}
	return nil
}

// Spend is what a caller spent of a bucket.
type Spend struct {
	Caller string `json:"caller"`
	Bucket string `json:"bucket"`
	Points int    `json:"points"`
}

// BucketState is a bucket as an expectation reads it.
type BucketState struct {
	Bucket    string    `json:"bucket"`
	Limit     int       `json:"limit"`
	Remaining int       `json:"remaining"`
	Used      int       `json:"used"`
	Reset     time.Time `json:"reset"`
}
