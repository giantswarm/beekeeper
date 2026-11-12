package cmd

import (
	"strings"
)

// RemovedAgent reports whether detail, an agents.remove line's, names name.
func RemovedAgent(detail, name string) bool {
	return detail == name || strings.HasPrefix(detail, name+": ")
}
