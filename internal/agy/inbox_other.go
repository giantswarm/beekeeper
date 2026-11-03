//go:build !unix

package agy

import "errors"

var errNoFIFO = errors.New("agy agents need a FIFO inbox, which this platform lacks")

// MakeInbox refuses: no FIFO here.
func MakeInbox(string) error { return errNoFIFO }

// Send refuses: no FIFO here.
func Send(string, string) error { return errNoFIFO }

// Drain refuses: no FIFO here.
func Drain(string) ([]string, error) { return nil, errNoFIFO }

func drainHook(string, string) ([]string, error) { return nil, errNoFIFO }

// Confirm refuses: no FIFO here.
func Confirm(string) error { return errNoFIFO }

func popInflight(string) (string, bool, error) { return "", false, errNoFIFO }
func finishTurn(string) error                  { return errNoFIFO }
func restoreInflight(string) error             { return errNoFIFO }

// RemoveInbox has nothing to remove: no FIFO here.
func RemoveInbox(string) (bool, error) { return false, nil }
