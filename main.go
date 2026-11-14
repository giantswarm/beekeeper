// Command beekeeper is the engine's one binary; its roles are subcommands (engine, receiver, migrate,
// replay, version).
package main

import (
	"os"

	"github.com/giantswarm/beekeeper/cmd"
)

func main() { os.Exit(cmd.Execute(os.Args[1:])) }
