//go:build !unix

package secret

import "fmt"

// producerPath refuses every producer: only a unix system's root-owned
// directories are a place beekeeper trusts one in.
func producerPath(name string) (string, error) {
	return "", fmt.Errorf("the producer %s runs on a unix system only: nothing run", name)
}
