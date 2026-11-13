//go:build !unix

package cmd

import "github.com/giantswarm/beekeeper/internal/proc"

// mergeCheckGroup leaves c as it is where there are no process groups.
func mergeCheckGroup(*proc.Cmd) {}
