//go:build !unix

package codex

import "github.com/giantswarm/beekeeper/internal/proc"

func configureTurnCmd(*proc.Cmd)     {}
func stopTurnProcessGroup(*proc.Cmd) {}
