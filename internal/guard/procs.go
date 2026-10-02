package guard

import (
	"path"
	"regexp"
	"slices"
	"strings"
)

// A process's command line and environment are readable by every process,
// and a program started with -e PASSWORD=…, --token … or -p … carries the
// credential there. So the reads that print whole command lines or
// environments are Secret reads: ps with its args column, pgrep -a,
// pstree -a, /proc/<pid>/cmdline and environ, docker and podman inspect
// without a format limited to names and state, docker ps --no-trunc.

var procSafe = "  beekeeper ps [name|pid…]   (PID, parent, age, CPU, memory and the command line masked)\n" +
	"  ps -eo pid,ppid,etime,time,rss,comm\n" +
	"  pgrep -l <name>   (pgrep -f <pattern> prints PIDs only, pgrep -c a count)\n" +
	"  docker inspect --format '{{.Name}} {{.State.Status}}' <container>"

var (
	// procFile: a process's command line or environment under /proc, after
	// an input redirect or an assignment when the word carries one.
	procFile = regexp.MustCompile(`(?:^|[<=])/proc/[^/\s]+/(?:task/[^/\s]+/)?(?:cmdline|environ)$`)
	// psFullColumn: a ps format column with the whole command line.
	psFullColumn = regexp.MustCompile(`^(?:args|cmd|command)$`)
	// leakyContainerFormat: a docker or podman format that prints the
	// command line, the environment or the whole object.
	leakyContainerFormat = regexp.MustCompile(`(?i)\b(?:Args|Env|Cmd|Entrypoint|Config|Command|Path)\b|\{\{-?\s*(?:json\s+)?\.\s*-?\}\}`)
)

// subcommandHosts take ps as a subcommand of their own: docker ps is no
// ps.
var subcommandHosts = map[string]bool{dockerCmd: true, podmanCmd: true, "nerdctl": true, "crictl": true, "compose": true, "beekeeper": true}

const (
	dockerCmd = "docker"
	podmanCmd = "podman"
	userFlag  = "--user"
)

// procReaders are the commands that name a /proc file without printing it.
var procReaders = map[string]bool{"ls": true, "stat": true, "test": true, "[": true, "wc": true, "file": true}

// Options that take a value, so that the value is not taken for options.
var (
	psValue     = "CGNOUgopqstuk"
	psBSDValue  = "OoptUk"
	pgrepValue  = "dgGPstuUF"
	pstreeValue = "HN"
)

// procLeak returns the leak when the simple command name(args) prints
// whole command lines or environments.
func procLeak(name string, args []string) *leak {
	switch name {
	case "ps":
		if psLeaks(args) {
			return &leak{what: "ps with whole command lines, which may carry credentials", safe: procSafe}
		}
	case "pgrep":
		if shortFlag(args, 'a', pgrepValue) || hasAny(args, "--list-full") {
			return &leak{what: "pgrep -a, which prints whole command lines", safe: procSafe}
		}
	case "pstree":
		if shortFlag(args, 'a', pstreeValue) || hasAny(args, "--arguments") {
			return &leak{what: "pstree -a, which prints whole command lines", safe: procSafe}
		}
	case dockerCmd, podmanCmd:
		if what := containerLeak(name, args); what != "" {
			return &leak{what: what, safe: procSafe}
		}
	}
	return nil
}

// procFileLeak returns the leak when the simple command reads a process's
// command line or environment under /proc.
func procFileLeak(words []string) *leak {
	if len(words) == 0 || procReaders[path.Base(words[0])] {
		return nil
	}
	for _, w := range words {
		if procFile.MatchString(w) {
			return &leak{what: "a read of " + w[strings.Index(w, "/proc/"):] + ", a process's command line or environment", safe: procSafe}
		}
	}
	return nil
}

// psLeaks reports whether ps prints the args column: a format that names
// it, or no format with a full listing (-f, -F) or a BSD-style option
// word, whose default columns end in the command line unless c trims it to
// the name. e (BSD) adds the environment to any command column.
func psLeaks(args []string) bool {
	var formats []string
	full, bsd, trimmed, env := false, false, false, false
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			i = len(args)
		case strings.HasPrefix(a, "--"):
			name, val, eq := strings.Cut(a, "=")
			switch name {
			case "--format":
				if !eq && i+1 < len(args) {
					i++
					val = args[i]
				}
				formats = append(formats, val)
			case "--pid", "--ppid", "--sort", userFlag, "--User", "--group", "--Group", "--tty", "--sid", "--quick-pid", "--cols", "--columns", "--rows", "--lines", "--width":
				if !eq {
					i++
				}
			}
		case strings.HasPrefix(a, "-"):
			i += cluster(a[1:], psValue, args, i, func(letter byte, val string) {
				switch letter {
				case 'f', 'F':
					full = true
				case 'o', 'O':
					formats = append(formats, val)
				}
			})
		case strings.Trim(a, "0123456789,") == "":
			// a PID list
		default:
			bsd = true
			i += cluster(a, psBSDValue, args, i, func(letter byte, val string) {
				switch letter {
				case 'o', 'O':
					formats = append(formats, val)
				case 'c':
					trimmed = true
				case 'e':
					env = true
				}
			})
		}
	}
	for _, f := range formats {
		for _, col := range strings.FieldsFunc(f, func(r rune) bool { return r == ',' || r == ' ' }) {
			name, _, _ := strings.Cut(col, "=")
			name, _, _ = strings.Cut(name, ":")
			if psFullColumn.MatchString(name) {
				return true
			}
		}
	}
	if env {
		return true
	}
	if len(formats) > 0 {
		return false
	}
	return full || bsd && !trimmed
}

// cluster walks the letters of an option cluster, calling fn for each; a
// letter in value takes the rest of the cluster or the next argument as its
// value. It returns how many of the following arguments it consumed.
func cluster(letters, value string, args []string, i int, fn func(letter byte, val string)) int {
	for j := 0; j < len(letters); j++ {
		l := letters[j]
		if !strings.ContainsRune(value, rune(l)) {
			fn(l, "")
			continue
		}
		if rest := strings.TrimPrefix(letters[j+1:], "="); rest != "" {
			fn(l, rest)
			return 0
		}
		if i+1 < len(args) {
			fn(l, args[i+1])
			return 1
		}
		fn(l, "")
		return 0
	}
	return 0
}

// shortFlag reports whether a short option cluster in args holds letter,
// value naming the letters that take a value.
func shortFlag(args []string, letter byte, value string) bool {
	found := false
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			break
		}
		if !strings.HasPrefix(a, "-") || strings.HasPrefix(a, "--") {
			continue
		}
		i += cluster(a[1:], value, args, i, func(l byte, _ string) {
			if l == letter {
				found = true
			}
		})
	}
	return found
}

// containerLeak names the docker or podman read that prints command lines
// or environments: inspect without a narrow --format, ps --no-trunc
// without a format that leaves the command out. "" when it prints none.
func containerLeak(name string, args []string) string {
	sub := nonFlags(args)
	inspect := slices.Contains(sub, "inspect")
	format, hasFormat := "", false
	for i, a := range args {
		n, val, eq := strings.Cut(a, "=")
		// -f is inspect's --format, but ps's --filter.
		if n == "--format" || n == "-f" && inspect {
			if !eq && i+1 < len(args) {
				val = args[i+1]
			}
			format, hasFormat = val, true
		}
	}
	leaky := !hasFormat || leakyContainerFormat.MatchString(format)
	switch {
	case inspect && leaky:
		return name + " inspect, which prints the command line and the environment"
	case (slices.Contains(sub, "ps") || slices.Contains(sub, "ls") || slices.Contains(sub, "list")) && hasAny(args, "--no-trunc") && leaky:
		return name + " ps --no-trunc, which prints whole command lines"
	}
	return ""
}
