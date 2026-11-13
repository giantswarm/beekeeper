//go:build !unix

package platform

import "github.com/giantswarm/beekeeper/internal/proc"

// Detach does nothing where there are no sessions.
func Detach(*proc.Cmd) {}
