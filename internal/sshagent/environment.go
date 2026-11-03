// Package sshagent selects a local SSH agent location for child sessions.
package sshagent

import (
	"os"
	"path/filepath"
	"strings"
)

// Environment replaces SSH_AUTH_SOCK with the user-session socket when it
// exists, otherwise preserves a valid inherited socket. An explicit empty
// value masks stale environment inherited from a service manager. Only file
// metadata is inspected; selecting a socket does not read agent credentials.
func Environment(env []string) []string {
	var runtimeDir, inherited string
	out := make([]string, 0, len(env)+1)
	for _, entry := range env {
		key, value, _ := strings.Cut(entry, "=")
		if key == "SSH_AUTH_SOCK" {
			inherited = value
			continue
		}
		if key == "XDG_RUNTIME_DIR" {
			runtimeDir = value
		}
		out = append(out, entry)
	}
	socket := ""
	if runtimeDir != "" && filepath.IsAbs(runtimeDir) {
		candidate := filepath.Join(runtimeDir, "ssh-agent.socket")
		if isSocket(candidate) {
			socket = candidate
		}
	}
	if socket == "" && isSocket(inherited) {
		socket = inherited
	}
	return append(out, "SSH_AUTH_SOCK="+socket)
}

func isSocket(path string) bool {
	if path == "" || !filepath.IsAbs(path) {
		return false
	}
	info, err := os.Stat(path)
	return err == nil && info.Mode()&os.ModeSocket != 0
}
