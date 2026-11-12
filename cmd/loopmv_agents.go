package cmd

import (
	"strings"
)

// removedAgent reports whether detail, an agents.remove line's, names name.
func removedAgent(detail, name string) bool {
	return detail == name || strings.HasPrefix(detail, name+": ")
}
