package forge

import (
	"errors"
	"fmt"
	"time"
)

// ErrorKind classifies a failed read.
type ErrorKind string

// Error kinds.
const (
	// Transient errors (5xx, timeouts) are retried on the engine's clock: 10s, doubling, capped.
	Transient ErrorKind = "transient"
	// Refused reads (404, 403 without rate limit, 410, 422) are not retried until their inputs change.
	Refused ErrorKind = "refused"
	// RateLimited means a primary rate limit: no call to that API until ResetAt.
	RateLimited ErrorKind = "rate-limited"
	// SecondaryRateLimited is honoured with RetryAfter.
	SecondaryRateLimited ErrorKind = "secondary-rate-limited"
	// Deferred means the observation reserve kept the read from being made; it waits for ResetAt.
	Deferred ErrorKind = "deferred"
)

// Error is a failed read.
type Error struct {
	Kind      ErrorKind
	API       string
	Operation string
	Code      int // HTTP status, 0 when no request was made
	// ResetAt is when a rate limit or the reserve lifts.
	ResetAt time.Time
	// RetryAfter is the wait a secondary rate limit asks for.
	RetryAfter time.Duration
	Err        error
}

func (e *Error) Error() string {
	msg := fmt.Sprintf("%s %s: %s", e.API, e.Operation, e.Kind)
	if e.Code != 0 {
		msg += fmt.Sprintf(" (%d)", e.Code)
	}
	if e.Err != nil {
		msg += ": " + e.Err.Error()
	}
	return msg
}

func (e *Error) Unwrap() error { return e.Err }

// AsError returns err's *Error, if any.
func AsError(err error) (*Error, bool) {
	var fe *Error
	ok := errors.As(err, &fe)
	return fe, ok
}

// IsKind reports whether err is a forge error of kind k.
func IsKind(err error, k ErrorKind) bool {
	fe, ok := AsError(err)
	return ok && fe.Kind == k
}
