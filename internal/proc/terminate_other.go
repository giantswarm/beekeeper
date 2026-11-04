//go:build !linux

package proc

import "errors"

func openMatchingProcess(int) (processSignals, error) {
	return processSignals{}, errors.New("matching process termination requires Linux pidfds")
}
