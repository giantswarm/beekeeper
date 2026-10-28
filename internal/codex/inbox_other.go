//go:build !unix

package codex

import (
	"errors"
	"os"
)

var errUnsupported = errors.New("codex FIFO inbox is only supported on unix")

func MakeInbox(string) error                    { return errUnsupported }
func RemoveInbox(string) (bool, error)          { return false, errUnsupported }
func Send(string, string) error                 { return errUnsupported }
func Drain(string) ([]string, error)            { return nil, errUnsupported }
func DrainHook(string) ([]QueuedMessage, error) { return nil, errUnsupported }
func Confirm(string) error                      { return errUnsupported }
func ConfirmInjected(string) error              { return errUnsupported }
func openInboxRDWR(string) (*os.File, error)    { return nil, errUnsupported }
func withLock(_ string, fn func() error) error  { return fn() }
