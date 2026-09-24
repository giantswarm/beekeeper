// beekeeper keeps the Claude Code sessions sharing one machine working
// together: it shows what every session does, watches the machine, reads the
// GitHub budget they share and holds the shared resources one session at a
// time.
package main

import (
	"os"

	"github.com/giantswarm/beekeeper/cmd"
)

func main() {
	os.Exit(cmd.Main())
}
