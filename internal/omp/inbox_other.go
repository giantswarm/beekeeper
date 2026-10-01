//go:build !unix

package omp

import "errors"

var errNoFIFO = errors.New("omp agents need a FIFO inbox, which this platform lacks")

// MakeInbox refuses: no FIFO here.
func MakeInbox(string) error { return errNoFIFO }

// Send refuses: no FIFO here.
func Send(string, string) error { return errNoFIFO }
