package gocache

import (
	"os"
	"path/filepath"
	"strings"
)

// goEnv follows the go command's precedence for cache settings: a nonempty
// process variable wins over the user settings maintained by go env -w.
// Missing or unreadable settings are ignored, as they are by cmd/go.
func goEnv(key string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	file := os.Getenv("GOENV")
	if file == "off" {
		return ""
	}
	if file == "" {
		dir, err := os.UserConfigDir()
		if err != nil {
			return ""
		}
		file = filepath.Join(dir, "go", "env")
	}
	data, err := os.ReadFile(file) //nolint:gosec // GOENV deliberately selects the user's Go settings file
	if err != nil {
		return ""
	}
	var value string
	for _, line := range strings.Split(string(data), "\n") {
		if name, v, ok := strings.Cut(line, "="); ok && name == key {
			value = v
		}
	}
	return value
}
