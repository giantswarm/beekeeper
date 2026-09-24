// beekeeper keeps the Claude Code sessions sharing one machine working
// together: it shows what every session does, watches the machine, reads the
// GitHub budget they share and holds the shared resources one session at a
// time.
package main

import (
	"fmt"
	"os"

	"github.com/giantswarm/beekeeper/cmd"
)

func main() {
	if err := cmd.New().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "beekeeper:", err)
		os.Exit(cmd.Code(err))
	}
}
