package guard

import (
	"path"
	"regexp"
	"slices"
	"strings"
)

// The Secret guard refuses a Bash command that would print secret values:
// the output reaches the transcript and the model API. A command passes when
// its output keeps only keys, metadata or a hash, or goes to a file, a
// variable or a consumer that prints nothing of it (kubectl apply -f -).
// False positives beat leaks.

const (
	kubectlCmd = "kubectl"
	verbGet    = "get"
)

// A leak names the command that would print secret values and the safe forms.
type leak struct {
	what, safe string
	// at is the command of the line that prints them.
	at string
}

func (l leak) reason() string {
	at := strings.Join(strings.Fields(l.at), " ")
	if len(at) > 200 {
		at = at[:200] + "…"
	}
	return "Refused: `" + at + "` (" + l.what + ") would print secret values into the transcript, and from there to the model API. " +
		"Keep only keys, metadata or a hash of a value, or write the value to a file or a variable:\n" + l.safe +
		"\nThe same holds for a pipeline's later stages: grep, head or cut still print the values; jq keys, sha256sum, wc and > file do not."
}

var (
	kubectlSafe = "  kubectl get secret <name> -o json | jq '.data|keys'\n" +
		"  kubectl get secrets -o name\n" +
		"  kubectl get secret <name> -o jsonpath='{.data.<key>}' | base64 -d | sha256sum\n" +
		"  kubectl describe secret <name>   (sizes only)"
	sopsSafe = "  sops -d <file> | yq '.data|keys'   (or .stringData, or the top-level keys)\n" +
		"  sops -d --output <plain-file> <file>\n" +
		"  sops -d <file> | kubectl apply -f -"
	opSafe = "  op read --out-file <file> <ref>\n" +
		"  op read <ref> | sha256sum\n" +
		"  op run -- <command>   (masks the values in the output)"
	vaultSafe = "  vault kv get -field=<key> <path> > <file>\n" +
		"  vault kv get -format=json <path> | jq '.data.data|keys'\n" +
		"  vault kv metadata get <path>"
	base64Safe = "  … | base64 -d | sha256sum\n" +
		"  … | base64 -d > <file>"
)

var (
	// assignment: the command before a $( that captures its output in a
	// variable or a flag's value.
	assignment = regexp.MustCompile(`^\s*(?:(?:export|local|readonly|declare|typeset)(?:\s+-\w+)*\s+)?(?:\w+=\S*\s+)*\w+="?$|\s--?[\w-]+=[^\s"']*"?$`)
	// redirect: stdout to a file; group 1 is the target.
	redirect  = regexp.MustCompile(`(?:^|[^\d<>&])(?:1|&)?>>?\|?\s*([^\s;&|()<>]+)`)
	terminal  = regexp.MustCompile(`^/(?:dev/(?:stdout|stderr|tty|fd/[12])|proc/self/fd/[12])$`)
	secretRes = regexp.MustCompile(`(?i)^secrets?(?:\.v1)?\.?$`)
	dataRef   = regexp.MustCompile(`\.(?:data|stringData)\b`)
	// wholeObject: a template that prints an object with its data.
	wholeObject = regexp.MustCompile(`(?i)\b(?:string)?data\b|\{\s*[@.]\s*\}|\.\.|\{\s*\.items\s*(?:\[[^\]]*\])?\s*\}|\{\{-?\s*(?:json|toJson|toYaml|toPrettyJson)?\s*\.\s*-?\}\}|:=\s*\.\s*-?\}\}`)
	// keysOnly: a jq or yq filter part that keeps only the keys of the data.
	keysOnly = regexp.MustCompile(`(?:\.\w+|\[[^\]]*\])*\.(?:data|stringData)(?:\s*//\s*\{\}\s*\))?\s*\|\s*(?:keys_unsorted|keys|length)\b(?:\[\])?|\.value\s*\|\s*length\b`)
	// templateKeys: a template range over the data that binds the value to
	// group 1, or the data's length.
	templateKeys = regexp.MustCompile(`range\s+\$\w+\s*,\s*\$(\w+)\s*:=\s*\.(?:data|stringData)\b|len\s+\.(?:data|stringData)\b`)
	// leakyFilter: what prints values once the keys-only parts are gone.
	leakyFilter = regexp.MustCompile(`(?i)\b(?:string)?data\b|\bvalues?\b|\.\.|to_entries|with_entries|@base64d|\$ENV|\benv\b|\binputs?\b|tostream|paths|getpath|(?:^|[\s,:(|])\.(?:\s*(?:[,})\]|]|$)|\[\s*(?:\]|"))`)
	// kubectlFunc, kubectlVar: a shell function or a variable that runs kubectl.
	kubectlFunc = regexp.MustCompile(`(?:^|[\s;&|(])(?:function\s+([\w-]+)\s*(?:\(\))?|([\w-]+)\s*\(\))\s*\{[^}]*?(?:^|[\s/])kubectl\s`)
	kubectlVar  = regexp.MustCompile(`(?:^|[\s;&|(])(\w+)=\(?\s*["']?(?:[^\s"'()]*/)?kubectl(?:\s|["')])`)
	// harmlessFilter: a filter that names only keys or metadata.
	harmlessFilter = regexp.MustCompile(`\b(?:KEYS|keys|keys_unsorted|length)\b|\.(?:metadata|kind|type|apiVersion|name|namespace|label|id|title)\b`)
	shells         = map[string]bool{"sh": true, "bash": true, "zsh": true, "ksh": true, "dash": true, "ssh": true, "eval": true, "watch": true}
	hashes         = map[string]bool{"sha256sum": true, "sha1sum": true, "sha224sum": true, "sha384sum": true, "sha512sum": true, "md5sum": true, "b2sum": true, "cksum": true, "shasum": true, "wc": true}
	quietSinks     = map[string]bool{"wl-copy": true, "xclip": true, "xsel": true, "kubeseal": true}
)

// kubectl flags that take a value, so that the value is not taken for the
// verb or a resource.
var kubectlValue = map[string]bool{
	"-n": true, "--namespace": true, "--context": true, "--kubeconfig": true, "--cluster": true, "--user": true,
	"-s": true, "--server": true, "--as": true, "--as-group": true, "--token": true, "--request-timeout": true,
	"-l": true, "--selector": true, "-o": true, "--output": true, "--field-selector": true, "--template": true,
	"-f": true, "--filename": true, "-c": true, "--container": true, "--sort-by": true, "--chunk-size": true, "--label-columns": true, "-L": true,
}

// secretLeak returns the leak cmd would print, nil when it prints none.
func secretLeak(cmd string) *leak {
	return scanLeaks(cmd, 0)
}

func scanLeaks(cmd string, depth int) *leak {
	if depth > 4 {
		return nil
	}
	sc := scanShell(cmd)
	segs := sc.segments()
	aliases := map[string]bool{kubectlCmd: true}
	for _, re := range []*regexp.Regexp{kubectlFunc, kubectlVar} {
		for _, m := range re.FindAllStringSubmatch(sc.plain, -1) {
			aliases[strings.Join(m[1:], "")] = true
		}
	}
	for i, sg := range segs {
		words := shellWords(sc.plain[sg.start:sg.end])
		// A command string a shell runs is a command of its own.
		for k, w := range words {
			if !shells[path.Base(w)] {
				continue
			}
			for _, arg := range words[k+1:] {
				if strings.ContainsAny(arg, " \t\n|;") {
					if l := scanLeaks(arg, depth+1); l != nil {
						return l
					}
				}
			}
			for _, h := range sc.heredocs {
				if h.op >= sg.start && h.op < sg.end {
					if l := scanLeaks(cmd[h.start:h.end], depth+1); l != nil {
						return l
					}
				}
			}
			break
		}
		l := sourceLeak(words, pipelineBefore(sc.plain, segs, i), aliases)
		if l == nil || flowsSafely(sc, segs, i) {
			continue
		}
		first, last := i, i
		for first > 0 && segs[first-1].after == "|" {
			first--
		}
		for last+1 < len(segs) && segs[last].after == "|" {
			last++
		}
		l.at = sc.plain[segs[first].start:segs[last].end]
		return l
	}
	return nil
}

// pipelineBefore is the text of the pipeline stages before segs[i].
func pipelineBefore(cmd string, segs []segment, i int) string {
	first := i
	for first > 0 && segs[first-1].after == "|" {
		first--
	}
	return cmd[segs[first].start:segs[i].start]
}

// sourceLeak returns the leak when the simple command prints secret values.
func sourceLeak(words []string, before string, aliases map[string]bool) *leak {
	for k, w := range words {
		args := words[k+1:]
		name := path.Base(w)
		if aliases[strings.Trim(strings.TrimSuffix(strings.TrimSuffix(w, "[@]}"), "[*]}"), "${}")] {
			name = kubectlCmd
		}
		switch name {
		case kubectlCmd:
			if l := kubectlLeak(args, before); l != nil {
				return l
			}
		case "kubectl-view_secret":
			return &leak{what: "kubectl view-secret", safe: kubectlSafe}
		case "sops":
			if hasAny(args, "-d", "--decrypt", "decrypt") && !hasFlag(args, "--output", "-i", "--in-place") {
				return &leak{what: "sops -d", safe: sopsSafe}
			}
		case "op":
			if l := opLeak(args); l != nil {
				return l
			}
		case "vault":
			if l := vaultLeak(args); l != nil {
				return l
			}
		case "base64":
			if hasAny(args, "-d", "--decode", "-D") && dataRef.MatchString(before) {
				return &leak{what: "base64 -d of a secret's data", safe: base64Safe}
			}
		}
	}
	return nil
}

// kubectlLeak: before is the pipeline before the command; a kubectl get of
// names from stdin or a variable counts as a Secret read when it mentions
// Secrets.
func kubectlLeak(args []string, before string) *leak {
	verb, rest := "", []string{}
	output, template := "", ""
	for i := 0; i < len(args); i++ {
		a := args[i]
		name, val, eq := strings.Cut(a, "=")
		switch {
		case a == "--":
			i = len(args)
		case strings.HasPrefix(a, "-o") && len(a) > 2 && !strings.HasPrefix(a, "--"):
			output = strings.TrimPrefix(a[2:], "=")
		case strings.HasPrefix(a, "-") && kubectlValue[name]:
			if !eq {
				if i+1 < len(args) {
					i++
					val = args[i]
				}
			}
			switch name {
			case "-o", "--output":
				output = val
			case "--template":
				template = val
			}
		case strings.HasPrefix(a, "-"):
		case verb == "":
			verb = a
		default:
			rest = append(rest, a)
		}
	}
	switch verb {
	case "view-secret":
		return &leak{what: "kubectl view-secret", safe: kubectlSafe}
	case verbGet:
	default:
		return nil
	}
	secret, unknown := false, len(rest) == 0
	for _, r := range rest {
		if strings.ContainsAny(r, "${}") {
			unknown = true
		}
		for part := range strings.SplitSeq(r, ",") {
			kind, _, _ := strings.Cut(part, "/")
			if secretRes.MatchString(kind) {
				secret = true
			}
		}
	}
	if !secret && (!unknown || !strings.Contains(strings.ToLower(before), "secret")) {
		return nil
	}
	format, tmpl, _ := strings.Cut(output, "=")
	if tmpl == "" {
		tmpl = template
	}
	switch {
	case format == "yaml" || format == "json":
	case format == "jsonpath" || format == "jsonpath-as-json" || format == "go-template" || format == "template" || format == "custom-columns":
		if !templateLeaks(tmpl) {
			return nil
		}
	case strings.HasSuffix(format, "-file"):
	default:
		return nil
	}
	if !secret {
		return &leak{what: "kubectl get -o " + format + " of objects that may be Secrets", safe: kubectlSafe}
	}
	return &leak{what: "kubectl get secret -o " + format, safe: kubectlSafe}
}

// templateLeaks reports whether a kubectl output template prints values: a
// range over the data prints only keys unless it uses the value variable.
func templateLeaks(tmpl string) bool {
	for _, m := range templateKeys.FindAllStringSubmatch(tmpl, -1) {
		if m[1] != "" && len(regexp.MustCompile(`\$`+m[1]+`\b`).FindAllStringIndex(tmpl, -1)) > 1 {
			return true
		}
	}
	return wholeObject.MatchString(templateKeys.ReplaceAllString(tmpl, "KEYS"))
}

func opLeak(args []string) *leak {
	sub := nonFlags(args)
	if len(sub) == 0 {
		return nil
	}
	out := hasFlag(args, "-o", "--out-file")
	switch {
	case sub[0] == "read" && !out, sub[0] == "inject" && !out:
		return &leak{what: "op " + sub[0], safe: opSafe}
	case len(sub) > 1 && sub[0] == "document" && sub[1] == verbGet && !out:
		return &leak{what: "op document get", safe: opSafe}
	case sub[0] == "run" && hasFlag(args, "--no-masking"):
		return &leak{what: "op run --no-masking", safe: opSafe}
	}
	return nil
}

func vaultLeak(args []string) *leak {
	sub := nonFlags(args)
	switch {
	case len(sub) > 1 && sub[0] == "kv" && sub[1] == verbGet:
		return &leak{what: "vault kv get", safe: vaultSafe}
	case len(sub) > 0 && sub[0] == "read":
		return &leak{what: "vault read", safe: vaultSafe}
	}
	return nil
}

// flowsSafely reports whether the output of the source in segs[i] ends up in
// a file, a variable, a hash or keys only instead of on the terminal.
func flowsSafely(sc shellScan, segs []segment, i int) bool {
	cmd, masked := sc.plain, sc.masked
	for j := i; ; j++ {
		sg := segs[j]
		if j > i {
			words := shellWords(cmd[sg.start:sg.end])
			if safeSink(words) {
				return true
			}
			if len(words) > 0 && path.Base(words[0]) == "tee" && slices.ContainsFunc(words[1:], terminal.MatchString) {
				return false
			}
		}
		if m := redirect.FindAllStringSubmatchIndex(masked[sg.start:sg.end], -1); m != nil {
			for _, r := range m {
				target := strings.Trim(cmd[sg.start+r[2]:sg.start+r[3]], `'"`)
				if !terminal.MatchString(target) {
					return true
				}
			}
		}
		if sg.after != "|" || j+1 == len(segs) {
			break
		}
	}
	first := i
	for first > 0 && segs[first-1].after == "|" {
		first--
	}
	return segs[first].before == "$(" && assignment.MatchString(cmd[segs[first-1].start:segs[first].start-2])
}

// safeSink reports whether a pipeline stage consumes its input without
// printing the values.
func safeSink(words []string) bool {
	k := 0
	for k < len(words) && strings.Contains(words[k], "=") && !strings.HasPrefix(words[k], "-") {
		k++
	}
	if k == len(words) {
		return false
	}
	name, args := path.Base(words[k]), words[k+1:]
	switch {
	case hashes[name], quietSinks[name], hasAny(args, "--password-stdin"):
		return true
	case name == "openssl":
		// a digest, or a certificate's fields without the PEM
		return hasAny(args, "dgst", "sha256", "sha1", "md5") || hasAny(args, "x509") && hasAny(args, "-noout")
	case name == "age-keygen":
		return hasAny(args, "-y")
	case name == "jq" || name == "gojq" || name == "yq":
		return filterSafe(args)
	case name == "grep" || name == "ugrep" || name == "rg":
		for _, a := range args {
			if a == "--quiet" || a == "--count" || a == "--files-with-matches" ||
				strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "--") && strings.ContainsAny(a, "qcl") {
				return true
			}
		}
	case name == kubectlCmd:
		sub := nonFlags(args)
		return len(sub) > 0 && (sub[0] == "apply" || sub[0] == "create" || sub[0] == "replace") && readsStdin(args)
	case name == "sops":
		return hasAny(args, "-e", "--encrypt", "encrypt")
	case name == "gh":
		sub := nonFlags(args)
		return len(sub) > 1 && sub[0] == "secret" && sub[1] == "set"
	}
	return false
}

func readsStdin(args []string) bool {
	for i, a := range args {
		if (a == "-f" || a == "--filename") && i+1 < len(args) && args[i+1] == "-" || a == "-f-" || a == "-f=-" || a == "--filename=-" {
			return true
		}
	}
	return false
}

// filterSafe reports whether a jq or yq invocation prints only keys or metadata.
func filterSafe(args []string) bool {
	filter := ""
	for i := 0; i < len(args) && filter == ""; i++ {
		a := args[i]
		switch {
		case a == "-f" || a == "--from-file":
			return false
		case a == "--arg" || a == "--argjson" || a == "--slurpfile" || a == "--rawfile":
			i += 2
		case a == "--indent" || a == "-o" || a == "--output-format" || a == "-p" || a == "--input-format":
			i++
		case strings.HasPrefix(a, "-"):
		case a == "e" || a == "eval":
		default:
			filter = a
		}
	}
	if filter == "" {
		return false
	}
	rest := keysOnly.ReplaceAllString(filter, " KEYS ")
	return !leakyFilter.MatchString(rest) && harmlessFilter.MatchString(rest)
}

func nonFlags(args []string) []string {
	var out []string
	for _, a := range args {
		if !strings.HasPrefix(a, "-") {
			out = append(out, a)
		}
	}
	return out
}

func hasAny(args []string, want ...string) bool {
	for _, a := range args {
		for _, w := range want {
			if a == w {
				return true
			}
		}
	}
	return false
}

// hasFlag reports whether a flag is given, as a word or as flag=value.
func hasFlag(args []string, flags ...string) bool {
	for _, a := range args {
		name, _, _ := strings.Cut(a, "=")
		for _, f := range flags {
			if name == f {
				return true
			}
		}
	}
	return false
}

// segment is one simple command of a shell command line: its offsets and the
// operators before and after it ("|", "&&", ";", "$(", ")", …).
type segment struct {
	start, end    int
	before, after string
}

// heredoc is a here-document: the offset of its << and its body.
type heredoc struct {
	op, start, end int
}

// shellScan is a command line with the contents of quotes, comments and
// here-documents masked, offsets unchanged, so that operators can be found
// in it; command substitutions inside double quotes stay unmasked.
type shellScan struct {
	masked string
	// plain is the command line with only comments and here-document
	// bodies blanked, for splitting a segment into words.
	plain    string
	heredocs []heredoc
}

func scanShell(s string) shellScan {
	b, p := []byte(s), []byte(s)
	var docs []heredoc
	var pending []struct {
		op    int
		delim string
		tabs  bool
	}
	// frames: 't' top, 's' $( ), '"' double quotes, '`' backticks.
	stack := []byte{'t'}
	for i := 0; i < len(b); i++ {
		top := stack[len(stack)-1]
		c := b[i]
		if top == '"' {
			switch {
			case c == '\\' && i+1 < len(b):
				b[i+1] = '_'
				i++
			case c == '"':
				stack = stack[:len(stack)-1]
			case c == '$' && i+1 < len(b) && b[i+1] == '(':
				stack = append(stack, 's')
				i++
			case c == '`':
				stack = append(stack, '`')
			default:
				b[i] = '_'
			}
			continue
		}
		switch {
		case c == '\\' && i+1 < len(b):
			b[i+1] = '_'
			i++
		case c == '\'':
			j := strings.IndexByte(s[i+1:], '\'')
			if j < 0 {
				j = len(s) - i - 1
			}
			for k := i + 1; k <= i+j; k++ {
				b[k] = '_'
			}
			i += j + 1
		case c == '"':
			stack = append(stack, '"')
		case c == '$' && i+1 < len(b) && b[i+1] == '(':
			stack = append(stack, 's')
			i++
		case c == ')' && top == 's', c == '`' && top == '`':
			stack = stack[:len(stack)-1]
		case c == '`':
			stack = append(stack, '`')
		case c == '#' && (i == 0 || strings.IndexByte(" \t\n;&|(", b[i-1]) >= 0):
			for i < len(b) && b[i] != '\n' {
				b[i], p[i] = '_', ' '
				i++
			}
			i--
		case c == '<' && strings.HasPrefix(s[i:], "<<") && !strings.HasPrefix(s[i:], "<<<"):
			if m := heredocOp.FindStringSubmatch(s[i:]); m != nil {
				pending = append(pending, struct {
					op    int
					delim string
					tabs  bool
				}{i, m[2] + m[3] + m[4], m[1] == "-"})
				for k := i + 2; k < i+len(m[0]); k++ {
					b[k] = '_'
				}
				i += len(m[0]) - 1
			}
		case c == '\n' && len(pending) > 0:
			pos := i + 1
			for _, p := range pending {
				body := pos
				for pos < len(s) {
					end := strings.IndexByte(s[pos:], '\n')
					line := s[pos:]
					if end >= 0 {
						line = s[pos : pos+end]
					}
					if p.tabs {
						line = strings.TrimLeft(line, "\t")
					}
					if line == p.delim {
						docs = append(docs, heredoc{p.op, body, pos})
						if end < 0 {
							pos = len(s)
						} else {
							pos += end
						}
						break
					}
					if end < 0 {
						docs = append(docs, heredoc{p.op, body, len(s)})
						pos = len(s)
						break
					}
					pos += end + 1
				}
			}
			for k := i + 1; k < pos && k < len(b); k++ {
				b[k], p[k] = '_', ' '
			}
			pending = pending[:0]
			i = pos - 1
		}
	}
	return shellScan{string(b), string(p), docs}
}

var heredocOp = regexp.MustCompile(`^<<(-?)\s*(?:'([^']+)'|"([^"]+)"|([\w.-]+))`)

// segments splits the masked command line at its operators.
func (sc shellScan) segments() []segment {
	m := sc.masked
	var segs []segment
	start, before := 0, ""
	cut := func(end, next int, op string) {
		segs = append(segs, segment{start, end, before, op})
		start, before = next, op
	}
	for i := 0; i < len(m); i++ {
		c := m[i]
		switch {
		case c == '|' && i+1 < len(m) && m[i+1] == '|', c == '&' && i+1 < len(m) && m[i+1] == '&':
			cut(i, i+2, m[i:i+2])
			i++
		case c == '|' && i+1 < len(m) && m[i+1] == '&':
			cut(i, i+2, "|")
			i++
		case c == '|' && i > 0 && m[i-1] == '>':
		case c == '|':
			cut(i, i+1, "|")
		case c == '&' && (i > 0 && (m[i-1] == '>' || m[i-1] == '<') || i+1 < len(m) && m[i+1] == '>'):
		case c == '&', c == ';', c == '\n', c == '`':
			cut(i, i+1, string(c))
		case c == '$' && i+1 < len(m) && m[i+1] == '(':
			cut(i, i+2, "$(")
			i++
		case c == '(' || c == ')':
			if c == '(' && i > 0 && m[i-1] == '<' || c == '(' && i > 0 && m[i-1] == '>' {
				cut(i-1, i+1, "<(")
				continue
			}
			cut(i, i+1, string(c))
		}
	}
	return append(segs, segment{start, len(m), before, ""})
}

// shellWords splits a simple command into its words with the quotes removed.
func shellWords(s string) []string {
	var words []string
	var w strings.Builder
	in := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '\'':
			j := strings.IndexByte(s[i+1:], '\'')
			if j < 0 {
				j = len(s) - i - 1
			}
			w.WriteString(s[i+1 : i+1+j])
			i += j + 1
			in = true
		case c == '"':
			i++
			for ; i < len(s) && s[i] != '"'; i++ {
				if s[i] == '\\' && i+1 < len(s) && strings.IndexByte(`"\$`+"`", s[i+1]) >= 0 {
					i++
				}
				w.WriteByte(s[i])
			}
			in = true
		case c == '\\' && i+1 < len(s):
			i++
			w.WriteByte(s[i])
			in = true
		case c == ' ' || c == '\t' || c == '\n':
			if in {
				words = append(words, w.String())
				w.Reset()
				in = false
			}
		default:
			w.WriteByte(c)
			in = true
		}
	}
	if in {
		words = append(words, w.String())
	}
	return words
}
