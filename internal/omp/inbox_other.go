//go:build !unix

package omp

import (
	"errors"
	"io"
)

var errNoFIFO = errors.New("omp agents need a FIFO inbox, which this platform lacks")

// MakeInbox refuses: no FIFO here.
func MakeInbox(string) error { return errNoFIFO }

// Send refuses: no FIFO here.
func Send(string, string) error { return errNoFIFO }

// Write refuses: no FIFO here.
func Write(string, func(io.Writer) error) error { return errNoFIFO }

// RemoveInbox has nothing to remove: no FIFO here.
func RemoveInbox(string) (bool, error) { return false, nil }
