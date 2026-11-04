package guard

import (
	"os"
	"path"
	"runtime"
	"strings"
)

// Cross-target run and test can execute through binfmt handlers, including
// desktop emulators. A successful build does not make execution safe.
func crossGoRefusal(cmd string) string {
	return scanCrossGo(cmd, os.Getenv("GOOS"), 0)
}

func scanCrossGo(cmd, inherited string, depth int) string {
	if depth > 4 {
		return ""
	}
	for _, raw := range rawCommands(scanShell(cmd).plain) {
		words := make([]string, len(raw))
		for i, r := range raw {
			words[i] = strings.Join(shellWords(r), "")
		}
		k := commandAt(words)
		if k == len(words) {
			for _, w := range words {
				if v, ok := strings.CutPrefix(w, "GOOS="); ok {
					inherited = v
				}
			}
			continue
		}
		target := inherited
		for _, w := range words[:k] {
			if v, ok := strings.CutPrefix(w, "GOOS="); ok {
				target = v
			}
		}
		name := path.Base(words[k])
		// Retain explicit shell assignments for later commands in this script.
		if name == "export" {
			for _, w := range words[k+1:] {
				if v, ok := strings.CutPrefix(w, "GOOS="); ok {
					inherited = v
				}
			}
		}
		if shells[name] || name == evalCmd {
			for _, a := range words[k+1:] {
				if strings.ContainsAny(a, " \t\n;|&") {
					if r := scanCrossGo(a, target, depth+1); r != "" {
						return r
					}
				}
			}
		}
		if name != "go" || target == "" || target == runtime.GOOS || k+1 >= len(words) {
			continue
		}
		sub := words[k+1]
		if sub != "run" && sub != "test" {
			continue
		}
		exec := false
		args := words[k+2:]
		for i := 0; i < len(args); i++ {
			a := args[i]
			if a == "-args" || a == "--" {
				break
			}
			if value, ok := strings.CutPrefix(a, "-exec="); ok {
				exec = strings.TrimSpace(value) != ""
			}
			if a == "-exec" && i+1 < len(args) {
				i++
				exec = strings.TrimSpace(args[i]) != ""
			}
		}
		if sub == "test" && exec {
			continue
		}
		return "Refused: GOOS=" + target + " go " + sub + " may execute a non-host binary through an emulator. Build for the host (GOOS=" + runtime.GOOS + " go build) and run the host binary; for cross-compilation use GOOS=" + target + " go build. For cross-scans, build the scanner for the host, then run that host binary with GOOS=" + target + ". Cross-target go test requires an explicit -exec runner."
	}
	return ""
}
