//go:build !linux

package sandbox

import (
	"context"
	"errors"
	"time"
)

// Serve answers the spool's requests on Linux only, where Claude Code's
// sandbox closes every Unix socket.
func Serve(context.Context, string, string, time.Duration, Handler) error {
	return errors.New("the sandbox broker runs on Linux only")
}

// Origin is the broker's, on Linux only.
func Origin(string, int, []string) (string, []string, error) {
	return "", nil, errors.New("the sandbox broker runs on Linux only")
}
