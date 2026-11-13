//go:build !unix

package cmd

import (
	"time"

	"github.com/giantswarm/beekeeper/internal/proc"
)

// armServeModeSwitch ends the switch command when the switch is cancelled.
// Without a unix process group, the command's own process is what the
// context can reach; its timeout still stands.
func armServeModeSwitch(c *proc.Cmd) {
	c.Cancel = func() error {
		if c.Process == nil {
			return nil
		}
		return c.Process.Kill()
	}
	c.WaitDelay = 5 * time.Second
}
