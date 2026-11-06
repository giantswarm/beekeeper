//go:build !linux

package proc

import "errors"

func openProcessHandle(int) (processHandle, error) {
	return nil, errors.New("stable process handles are unavailable")
}
