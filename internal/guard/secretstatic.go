package guard

import "strings"

// shellSyntaxOnly recognizes a conservative set of non-interactive shell
// options. Stop at the script operand: later arguments cannot enable noexec.
// Unknown options (especially -i, +n and -c) retain execution scanning.
func shellSyntaxOnly(name string, args []string) bool {
	if !localShells[name] {
		return false
	}
	noexec := false
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			return noexec
		}
		if a == "-o" {
			if i+1 >= len(args) || args[i+1] != "noexec" {
				return false
			}
			i++
			noexec = true
			continue
		}
		if !strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "+") {
			return noexec
		}
		if !strings.HasPrefix(a, "-") || len(a) < 2 {
			return false
		}
		for _, option := range a[1:] {
			switch option {
			case 'n':
				noexec = true
			case 'v', 'x':
			default:
				return false
			}
		}
	}
	return noexec
}
