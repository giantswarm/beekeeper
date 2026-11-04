package proc

import (
	"path/filepath"
	"slices"
	"strings"
)

// MaskedCommand returns a command line safe to print and whether values were omitted.
// It retains the executable, safe subcommands and flag names, masking flag
// values, assignments, URLs and attached short-option values.
func MaskedCommand(args []string) (string, bool) {
	if len(args) == 1 {
		// A program that rewrote its argv into one line (Electron,
		// setproctitle) keeps its words in the first argument.
		args = strings.Fields(args[0])
	}
	if len(args) == 0 {
		return "", false
	}
	out := []string{filepath.Base(args[0])}
	flags, cut := false, false
	for _, a := range args[1:] {
		switch {
		case strings.HasPrefix(a, "-"):
			flags = true
			name, _, eq := strings.Cut(a, "=")
			if !eq && !strings.HasPrefix(a, "--") && len(a) > 2 {
				name = a[:2]
			}
			cut = cut || name != a
			if !slices.Contains(out, name) {
				out = append(out, name)
			}
		case !flags && !strings.ContainsAny(a, ":@="):
			out = append(out, a)
		default:
			cut = true
		}
	}
	return strings.Join(out, " "), cut
}
