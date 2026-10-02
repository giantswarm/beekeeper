package guard

// Commands splits a shell command line into the words of its simple
// commands, quotes removed, comments and here-document bodies left out.
func Commands(cmd string) [][]string {
	sc := scanShell(cmd)
	var out [][]string
	for _, sg := range sc.segments() {
		if words := shellWords(sc.plain[sg.start:sg.end]); len(words) > 0 {
			out = append(out, words)
		}
	}
	return out
}
