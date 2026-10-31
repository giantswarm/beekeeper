package guard

import (
	"bytes"
	"cmp"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// The Secret guard refuses a Bash command that would put secret values where
// an agent can read them: the terminal (and so the transcript and the model
// API), a file, a variable, the clipboard, a hash or a diff. For credential
// tools it is an allow list: a read passes only when its output keeps key
// names, metadata or lengths, or goes to a consumer that prints nothing of
// it. sops, op and decryption run only in beekeeper (beekeeper secret), never
// in an agent session, and no agent session signs in to or unlocks a vault:
// the vault session lives in beekeeper's broker alone. Every other shape that
// touches a secret is refused: false positives beat leaks.

const (
	kubectlCmd   = "kubectl"
	sopsCmd      = "sops"
	helmCmd      = "helm"
	evalCmd      = "eval"
	selfCmd      = "beekeeper"
	findCmd      = "find"
	verbGet      = "get"
	verbSet      = "set"
	verbCreate   = "create"
	verbEdit     = "edit"
	verbList     = "list"
	verbClose    = "close"
	verbComment  = "comment"
	verbUnlock   = "unlock"
	verbDelete   = "delete"
	gitCmd       = "git"
	flagRepo     = "--repo"
	flagBodyFile = "--body-file"
	flagJSON     = "--json"
	flagJQ       = "--jq"
	flagField    = "--field"
	flagRawField = "--raw-field"
	flagOutput   = "--output"
	flagInput    = "--input"
	flagTemplate = "--template"
)

// A leak names the command that would expose secret values and the safe forms.
type leak struct {
	what, safe string
	// at is the command of the line that exposes them.
	at string
	// never: no sink makes the command safe (sops, op, a hash of a secret file).
	never bool
	// render: a render fed with secret values, which a filter that blanks
	// its Secret data makes safe.
	render bool
	// unlock: a sign-in to or an unlock of a vault, which no agent runs.
	unlock bool
	// person: the person's own store into the vault, whose value (the
	// clipboard, a download) no agent handles.
	person bool
	// file: a read of a file that holds secret values, which a jq or yq
	// filter that strips its credential keys makes safe.
	file string
}

// secretOps names the beekeeper secret operations every refusal points to.
const secretOps = "Equality, the one comparison an agent runs: `beekeeper secret compare <a> <b>` or `beekeeper secret fingerprint <ref>`, " +
	"whose broker compares out of the session, no value reaching it; any other comparison of a value (a hash, a diff, `grep -c|-q`) stays refused. " +
	"Changes: `beekeeper secret copy <src.sops.yaml> <dst.sops.yaml> [--name n --namespace ns]`, " +
	"`copy <ref> <file#path>`, `copy <ref> -- <consumer>`, `copy <ref> ssh://<host>/<path> [--mode 0600] [--owner user:group] [--sudo] [--template …]` (a file on a host, the value on ssh's stdin), `set <file> <path> --generate [--name n --namespace ns] [--vault op://…] [--to-secret … | -- <consumer>]`, " +
	"`unset <file> <path>… --write` (keys out of a SOPS file); configuration a SOPS file encrypts (ids, URLs, peers): `reveal <file> <path>…`; " +
	"a consumer reads the value on stdin, in a pod through `kubectl exec -i --context <context> <pod> -- <consumer>` (`--stdin-json`/`--stdin-field` wrap it in a JSON request). " +
	"Rotations: `beekeeper secret rotate op://… --generate` (a value beekeeper made), `rotate op://…` (a value its issuer " +
	"rotated into the vault), `rotate platform://<installation>/<capability>/<name> --reason …` (a platform manager credential)."

func (l leak) reason() string {
	at := strings.Join(strings.Fields(l.at), " ")
	if len(at) > 200 {
		at = at[:200] + "…"
	}
	if l.person {
		return "Refused: `" + at + "` (" + l.what + ") is the person's, run in their own terminal: the value it takes in (the " +
			"clipboard, a downloaded file) never passes through an agent session. Ask the person for it (a one-line question to the " +
			"supervisor: `beekeeper secret store op://<vault>/<item>/<field>` or `--github-app <item>`), then run the operations " +
			"on the stored field yourself:\n" + secretOps
	}
	if l.unlock {
		return "Refused: `" + at + "` (" + l.what + ") signs in to or unlocks a vault from an agent session. No agent session holds " +
			"a vault session or opens one: it lives in beekeeper's broker alone, which signs in by itself. A `beekeeper secret` call " +
			"that needs the vault waits for that sign-in (`vault locked: waiting for the broker's sign-in`), so run the operation " +
			"itself:\n" + secretOps
	}
	return "Refused: `" + at + "` (" + l.what + ") would put secret values where an agent can read them: the transcript " +
		"and from there the model API, a file, a variable, a hash or a diff. Only key names, metadata and lengths reach an agent, " +
		"and a value goes only to a consumer that prints nothing of it:\n" + l.safe + "\n" + secretOps
}

var (
	kubectlSafe = "  kubectl get secret <name> -o json | jq '.data|keys'\n" +
		"  kubectl get secrets -o name\n" +
		"  kubectl describe secret <name>   (sizes only)\n" +
		"  kubectl get secret <name> -o jsonpath='{.data.<key>}' | base64 -d | wc -c   (the length)\n" +
		"  beekeeper secret copy k8s://<context>/<namespace>/<name>/<key>[#<path>] <file.sops.yaml#path>   (the value into a SOPS path, in beekeeper)\n" +
		"  beekeeper secret copy k8s://<context>/<namespace>/<name>/<key>[#<path>] ssh://<host>/<path> [--mode 0600] [--owner user:group] [--sudo]   (the value into a file on a host, on ssh's stdin)"
	sopsSafe = "  sops runs only in beekeeper, never in an agent session, encryption included.\n" +
		"  yq '.stringData|keys' <file>   (a SOPS file keeps its key names in plaintext)"
	opSafe      = "  op runs only in beekeeper, never in an agent session (op run included): beekeeper secret reads the shared vault."
	keyringSafe = "  The keyring's entries are the person's: no agent session reads them."
	cryptSafe   = "  Decryption runs only in beekeeper, never in an agent session."
	vaultSafe   = "  vault kv get -format=json <path> | jq '.data.data|keys'\n  vault kv metadata get <path>\n  vault kv list <path>"
	base64Safe  = "  … | base64 -d | wc -c   (the length)"
	renderSafe  = "  … | yq 'del(.data, .stringData)'   (the render with its Secret data blanked)\n" +
		"  diff <(… | yq 'del(.data, .stringData)') <(… | yq 'del(.data, .stringData)')"
	fileSafe = "  yq '.data|keys' <file>   (the key names)"
)

var (
	// redirect: stdout to a file; group 1 is the target.
	redirect  = regexp.MustCompile(`(?:^|[^\d<>&])(?:1|&)?>>?\|?\s*([^\s;&|()<>]+)`)
	terminal  = regexp.MustCompile(`^/(?:dev/(?:stdout|stderr|tty|fd/[12])|proc/self/fd/[12])$`)
	secretRes = regexp.MustCompile(`(?i)^secrets?(?:\.v1)?\.?$`)
	dataRef   = regexp.MustCompile(`\.(?:data|stringData)\b`)
	// wholeObject: a template that prints an object with its data.
	wholeObject = regexp.MustCompile(`(?i)\b(?:string)?data\b|\{\s*[@.]\s*\}|\.\.|\{\s*\.items\s*(?:\[[^\]]*\])?\s*\}|\{\{-?\s*(?:json|toJson|toYaml|toPrettyJson)?\s*\.\s*-?\}\}|:=\s*\.\s*-?\}\}`)
	// fieldPath: a field path in a template; harmlessPath: one that names
	// only an object's kind, type or metadata other than its annotations,
	// which can carry the whole object (last-applied-configuration).
	fieldPath    = regexp.MustCompile(`(?:\.[A-Za-z_][\w-]*(?:\\\.[\w-]+)*(?:\[[^\]]*\])*)+`)
	harmlessPath = regexp.MustCompile(`^(?:\.items(?:\[[^\]]*\])*)?(?:\.(?:kind|type|apiVersion|immutable)|\.metadata\.(?:name|namespace|labels|uid|creationTimestamp|resourceVersion|generation|ownerReferences|finalizers|deletionTimestamp)\b.*)?$`)
	// keysOnly: a jq or yq filter part that keeps only the keys of the data.
	keysOnly = regexp.MustCompile(`(?:\.\w+|\[[^\]]*\])*\.(?:data|stringData)(?:\s*//\s*\{\}\s*\))?\s*\|\s*(?:keys_unsorted|keys|length)\b(?:\[\])?|\.value\s*\|\s*length\b`)
	// templateKeys: a template range over the data that binds the value to
	// group 1, or the data's length.
	templateKeys = regexp.MustCompile(`range\s+\$\w+\s*,\s*\$(\w+)\s*:=\s*\.(?:data|stringData)\b|len\s+\.(?:data|stringData)\b`)
	// leakyFilter: what prints values once the keys-only parts are gone:
	// the whole metadata and its annotations, a deletion or an assignment,
	// which print the rest of the object.
	leakyFilter = regexp.MustCompile(`(?i)\b(?:string)?data\b|\bdel\(|(?:^|[^=!<>])=(?:[^=]|$)|\bvalues?\b|\.\.|to_entries|with_entries|@base64d|\$ENV|\benv\b|\binputs?\b|tostream|paths|getpath|\bannotations\b|\.metadata\s*(?:$|[|,})\]])|(?:^|[\s,:(|])\.(?:\s*(?:[,})\]|]|$)|\[\s*(?:\]|"))`)
	// blankData: a filter part that blanks the data or stringData of the
	// objects it passes; group 1 names which.
	blankData = regexp.MustCompile(`\.(data|stringData)\s*\|?=\s*(?:keys_unsorted|keys|\{\}|null|""|map_values\(\s*""\s*\))`)
	dataField = regexp.MustCompile(`\.(data|stringData)\b`)
	// kubectlFunc, kubectlVar: a shell function or a variable that runs kubectl.
	kubectlFunc = regexp.MustCompile(`(?:^|[\s;&|(])(?:function\s+([\w-]+)\s*(?:\(\))?|([\w-]+)\s*\(\))\s*\{[^}]*?(?:^|[\s/])kubectl\s`)
	kubectlVar  = regexp.MustCompile(`(?:^|[\s;&|(])(\w+)=\(?\s*["']?(?:[^\s"'()]*/)?kubectl(?:[\s"');&|]|$)`)
	// harmlessFilter: a filter that names only keys or metadata.
	harmlessFilter = regexp.MustCompile(`\b(?:KEYS|keys|keys_unsorted|length)\b|\.(?:metadata|kind|type|apiVersion|name|namespace|label|id|title)\b`)
	// codeFile: a file name that holds code or prose, not a secret's values.
	codeFile = regexp.MustCompile(`(?i)\.(?:go|md|ts|tsx|js|mjs|py|sh|rs|java|kt|rb|tf|html|css|tpl|snap|golden|mod|sum)$`)
	dataWord = regexp.MustCompile(`(?i)\b(?:string)?data\b`)
	shells   = map[string]bool{"sh": true, "bash": true, "zsh": true, "ksh": true, "dash": true, "ssh": true, evalCmd: true, watchCmd: true}
	hashCmds = map[string]bool{"sha256sum": true, "sha1sum": true, "sha224sum": true, "sha384sum": true, "sha512sum": true, "md5sum": true, "b2sum": true, "cksum": true, "shasum": true}
	diffCmds = map[string]bool{"diff": true, "cmp": true, "colordiff": true, "sdiff": true, "diff3": true, "vimdiff": true, "delta": true, "difft": true}
	// wrapperValues: the options of wrappers that take a value.
	wrapperValues = map[string]bool{"-u": true, "-g": true, "-n": true, "-s": true, "-k": true, "-C": true, "-P": true, "-L": true, "-I": true, "-E": true, "-d": true, "-a": true, userFlag: true, groupFlag: true, "--signal": true, "--kill-after": true, chdirFlag: true, "--unset": true}
)

// kubectl flags that take a value, so that the value is not taken for the
// verb or a resource.
var kubectlValue = map[string]bool{
	"-n": true, namespaceFlag: true, "--context": true, "--kubeconfig": true, "--cluster": true, userFlag: true,
	"-s": true, "--server": true, "--as": true, "--as-group": true, "--token": true, "--request-timeout": true,
	"-l": true, "--selector": true, "-o": true, flagOutput: true, "--field-selector": true, flagTemplate: true,
	"-f": true, "--filename": true, "-c": true, "--container": true, "--sort-by": true, "--chunk-size": true, "--label-columns": true, "-L": true,
	"--raw": true,
}

// secretGuard is the Secret guard of one call: the commands that unlock the
// person's vault besides op's own, and the directory a script it runs is
// read from.
type secretGuard struct {
	// unlock are the names of commands that sign in to or unlock a vault:
	// the person's own helpers around op signin (secret.unlockCommands).
	unlock []string
	// cwd is the session's working directory; empty, no script is read.
	cwd string
	// files are the files known to hold secret values (secretfiles.go).
	files secretFiles
	// metadata holds helpers whose complete bodies have been checked. An empty
	// entry shadows a utility and must not inherit its metadata exemption.
	metadata         map[string]string
	linkAllowance    linkAllowance
	noLinks          bool
	standaloneScript string
}

// maxScript bounds what the guard reads of a script a command runs.
const maxScript = 64 << 10

// leak returns the leak cmd would expose, nil when it exposes none.
func (g secretGuard) leak(cmd string) *leak {
	return g.scan(cmd, 0)
}

func (g secretGuard) scan(cmd string, depth int) *leak {
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
	// dirs: where a relative path may point, the working directory and
	// every directory the command line changes to.
	dirs := []string{g.cwd}
	for _, m := range cdArg.FindAllStringSubmatch(sc.plain, -1) {
		dirs = append(dirs, within(g.cwd, m[1]))
	}
	vars := scanVars(sc, segs)
	g.metadata = metadataFunctions(cmd)
	g.linkAllowance = simpleLinkAllowance(cmd, g.metadata)
	g.standaloneScript = standaloneLinkScript(cmd)
	copied, copyLeak := g.copiedUnlocks(sc, segs, vars, depth)
	if copyLeak != nil {
		return copyLeak
	}
	for i, sg := range segs {
		words := shellWords(sc.plain[sg.start:sg.end])
		for _, exp := range expandWordsList(words, vars) {
			exp = g.linkAllowance.resolve(exp)
			if l := g.nested(cmd, sc, segs, i, exp, copied, depth); l != nil {
				return l
			}
			l := g.sourceLeak(exp, pipelineBefore(sc.plain, segs, i), aliases, dirs, copied)
			if l == nil || flowsSafely(sc, segs, i, l) {
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
	}
	return nil
}

// scanVars collects literal variable bindings and for-loop variable items on
// the command line so a guarded tool run through $X or a loop is seen.
func scanVars(sc shellScan, segs []segment) map[string][]string {
	out := map[string][]string{}
	for _, m := range assignment.FindAllStringSubmatch(sc.plain, -1) {
		out[m[1]] = append(out[m[1]], strings.Trim(m[2], `'"`))
	}
	for _, sg := range segs {
		w := shellWords(sc.plain[sg.start:sg.end])
		for len(w) > 0 && (keywords[w[0]] && w[0] != forWord) {
			w = w[1:]
		}
		if len(w) >= 4 && w[0] == forWord && w[2] == "in" {
			out[w[1]] = append(out[w[1]], w[3:]...)
		}
	}
	return out
}

// expandWordsList returns words and, when a word references a bound variable,
// the words with each value substituted in.
func expandWordsList(words []string, vars map[string][]string) [][]string {
	out := [][]string{words}
	if len(vars) == 0 {
		return out
	}
	for i, w := range words {
		if !strings.Contains(w, "$") {
			continue
		}
		for name, vals := range vars {
			for _, ref := range []string{"${" + name + "}", "$" + name} {
				if !strings.Contains(w, ref) {
					continue
				}
				for _, v := range vals {
					cp := slices.Clone(words)
					cp[i] = strings.ReplaceAll(w, ref, v)
					out = append(out, cp)
				}
			}
		}
	}
	return out
}

// copiedUnlocks maps a destination path created on the command line from an
// unlock helper (by cp, mv, ln, install, dd, tee or >) to that helper's name,
// and returns a leak when a segment copies, moves, links or redirects an unlock
// helper so no alias can be staged across tool calls.
func (g secretGuard) copiedUnlocks(sc shellScan, segs []segment, vars map[string][]string, depth int) (map[string]string, *leak) {
	if len(g.unlock) == 0 {
		return nil, nil
	}
	copied := map[string]string{}
	var copyLeak *leak
	isUnlock := func(p string) string {
		b := path.Base(p)
		if slices.Contains(g.unlock, b) {
			return b
		}
		if orig, ok := copied[p]; ok {
			return orig
		}
		if l := g.script(p, depth); l != nil && l.unlock {
			return b
		}
		return ""
	}
	for segIdx, sg := range segs {
		first := segIdx
		for first > 0 && segs[first-1].after == "|" {
			first--
		}
		for _, words := range expandWordsList(shellWords(sc.plain[sg.start:sg.end]), vars) {
			c := commandAt(words)
			if c >= len(words) {
				continue
			}
			var src, srcStageCmd string
			var srcStageArgs []string
			for j := first; j <= segIdx && src == ""; j++ {
				stageWords := words
				if j < segIdx {
					stageWords = shellWords(sc.plain[segs[j].start:segs[j].end])
				}
				scIdx := commandAt(stageWords)
				if scIdx >= len(stageWords) {
					continue
				}
				stageArgs := stageWords[scIdx:]
				for idx, w := range stageArgs {
					if idx == 0 && !strings.HasPrefix(w, "<") && !strings.Contains(w, "/") {
						continue
					}
					if idx > 0 && (stageArgs[idx-1] == "-g" || stageArgs[idx-1] == "--glob" || stageArgs[idx-1] == "--exclude" || stageArgs[idx-1] == "--ignore-file") {
						continue
					}
					w = strings.TrimPrefix(w, "if=")
					w = strings.TrimLeft(w, "<")
					if strings.HasPrefix(w, "!") || strings.HasPrefix(w, "--glob=") || strings.HasPrefix(w, "--exclude=") {
						continue
					}
					if u := isUnlock(w); u != "" {
						src = u
						srcStageCmd = path.Base(stageArgs[0])
						srcStageArgs = stageArgs[1:]
						break
					}
				}
			}
			if src == "" {
				continue
			}
			staged := false
			name := path.Base(words[c])
			readSearchStage := isReadOnlyUnlockSearch(srcStageCmd, srcStageArgs)
			switch name {
			case "cp", "mv", "ln", "install":
				staged = true
				if non := nonFlags(words[c+1:]); len(non) >= 2 {
					dst := non[len(non)-1]
					copied[dst] = src
					copied[path.Base(dst)] = src
				}
			case "dd":
				for _, w := range words[c+1:] {
					if dst, ok := strings.CutPrefix(w, "of="); ok {
						staged = true
						copied[dst] = src
						copied[path.Base(dst)] = src
					}
				}
			case "s" + "ed", gsedCmd, perlCmd:
				if hasInPlaceFlag(words[c+1:]) {
					staged = true
				}
			case teeCmd:
				if !readSearchStage {
					for _, dst := range nonFlags(words[c+1:]) {
						if !strings.Contains(dst, "<") {
							staged = true
							copied[dst] = src
							copied[path.Base(dst)] = src
						}
					}
				}
			}
			if !readSearchStage {
				if m := redirect.FindStringSubmatch(sc.plain[sg.start:sg.end]); m != nil && !terminal.MatchString(m[1]) && m[1] != "/dev/null" {
					staged = true
					copied[m[1]] = src
					copied[path.Base(m[1])] = src
				}
			}
			if staged && copyLeak == nil {
				copyLeak = &leak{
					what:   src + ", a vault unlock",
					at:     sc.plain[segs[first].start:sg.end],
					never:  true,
					unlock: true,
				}
			}
		}
	}
	return copied, copyLeak
}

// nested returns the leak of what the simple command runs besides itself:
// the command strings and scripts a shell or interpreter runs, its input
// redirections, here-strings and here-documents, git -c aliases, eval's
// words, a sourced script and a script run by its path.
func (g secretGuard) nested(cmd string, sc shellScan, segs []segment, segIdx int, words []string, copied map[string]string, depth int) *leak {
	g.noLinks = true
	if depth == 0 && g.standaloneScript != "" {
		g.noLinks = false
		return g.unlockOrScript(wordPath(g.standaloneScript, g.cwd), copied, depth)
	}
	if envCmd := envSplitCommand(words); envCmd != "" {
		if l := g.scan(envCmd, depth+1); l != nil {
			return l
		}
	}
	if flockCmd := flockCommand(words); flockCmd != "" {
		if l := g.scan(flockCmd, depth+1); l != nil {
			return l
		}
	}
	c := commandAt(words)
	run := c
	for c < len(words) && redirection.MatchString(words[c]) {
		if bareRedirect.MatchString(words[c]) {
			c++
		}
		c++
	}
	if c < len(words) {
		name, args := path.Base(words[c]), words[c+1:]
		if shellSyntaxOnly(name, args) {
			// The caller's substitutions remain separate scan segments, and
			// sourceLeak still checks files supplied directly to this command.
			return nil
		}
		switch {
		case name == evalCmd:
			if l := g.scan(strings.Join(args, " "), depth+1); l != nil {
				return l
			}
			return g.inputLeak(cmd, sc, segs, segIdx, words[run:], copied, depth)
		case (name == sourceCmd || name == ".") && len(args) > 0:
			return g.unlockOrScript(args[0], copied, depth)
		case name == gitCmd:
			for _, aliasCmd := range gitAliasCommands(args) {
				if l := g.scan(aliasCmd, depth+1); l != nil {
					return l
				}
			}
			if l := g.gitConfigLeak(args, copied, depth); l != nil {
				return l
			}
		case name == "sed" || name == "gsed":
			if l := g.sedExecLeak(args, copied, depth); l != nil {
				return l
			}
		case name == "awk" || name == "gawk" || name == "mawk" || name == "nawk":
			for i := 0; i < len(args); i++ {
				if args[i] == "-f" && i+1 < len(args) {
					if l := g.unlockOrScript(args[i+1], copied, depth); l != nil {
						return l
					}
					i++
				} else if f, ok := strings.CutPrefix(args[i], "-f"); ok && f != "" && !strings.HasPrefix(args[i], "--") {
					if l := g.unlockOrScript(f, copied, depth); l != nil {
						return l
					}
				}
			}
		case interpreters[name].code != nil || len(interpreters[name].file) > 0:
			ip := interpreters[name]
			switch p := readProgram(args, ip); p.kind {
			case programCode:
				if l := g.scan(p.arg, depth+1); l != nil {
					return l
				}
			case programScript:
				if l := g.unlockOrScript(p.arg, copied, depth); l != nil {
					return l
				}
			}
			if l := g.inputLeak(cmd, sc, segs, segIdx, words[run:], copied, depth); l != nil {
				return l
			}
		case strings.Contains(words[c], "/") && !shells[name]:
			if l := g.script(words[c], depth); l != nil {
				return l
			}
		}
	}
	for k, w := range words {
		name := path.Base(w)
		if w == "$SHELL" || w == "${SHELL}" {
			name = "sh"
		}
		if !shells[name] {
			continue
		}
		// Every argument is scanned as a command line of its own: a -c
		// string, ssh's remote command, watch's; one word (zsh -ic f) too.
		script := name != "ssh" && name != watchCmd && name != evalCmd && !slices.ContainsFunc(words[k+1:], isCFlag)
		args := words[k+1:]
		for j := 0; j < len(args); j++ {
			arg := args[j]
			if strings.HasPrefix(arg, "-") || strings.HasPrefix(arg, "+") {
				if isCFlag(arg) {
					script = false
				}
				if script && (arg == "-o" || arg == "+o") && j+1 < len(args) {
					j++ // shell option name, not the script operand
				}
				continue
			}
			if bareRedirect.MatchString(arg) || redirection.MatchString(arg) {
				continue
			}
			if l := g.scan(arg, depth+1); l != nil {
				return l
			}
			if script {
				if l := g.unlockOrScript(arg, copied, depth); l != nil {
					return l
				}
				script = false
			}
		}
		if l := g.inputLeak(cmd, sc, segs, segIdx, words[k:], copied, depth); l != nil {
			return l
		}
		break
	}
	return nil
}

// inputLeak checks the stdin fed to a shell or an interpreter: a redirected
// file (< path), a here-string (<<< word), a here-document on the segment or
// piped into it, or a pipeline stage before it.
func (g secretGuard) inputLeak(cmd string, sc shellScan, segs []segment, segIdx int, words []string, copied map[string]string, depth int) *leak {
	sg := segs[segIdx]
	for i := 0; i < len(words); i++ {
		w := words[i]
		switch {
		case w == "<<<" && i+1 < len(words):
			if l := g.scan(words[i+1], depth+1); l != nil {
				return l
			}
			i++
		case strings.HasPrefix(w, "<<<") && len(w) > 3:
			if l := g.scan(w[3:], depth+1); l != nil {
				return l
			}
		case (w == "<" || w == "0<") && i+1 < len(words):
			if l := g.unlockOrScript(words[i+1], copied, depth); l != nil {
				return l
			}
			i++
		case strings.HasPrefix(w, "<") && !strings.HasPrefix(w, "<<") && !strings.HasPrefix(w, "<&") && len(w) > 1:
			if l := g.unlockOrScript(w[1:], copied, depth); l != nil {
				return l
			}
		case strings.HasPrefix(w, "0<") && !strings.HasPrefix(w, "0<&") && len(w) > 2:
			if l := g.unlockOrScript(w[2:], copied, depth); l != nil {
				return l
			}
		}
	}
	first := segIdx
	for first > 0 && segs[first-1].after == "|" {
		first--
	}
	for _, h := range sc.heredocs {
		if h.op >= segs[first].start && h.op < sg.end {
			if l := g.scan(cmd[h.start:h.end], depth+1); l != nil {
				return l
			}
		}
	}
	if first < segIdx {
		for j := first; j < segIdx; j++ {
			if l := g.stageInputLeak(sc, segs[j], copied, depth); l != nil {
				return l
			}
		}
	}
	for j := segIdx + 1; j < len(segs); j++ {
		prevAfter := segs[j-1].after
		if prevAfter != "$(" && prevAfter != "`" && segs[j].before != "$(" && segs[j].before != "`" {
			break
		}
		if l := g.stageInputLeak(sc, segs[j], copied, depth); l != nil {
			return l
		}
	}
	return nil
}

func (g secretGuard) stageInputLeak(sc shellScan, sg segment, copied map[string]string, depth int) *leak {
	stageWords := shellWords(sc.plain[sg.start:sg.end])
	scIdx := commandAt(stageWords)
	if scIdx >= len(stageWords) {
		return nil
	}
	stageName := path.Base(stageWords[scIdx])
	if stageName == echoCommand || stageName == printfCommand {
		if l := g.scan(strings.Join(stageWords[scIdx+1:], " "), depth+1); l != nil {
			return l
		}
	}
	for _, sw := range stageWords[scIdx+1:] {
		if strings.HasPrefix(sw, "-") {
			continue
		}
		if l := g.unlockOrScript(sw, copied, depth); l != nil {
			return l
		}
		if stageName == gitCmd {
			if _, rest, ok := strings.Cut(sw, ":"); ok && rest != "" {
				if l := g.unlockOrScript(rest, copied, depth); l != nil {
					return l
				}
			}
		}
	}
	return nil
}

// unlockOrScript returns a leak when p names an unlock helper (or a copy of
// one made on the command line), else scans p as a script file.
func (g secretGuard) unlockOrScript(p string, copied map[string]string, depth int) *leak {
	b := path.Base(p)
	if slices.Contains(g.unlock, b) {
		return &leak{what: b + ", a vault unlock", never: true, unlock: true}
	}
	if orig, ok := copied[p]; ok {
		return &leak{what: orig + ", a vault unlock", never: true, unlock: true}
	}
	if orig, ok := copied[b]; ok {
		return &leak{what: orig + ", a vault unlock", never: true, unlock: true}
	}
	return g.script(p, depth)
}

// script returns the leak of the script at p, a shell script a command line
// runs: a regular text file of at most maxScript bytes, relative to the
// session's working directory. Anything else is no script and passes.
// When a guarded unlock tool is a script and a preexisting symlink to it on
// disk is a read-only command of the same file (selected by argv[0]), the
// symlink whose own name is not in g.unlock passes.
func (g secretGuard) script(p string, depth int) *leak {
	if g.cwd == "" || p == "" {
		return nil
	}
	if rest, ok := strings.CutPrefix(p, "~/"); ok {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil
		}
		p = filepath.Join(home, rest)
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(g.cwd, p)
	}
	if lfi, err := os.Lstat(p); err == nil && lfi.Mode()&os.ModeSymlink != 0 && !slices.Contains(g.unlock, filepath.Base(p)) {
		if target, err := filepath.EvalSymlinks(p); err == nil && slices.Contains(g.unlock, filepath.Base(target)) {
			return nil
		}
	}
	fi, err := os.Stat(p)
	if err != nil || !fi.Mode().IsRegular() || fi.Size() > maxScript {
		return nil
	}
	raw, err := os.ReadFile(p) //nolint:gosec // a script the command line runs, read to scan it
	if err != nil || bytes.IndexByte(raw, 0) >= 0 {
		return nil
	}
	if l := g.scan(string(raw), depth+1); l != nil {
		l.what += ", run from " + filepath.Base(p)
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

// sourceLeak returns the leak when the simple command exposes secret values.
func (g secretGuard) sourceLeak(words []string, before string, aliases map[string]bool, dirs []string, copied map[string]string) *leak {
	if l := procFileLeak(words); l != nil {
		return l
	}
	if l := g.fileLeak(words, dirs); l != nil {
		return l
	}
	if l := g.toolLeak(words, copied); l != nil {
		return l
	}
	if l := g.installedUnlockLeak(words, dirs); l != nil {
		return l
	}
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
			return &leak{what: "kubectl view-secret", safe: kubectlSafe, never: true}
		case "ps", "pgrep", "pstree", dockerCmd, podmanCmd:
			if !processLister(words, k) {
				continue
			}
			if l := procLeak(name, args); l != nil {
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

// unmodeledWrappers are prefix utilities whose option grammars are not modeled
// in full; when a command line wraps a guarded tool in one, it fails closed.
var unmodeledWrappers = map[string]bool{
	"strace": true, "ltrace": true, "gdb": true, "lldb": true, "perf": true, "valgrind": true,
	"bwrap": true, "firejail": true, "nsenter": true, "unshare": true, "chroot": true, "fakeroot": true,
	"proot": true, "su": true, "runuser": true, "doas": true, "pkexec": true,
	"systemd-run": true, "systemd-inhibit": true, "at": true, "batch": true,
	"parallel": true, "busybox": true, "toybox": true,
	"expect": true, "socat": true, "script": true, "screen": true, "tmux": true,
}

// commandAt returns the index of the word the simple command runs, past its
// assignments, wrappers (sudo, env, timeout, nice, stdbuf, ionice, chrt,
// flock, taskset, xargs, beekeeper run, …), the shell's keywords and a
// function definition's head.
func commandAt(words []string) int {
	rest, _ := unwrapWords(words)
	return len(words) - len(rest)
}

// unwrapWords strips leading assignments, keywords, redirections and modeled
// wrapper commands from words, returning the remaining words starting at the
// inner command verb. If a wrapper cannot be safely unwrapped (an unmodeled
// wrapper or an unrecognized option), unsure names the wrapper.
func unwrapWords(words []string) (rest []string, unsure string) {
	markUnsure := func(s string) {
		if unsure == "" {
			unsure = s
		}
	}
	i := 0
	for i < len(words) {
		w := words[i]
		name := path.Base(w)
		switch {
		case envWord.MatchString(w), w == "--", keywords[w]:
			i++
		case w == "function":
			i += 2
		case redirection.MatchString(w):
			if bareRedirect.MatchString(w) {
				i += 2
			} else {
				i++
			}
		case name == selfCmd && i+1 < len(words) && words[i+1] == "run":
			i += 2
		case name == "nohup" || name == "setsid" || name == "time":
			i++
			for i < len(words) && strings.HasPrefix(words[i], "-") {
				if words[i] == "--" {
					i++
					break
				}
				i++
			}
		case name == "command" || name == "builtin" || name == execCmd:
			i++
			for i < len(words) && strings.HasPrefix(words[i], "-") {
				o := words[i]
				if o == "--" {
					i++
					break
				}
				if o == "-a" && name == execCmd {
					i += 2
					continue
				}
				if strings.HasPrefix(o, "-a") && name == execCmd {
					i++
					continue
				}
				flags := strings.TrimPrefix(o, "-")
				if flags != "" && strings.Trim(flags, "pvVcl") == "" {
					i++
					continue
				}
				markUnsure(name + " " + o)
				i++
			}
		case name == envCmdName:
			i++
			for i < len(words) {
				o := words[i]
				switch {
				case o == "--":
					i++
				case envWord.MatchString(o):
					i++
					continue
				case o == "-i" || o == "-" || o == "-0" || o == "--ignore-environment" || o == "--null" || o == "-v" || o == "--debug":
					i++
					continue
				case o == "-u" || o == "--unset" || o == "-C" || o == chdirFlag || o == "-P" || o == "--default-signal" || o == "--ignore-signal" || o == "--block-signal":
					i += 2
					continue
				case strings.HasPrefix(o, "-u") || strings.HasPrefix(o, "-C") || strings.HasPrefix(o, "-P") ||
					strings.HasPrefix(o, "--unset=") || strings.HasPrefix(o, "--chdir="):
					i++
					continue
				case o == "-S" || o == "--split-string" || strings.HasPrefix(o, "-S") || strings.HasPrefix(o, "--split-string="):
					return words[i:], cmp.Or(unsure, envCmdName)
				case strings.HasPrefix(o, "-") && !strings.HasPrefix(o, "--"):
					flags := o[1:]
					for len(flags) > 0 && (flags[0] == 'i' || flags[0] == '0' || flags[0] == 'v') {
						flags = flags[1:]
					}
					if flags == "" {
						i++
						continue
					}
					if flags == "u" || flags == "C" || flags == "P" {
						i += 2
						continue
					}
					if flags[0] == 'u' || flags[0] == 'C' || flags[0] == 'P' {
						i++
						continue
					}
					markUnsure("env " + o)
					i++
					continue
				case strings.HasPrefix(o, "-"):
					markUnsure("env " + o)
					i++
					continue
				}
				break
			}
		case name == "sudo":
			i++
			for i < len(words) && strings.HasPrefix(words[i], "-") {
				o := words[i]
				if o == "--" {
					i++
					break
				}
				if o == "-u" || o == "-g" || o == "-C" || o == "-D" || o == "-R" || o == "-T" || o == "-U" || o == "-h" || o == "-p" || o == "-r" || o == "-t" ||
					o == userFlag || o == groupFlag || o == chdirFlag || o == "--chroot" || o == "--close-from" || o == "--host" || o == "--prompt" || o == "--role" || o == "--type" || o == "--other-user" || o == "--command-timeout" {
					i += 2
					continue
				}
				if strings.HasPrefix(o, "--user=") || strings.HasPrefix(o, "--group=") || strings.HasPrefix(o, "--chdir=") || strings.HasPrefix(o, "--chroot=") ||
					strings.HasPrefix(o, "--host=") || strings.HasPrefix(o, "--prompt=") || strings.HasPrefix(o, "--role=") || strings.HasPrefix(o, "--type=") ||
					strings.HasPrefix(o, "--other-user=") || strings.HasPrefix(o, "--command-timeout=") || strings.HasPrefix(o, "--preserve-env=") {
					i++
					continue
				}
				if o == "--preserve-env" || o == "--set-home" || o == "--non-interactive" || o == "--login" || o == "--shell" || o == "--background" || o == "--reset-timestamp" || o == "--remove-timestamp" {
					i++
					continue
				}
				if !strings.HasPrefix(o, "--") {
					flags := o[1:]
					for len(flags) > 0 && strings.IndexByte("AbEHhiKkNnPSsVv", flags[0]) >= 0 {
						flags = flags[1:]
					}
					if flags == "" {
						i++
						continue
					}
					if len(flags) == 1 && strings.IndexByte("ugCDRThprtU", flags[0]) >= 0 {
						i += 2
						continue
					}
					if strings.IndexByte("ugCDRThprtU", flags[0]) >= 0 {
						i++
						continue
					}
				}
				markUnsure("sudo " + o)
				i++
			}
		case name == "nice":
			i++
			for i < len(words) && strings.HasPrefix(words[i], "-") {
				o := words[i]
				if o == "--" {
					i++
					break
				}
				if o == "-n" || o == "--adjustment" {
					i += 2
					continue
				}
				if strings.HasPrefix(o, "-n") || strings.HasPrefix(o, "--adjustment=") || isSignedDigits(o[1:]) {
					i++
					continue
				}
				markUnsure("nice " + o)
				i++
			}
		case name == "timeout":
			i++
			for i < len(words) && strings.HasPrefix(words[i], "-") {
				o := words[i]
				if o == "--" {
					i++
					break
				}
				if o == "-s" || o == "-k" || o == "--signal" || o == "--kill-after" {
					i += 2
					continue
				}
				if strings.HasPrefix(o, "-s") || strings.HasPrefix(o, "-k") || strings.HasPrefix(o, "--signal=") || strings.HasPrefix(o, "--kill-after=") ||
					o == "-v" || o == verboseFlag || o == "--foreground" || o == "--preserve-status" {
					i++
					continue
				}
				if !strings.HasPrefix(o, "--") {
					flags := o[1:]
					for len(flags) > 0 && (flags[0] == 'v' || flags[0] == 'f' || flags[0] == 'p') {
						flags = flags[1:]
					}
					if flags == "" {
						i++
						continue
					}
					if flags == "s" || flags == "k" {
						i += 2
						continue
					}
					if flags[0] == 's' || flags[0] == 'k' {
						i++
						continue
					}
				}
				markUnsure("timeout " + o)
				i++
			}
			if i < len(words) && duration.MatchString(words[i]) {
				i++
			}
		case name == "stdbuf":
			i++
			for i < len(words) && strings.HasPrefix(words[i], "-") {
				o := words[i]
				if o == "--" {
					i++
					break
				}
				if o == "-i" || o == "-o" || o == "-e" || o == "--input" || o == "--output" || o == "--error" {
					i += 2
					continue
				}
				if strings.HasPrefix(o, "-i") || strings.HasPrefix(o, "-o") || strings.HasPrefix(o, "-e") ||
					strings.HasPrefix(o, "--input=") || strings.HasPrefix(o, "--output=") || strings.HasPrefix(o, "--error=") {
					i++
					continue
				}
				markUnsure("stdbuf " + o)
				i++
			}
		case name == "ionice":
			i++
			for i < len(words) && strings.HasPrefix(words[i], "-") {
				o := words[i]
				if o == "--" {
					i++
					break
				}
				if o == "-c" || o == "-n" || o == "-p" || o == "-P" || o == "-u" || o == "--class" || o == "--classdata" {
					i += 2
					continue
				}
				if strings.HasPrefix(o, "-c") || strings.HasPrefix(o, "-n") || strings.HasPrefix(o, "--class=") || strings.HasPrefix(o, "--classdata=") ||
					o == "-t" || o == "--ignore" {
					i++
					continue
				}
				if !strings.HasPrefix(o, "--") {
					flags := o[1:]
					for len(flags) > 0 && flags[0] == 't' {
						flags = flags[1:]
					}
					if flags == "" {
						i++
						continue
					}
					if flags == "c" || flags == "n" {
						i += 2
						continue
					}
					if flags[0] == 'c' || flags[0] == 'n' {
						i++
						continue
					}
				}
				markUnsure("ionice " + o)
				i++
			}
		case name == "chrt":
			i++
			for i < len(words) && strings.HasPrefix(words[i], "-") {
				o := words[i]
				if o == "--" {
					i++
					break
				}
				if o == "-p" || o == "--pid" {
					return words[i:], cmp.Or(unsure, "chrt")
				}
				if o == "-T" || o == "-P" || o == "-D" || o == "--sched-runtime" || o == "--sched-period" || o == "--sched-deadline" {
					i += 2
					continue
				}
				if !strings.HasPrefix(o, "--") && strings.Trim(o[1:], "bfiorRmv") == "" {
					i++
					continue
				}
				if o == "--batch" || o == "--fifo" || o == "--idle" || o == "--other" || o == "--rr" || o == "--reset-on-fork" || o == verboseFlag {
					i++
					continue
				}
				markUnsure("chrt " + o)
				i++
			}
			if i < len(words) && isSignedDigits(words[i]) {
				i++
			}
		case name == "taskset":
			markUnsure("taskset")
			i++
			inlineList := false
			for i < len(words) && strings.HasPrefix(words[i], "-") {
				o := words[i]
				if o == "--" {
					i++
					break
				}
				if o == "-c" || o == "--cpu-list" || o == "-a" || o == "--all-tasks" {
					i++
					continue
				}
				if strings.HasPrefix(o, "-c") && len(o) > 2 {
					inlineList = true
					i++
					break
				}
				i++
			}
			if !inlineList && i < len(words) {
				i++ // mask or cpu-list
			}
		case name == "flock":
			i++
			for i < len(words) && strings.HasPrefix(words[i], "-") {
				o := words[i]
				if o == "--" {
					i++
					break
				}
				if o == "-w" || o == "-E" || o == "--wait" || o == "--timeout" || o == "--conflict-exit-code" {
					i += 2
					continue
				}
				if strings.HasPrefix(o, "-w") || strings.HasPrefix(o, "-E") || strings.HasPrefix(o, "--wait=") || strings.HasPrefix(o, "--timeout=") || strings.HasPrefix(o, "--conflict-exit-code=") ||
					o == "-s" || o == "-x" || o == "-e" || o == "-u" || o == "-n" || o == "-o" || o == "-F" || o == "-v" ||
					o == "--shared" || o == "--exclusive" || o == "--unlock" || o == "--nonblock" || o == "--nb" || o == "--close" || o == "--no-fork" || o == verboseFlag {
					i++
					continue
				}
				if !strings.HasPrefix(o, "--") && strings.Trim(o[1:], "sxeunoFv") == "" {
					i++
					continue
				}
				markUnsure("flock " + o)
				i++
			}
			if i < len(words) {
				i++ // lock file or fd
			}
			if i < len(words) && (words[i] == "-c" || words[i] == "--command" || strings.HasPrefix(words[i], "--command=")) {
				return words[i:], cmp.Or(unsure, "flock")
			}
		case name == xargsCmd:
			markUnsure(xargsCmd)
			i++
			for i < len(words) && strings.HasPrefix(words[i], "-") {
				o := words[i]
				if o == "--" {
					i++
					break
				}
				if wrapperValues[o] || o == "-a" || o == "-d" || o == "-E" || o == "-I" || o == "-L" || o == "-l" || o == "-n" || o == "-P" || o == "-s" || o == "--arg-file" || o == "--delimiter" || o == "--eof" || o == "--replace" || o == "--max-lines" || o == "--max-args" || o == "--max-procs" || o == "--max-chars" {
					i += 2
					continue
				}
				i++
			}
		case name == watchCmd:
			return words[i:], cmp.Or(unsure, watchCmd)
		case unmodeledWrappers[name]:
			return words[i:], cmp.Or(unsure, name)
		case wrappers[name]:
			i++
			for i < len(words) && strings.HasPrefix(words[i], "-") {
				if wrapperValues[words[i]] {
					i += 2
				} else {
					i++
				}
			}
		default:
			return words[i:], unsure
		}
	}
	return nil, unsure
}

func isSignedDigits(s string) bool {
	s = strings.TrimPrefix(strings.TrimPrefix(s, "+"), "-")
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// envSplitCommand extracts the command string passed to env -S / --split-string.
func envSplitCommand(words []string) string {
	for i := 0; i < len(words); i++ {
		if path.Base(words[i]) != envCmdName {
			continue
		}
		for j := i + 1; j < len(words); j++ {
			o := words[j]
			if o == "-S" || o == "--split-string" {
				if j+1 < len(words) {
					return words[j+1]
				}
			}
			if rest, ok := strings.CutPrefix(o, "-S"); ok && rest != "" && !strings.HasPrefix(o, "--") {
				return rest
			}
			if rest, ok := strings.CutPrefix(o, "--split-string="); ok {
				return rest
			}
		}
	}
	return ""
}

// flockCommand extracts the command string passed to flock <lock> -c <cmd>.
func flockCommand(words []string) string {
	for i := 0; i < len(words); i++ {
		if path.Base(words[i]) != "flock" {
			continue
		}
		for j := i + 1; j < len(words); j++ {
			o := words[j]
			if (o == "-c" || o == "--command") && j+1 < len(words) {
				return words[j+1]
			}
			if rest, ok := strings.CutPrefix(o, "--command="); ok {
				return rest
			}
		}
	}
	return ""
}

// gitAliasCommands extracts shell or git commands defined via git -c alias.<name>=<val>.
func gitAliasCommands(args []string) []string {
	var out []string
	for i := 0; i < len(args); i++ {
		var kv string
		switch {
		case args[i] == "-c" && i+1 < len(args):
			kv = args[i+1]
			i++
		case strings.HasPrefix(args[i], "-c") && len(args[i]) > 2 && !strings.HasPrefix(args[i], "--"):
			kv = args[i][2:]
		default:
			continue
		}
		k, v, ok := strings.Cut(kv, "=")
		if !ok || !strings.HasPrefix(strings.ToLower(k), "alias.") {
			continue
		}
		if rest, ok := strings.CutPrefix(v, "!"); ok {
			out = append(out, rest)
		} else {
			out = append(out, "git "+v)
		}
	}
	return out
}

// isReadOnlyUnlockSearch reports whether a pipeline stage that mentions an
// unlock helper path is a read-only search, diff, status or metadata command
// whose redirected output does not copy the helper itself.
func isReadOnlyUnlockSearch(cmdName string, args []string) bool {
	if grepCmds[cmdName] || metaCmds[cmdName] || cmdName == "gh" {
		return true
	}
	if cmdName == gitCmd {
		sub := nonFlags(args)
		if len(sub) == 0 {
			return false
		}
		switch sub[0] {
		case "di" + "ff", "log", "st" + "atus", "blame", "grep", "add", "commit", "branch", "rev-parse":
			return true
		}
	}
	return false
}

func hasInPlaceFlag(args []string) bool {
	for _, a := range args {
		if a == "--" {
			break
		}
		if a == "-i" || a == "--in-place" || strings.HasPrefix(a, "--in-place=") {
			return true
		}
		if strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "--") && strings.ContainsRune(a[1:], 'i') {
			return true
		}
	}
	return false
}

// gitConfigLeak checks git -c <key>=<val> options that execute commands
// (core.pager, core.editor, core.sshcommand, core.fsmonitor, diff.external,
// credential.helper) and refuses piping git output of an unlock helper or
// script into a shell/interpreter via core.pager.
func (g secretGuard) gitConfigLeak(args []string, copied map[string]string, depth int) *leak {
	execKeys := map[string]bool{
		"core.pager":        true,
		"core.editor":       true,
		"core.sshcommand":   true,
		"core.fsmonitor":    true,
		"diff.external":     true,
		"credential.helper": true,
	}
	for i := 0; i < len(args); i++ {
		var kv string
		switch {
		case args[i] == "-c" && i+1 < len(args):
			kv = args[i+1]
			i++
		case strings.HasPrefix(args[i], "-c") && len(args[i]) > 2 && !strings.HasPrefix(args[i], "--"):
			kv = args[i][2:]
		default:
			continue
		}
		k, v, ok := strings.Cut(kv, "=")
		if !ok || !execKeys[strings.ToLower(k)] {
			continue
		}
		v = strings.TrimPrefix(v, "!")
		if l := g.scan(v, depth+1); l != nil {
			return l
		}
		vWords := shellWords(v)
		scIdx := commandAt(vWords)
		if scIdx < len(vWords) {
			vName := path.Base(vWords[scIdx])
			if shells[vName] || interpreters[vName].code != nil || len(interpreters[vName].file) > 0 || slices.Contains(g.unlock, vName) {
				for _, a := range args {
					if strings.HasPrefix(a, "-") {
						continue
					}
					if l := g.unlockOrScript(a, copied, depth); l != nil {
						return l
					}
					if _, rest, ok := strings.Cut(a, ":"); ok && rest != "" {
						if l := g.unlockOrScript(rest, copied, depth); l != nil {
							return l
						}
					}
				}
			}
		}
	}
	return nil
}

var sedExecCommand = regexp.MustCompile(`(?:^|[;\s{}0-9,$!])e(?:\s+(.*)|$)`)

// sedExecLeak checks sed -f script files and the GNU sed `e` command.
func (g secretGuard) sedExecLeak(args []string, copied map[string]string, depth int) *leak {
	var scripts, pos []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			pos = append(pos, args[i+1:]...)
			break
		}
		switch {
		case (a == "-f" || a == "--file") && i+1 < len(args):
			if l := g.unlockOrScript(args[i+1], copied, depth); l != nil {
				return l
			}
			i++
		case strings.HasPrefix(a, "--file="):
			if l := g.unlockOrScript(strings.TrimPrefix(a, "--file="), copied, depth); l != nil {
				return l
			}
		case strings.HasPrefix(a, "-f") && len(a) > 2 && !strings.HasPrefix(a, "--"):
			if l := g.unlockOrScript(a[2:], copied, depth); l != nil {
				return l
			}
		case (a == "-e" || a == "--expression") && i+1 < len(args):
			scripts = append(scripts, args[i+1])
			i++
		case strings.HasPrefix(a, "--expression="):
			scripts = append(scripts, strings.TrimPrefix(a, "--expression="))
		case strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "--"):
			if idx := strings.IndexByte(a[1:], 'e'); idx >= 0 {
				rest := a[1+idx+1:]
				if rest != "" {
					scripts = append(scripts, rest)
				} else if i+1 < len(args) {
					scripts = append(scripts, args[i+1])
					i++
				}
			}
		case !strings.HasPrefix(a, "-"):
			pos = append(pos, a)
		}
	}
	if len(scripts) == 0 && len(pos) > 0 {
		scripts = append(scripts, pos[0])
		pos = pos[1:]
	}
	hasExec := false
	for _, s := range scripts {
		if m := sedExecCommand.FindStringSubmatch(s); m != nil {
			hasExec = true
			if subCmd := strings.TrimSpace(m[1]); subCmd != "" {
				if l := g.scan(subCmd, depth+1); l != nil {
					return l
				}
			}
		}
	}
	if hasExec {
		for _, p := range pos {
			if l := g.unlockOrScript(p, copied, depth); l != nil {
				return l
			}
		}
	}
	return nil
}

// installedUnlockLeak refuses any command that touches an installed unlock
// helper under ~/.local/bin or ~/bin (even a read such as `cat ~/.local/bin/<helper>`),
// while repository files with the same basename inside a checkout/worktree
// remain readable and inspectable.
func (g secretGuard) installedUnlockLeak(words []string, dirs []string) *leak {
	if len(g.unlock) == 0 {
		return nil
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return nil
	}
	localBin := filepath.Join(home, ".local", "bin")
	homeBin := filepath.Join(home, "bin")
	k := commandAt(words)
	for _, w := range words[k:] {
		if _, v, ok := strings.Cut(w, "="); ok {
			w = v
		}
		w = strings.TrimLeft(w, "<>|&")
		if strings.HasPrefix(w, "!") || (!strings.Contains(w, "/") && !strings.Contains(w, "~") && !strings.Contains(w, "$HOME")) {
			continue
		}
		b := path.Base(w)
		if !slices.Contains(g.unlock, b) {
			continue
		}
		for _, d := range dirs {
			p := wordPath(w, d)
			if p == "" {
				continue
			}
			for _, cand := range []string{p} {
				dir := filepath.Dir(cand)
				if dir == localBin || dir == homeBin {
					return &leak{what: b + ", a vault unlock", never: true, unlock: true}
				}
				if real, err := filepath.EvalSymlinks(cand); err == nil {
					rdir := filepath.Dir(real)
					if rdir == localBin || rdir == homeBin {
						return &leak{what: b + ", a vault unlock", never: true, unlock: true}
					}
				}
			}
		}
	}
	return nil
}

// toolLeak returns the leak of a credential tool that runs only in beekeeper
// (sops, op, decryption), of a vault sign-in or unlock or a keyring read, of
// a hash or a diff of a secret file, or of a render fed with secret values.
func (g secretGuard) toolLeak(words []string, copied map[string]string) *leak {
	rest, unsure := unwrapWords(words)
	if unsure != "" {
		for _, w := range rest {
			b := path.Base(w)
			if slices.Contains(g.unlock, b) {
				return &leak{what: b + ", a vault unlock", never: true, unlock: true}
			}
			if orig, ok := copied[w]; ok {
				return &leak{what: orig + ", a vault unlock", never: true, unlock: true}
			}
			if orig, ok := copied[b]; ok {
				return &leak{what: orig + ", a vault unlock", never: true, unlock: true}
			}
		}
	}
	k := len(words) - len(rest)
	if k == len(words) {
		return nil
	}
	name, args := path.Base(words[k]), words[k+1:]
	if orig, ok := copied[words[k]]; ok {
		return &leak{what: orig + ", a vault unlock", never: true, unlock: true}
	}
	if orig, ok := copied[name]; ok {
		return &leak{what: orig + ", a vault unlock", never: true, unlock: true}
	}
	sub := nonFlags(args)
	switch {
	case slices.Contains(g.unlock, name):
		return &leak{what: name + ", a vault unlock", never: true, unlock: true}
	case name == "op" && len(sub) > 0 && (sub[0] == "signin" || sub[0] == verbUnlock || len(sub) > 1 && sub[0] == "account" && sub[1] == "add"):
		return &leak{what: "op " + strings.Join(sub[:min(len(sub), 2)], " ") + ", a vault sign-in", never: true, unlock: true}
	case name == selfCmd && slices.Contains(sub, "secret") && slices.Contains(sub, verbUnlock):
		return &leak{what: "beekeeper secret unlock, the person's own unlock", never: true, unlock: true}
	case name == selfCmd && secretOp(sub) == "store":
		return &leak{what: "beekeeper secret store, the person's own store into the vault", never: true, person: true}
	case name == sopsCmd:
		return &leak{what: "sops, which runs only in beekeeper", safe: sopsSafe, never: true}
	case name == "op":
		return &leak{what: "op, which runs only in beekeeper", safe: opSafe, never: true}
	case name == "secret-tool" && len(sub) > 0 && (sub[0] == "lookup" || sub[0] == "search"),
		name == "security" && len(sub) > 0 && strings.HasPrefix(sub[0], "find-") && strings.HasSuffix(sub[0], "-password"):
		return &leak{what: "a keyring read", safe: keyringSafe, never: true}
	case name == "vault":
		return vaultLeak(args)
	case name == helmCmd && len(sub) > 0 && sub[0] == "secrets":
		return &leak{what: "helm secrets", safe: sopsSafe, never: true}
	case (name == "age" || name == "rage" || name == "gpg" || name == "gpg2") && hasAny(args, "-d", "--decrypt"):
		return &leak{what: name + " --decrypt", safe: cryptSafe, never: true}
	case hashCmds[name], name == "openssl" && hasAny(sub, "dgst", "sha256", "sha1", "md5"):
		if f := secretFileArg(args); f != "" {
			return &leak{what: "a hash of " + f + ", which a guess can be checked against", safe: fileSafe, never: true}
		}
	case diffCmds[name], name == gitCmd && len(sub) > 0 && sub[0] == "diff" && hasAny(args, "--no-index"):
		if f := secretFileArg(args); f != "" {
			return &leak{what: "a diff of " + f + ", which prints its values", safe: fileSafe, never: true}
		}
	case name == helmCmd && len(sub) > 0 && (sub[0] == "template" || sub[0] == "install" || sub[0] == "upgrade"):
		if f := secretValues(args); f != "" {
			return &leak{what: "helm " + sub[0] + " with the secret values of " + f, safe: renderSafe, render: true}
		}
	case name == "kustomize" && len(sub) > 0 && sub[0] == "build" && hasAny(args, "--enable-alpha-plugins", "--enable-exec"):
		return &leak{what: "kustomize build with plugins that decrypt", safe: renderSafe, render: true}
	}
	return nil
}

// secretOp is the beekeeper secret operation among the words sub, "" for
// none.
func secretOp(sub []string) string {
	if i := slices.Index(sub, "secret"); i >= 0 && i+1 < len(sub) {
		return sub[i+1]
	}
	return ""
}

// secretFile reports whether a file name may hold secret values in
// plaintext: not code or prose, not a SOPS-encrypted file.
func secretFile(p string) bool {
	b := path.Base(p)
	return secretName.MatchString(b) && !codeFile.MatchString(b) && !strings.Contains(b, ".enc.") && !strings.Contains(b, ".sops.")
}

// secretFileArg returns the first argument that names a secret file.
func secretFileArg(args []string) string {
	for _, a := range args {
		if !strings.HasPrefix(a, "-") && secretFile(a) {
			return a
		}
	}
	return ""
}

// secretValues returns the first values file of a helm command that names a
// secret file (-f, --values, --set-file).
func secretValues(args []string) string {
	for i, a := range args {
		name, val, eq := strings.Cut(a, "=")
		switch name {
		case "-f", "--values", "--set-file":
		default:
			continue
		}
		if !eq {
			if i+1 == len(args) {
				continue
			}
			val = args[i+1]
		}
		for v := range strings.SplitSeq(val, ",") {
			if name == "--set-file" {
				_, v, _ = strings.Cut(v, "=")
			}
			if secretFile(v) {
				return v
			}
		}
	}
	return ""
}

// kubectlLeak: before is the pipeline before the command; a kubectl get of
// names from stdin or a variable counts as a Secret read when it mentions
// Secrets.
func kubectlLeak(args []string, before string) *leak {
	verb, rest := "", []string{}
	output, template, raw := "", "", ""
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
			case "-o", flagOutput:
				output = val
			case flagTemplate:
				template = val
			case "--raw":
				raw = val
			}
		case strings.HasPrefix(a, "-"):
		case verb == "":
			verb = a
		default:
			rest = append(rest, a)
		}
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
	switch verb {
	case "view-secret":
		return &leak{what: "kubectl view-secret", safe: kubectlSafe, never: true}
	case verbEdit:
		if secret {
			return &leak{what: "kubectl edit of a Secret", safe: kubectlSafe, never: true}
		}
		return nil
	case "kustomize":
		if hasAny(args, "--enable-alpha-plugins", "--enable-exec") {
			return &leak{what: "kubectl kustomize with plugins that decrypt", safe: renderSafe, render: true}
		}
		return nil
	case verbGet:
		if strings.Contains(raw, "/secrets") {
			return &leak{what: "kubectl get --raw of Secrets", safe: kubectlSafe}
		}
		if !secret && (!unknown || !strings.Contains(strings.ToLower(before), "secret")) {
			return nil
		}
	case verbCreate, "apply", "replace", "patch", "annotate", "label", verbSet:
		// They print the object they write with -o.
		if !secret {
			return nil
		}
	default:
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
	return &leak{what: "kubectl " + verb + " secret -o " + format, safe: kubectlSafe}
}

// templateLeaks reports whether a kubectl output template prints more than
// keys and harmless metadata: a range over the data prints only keys unless
// it uses the value variable, and every field it names must be harmless.
func templateLeaks(tmpl string) bool {
	for _, m := range templateKeys.FindAllStringSubmatch(tmpl, -1) {
		if m[1] != "" && len(regexp.MustCompile(`\$`+m[1]+`\b`).FindAllStringIndex(tmpl, -1)) > 1 {
			return true
		}
	}
	rest := templateKeys.ReplaceAllString(tmpl, "KEYS")
	if wholeObject.MatchString(rest) {
		return true
	}
	for _, p := range fieldPath.FindAllString(rest, -1) {
		if !harmlessPath.MatchString(p) {
			return true
		}
	}
	return false
}

// vaultLeak: vault is an allow list of commands that print no value; kv get
// and read print values unless their output keeps only keys, everything
// else is refused.
func vaultLeak(args []string) *leak {
	sub := nonFlags(args)
	if len(sub) == 0 {
		return nil
	}
	switch {
	case sub[0] == "status", sub[0] == verbVersion, sub[0] == verbList,
		sub[0] == "kv" && len(sub) > 1 && (sub[1] == verbList || sub[1] == "metadata" && len(sub) > 2 && sub[2] == verbGet),
		len(sub) > 1 && sub[1] == verbList && (sub[0] == "secrets" || sub[0] == "auth" || sub[0] == "policy" || sub[0] == "audit"):
		return nil
	case sub[0] == "kv" && len(sub) > 1 && sub[1] == verbGet:
		return &leak{what: "vault kv get", safe: vaultSafe}
	case sub[0] == "read":
		return &leak{what: "vault read", safe: vaultSafe}
	}
	return &leak{what: "vault " + strings.Join(sub[:min(2, len(sub))], " ") + ", which is not on the allow list", safe: vaultSafe, never: true}
}

// flowsSafely reports whether the output of the source in segs[i] ends only
// in key names, metadata, a length, a blanked render or a consumer that
// prints nothing; a file, a tee, a variable, a hash or the terminal is no
// safe end.
func flowsSafely(sc shellScan, segs []segment, i int, l *leak) bool {
	if l.never {
		return false
	}
	cmd, masked := sc.plain, sc.masked
	for j := i; ; j++ {
		sg := segs[j]
		if j > i {
			words := shellWords(cmd[sg.start:sg.end])
			if safeSink(words, l) {
				return true
			}
			if k := commandAt(words); k < len(words) && path.Base(words[k]) == "tee" {
				return false
			}
		}
		for _, r := range redirect.FindAllStringSubmatchIndex(masked[sg.start:sg.end], -1) {
			target := strings.Trim(cmd[sg.start+r[2]:sg.start+r[3]], `'"`)
			switch {
			case target == "/dev/null":
				return true
			case !terminal.MatchString(target):
				return false
			}
		}
		if sg.after != "|" || j+1 == len(segs) {
			return false
		}
	}
}

// safeSink reports whether a pipeline stage consumes its input printing
// only key names, metadata or a length, or nothing of it.
func safeSink(words []string, l *leak) bool {
	k := commandAt(words)
	if k == len(words) {
		return false
	}
	name, args := path.Base(words[k]), words[k+1:]
	switch {
	case name == "wc", name == "kubeseal", hasAny(args, "--password-stdin"):
		return true
	case name == "openssl":
		// a certificate's fields without the PEM
		return hasAny(args, "x509") && hasAny(args, "-noout")
	case name == "age-keygen":
		return hasAny(args, "-y")
	case name == "jq" || name == "gojq" || name == "yq":
		filter, ok := jqFilter(args)
		return ok && (filterSafe(filter) || l.render && blanksSecrets(filter) || l.file != "" && strips(filter, l.file))
	case name == kubectlCmd:
		verb := kubectlVerb(args)
		return (verb == "apply" || verb == verbCreate || verb == "replace") && readsStdin(args) && !printsObject(args)
	case name == "gh":
		sub := nonFlags(args)
		return len(sub) > 1 && sub[0] == "secret" && sub[1] == verbSet
	}
	return false
}

// kubectlVerb returns the verb of a kubectl command: its first word that is
// no flag and no flag's value (kubectl --context x apply …).
func kubectlVerb(args []string) string {
	for i := 0; i < len(args); i++ {
		a := args[i]
		name, _, eq := strings.Cut(a, "=")
		switch {
		case a == "--":
			return ""
		case strings.HasPrefix(a, "-"):
			if kubectlValue[name] && !eq {
				i++
			}
		default:
			return a
		}
	}
	return ""
}

func readsStdin(args []string) bool {
	for i, a := range args {
		if (a == "-f" || a == "--filename") && i+1 < len(args) && args[i+1] == "-" || a == "-f-" || a == "-f=-" || a == "--filename=-" {
			return true
		}
	}
	return false
}

// printsObject reports whether a kubectl write prints the object (-o).
func printsObject(args []string) bool {
	for _, a := range args {
		if strings.HasPrefix(a, "-o") || a == flagOutput || strings.HasPrefix(a, "--output=") {
			return true
		}
	}
	return false
}

// jqFilter returns the filter of a jq or yq invocation; false when it has
// none or reads it from a file.
func jqFilter(args []string) (string, bool) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-f" || a == "--from-file":
			return "", false
		case a == "--arg" || a == "--argjson" || a == "--slurpfile" || a == "--rawfile":
			i += 2
		case a == "--indent" || a == "-o" || a == "--output-format" || a == "-p" || a == "--input-format":
			i++
		case strings.HasPrefix(a, "-"):
		case a == "e" || a == "eval":
		default:
			return a, true
		}
	}
	return "", false
}

// filterSafe reports whether a jq or yq filter prints only keys or metadata.
func filterSafe(filter string) bool {
	rest := keysOnly.ReplaceAllString(filter, " KEYS ")
	return !leakyFilter.MatchString(rest) && harmlessFilter.MatchString(rest)
}

// blanksSecrets reports whether a jq or yq filter blanks both the data and
// the stringData of what it prints (del(.data, .stringData), .data |= keys)
// and names them nowhere else.
func blanksSecrets(filter string) bool {
	blanked := map[string]bool{}
	rest := blankData.ReplaceAllStringFunc(filter, func(m string) string {
		blanked[blankData.FindStringSubmatch(m)[1]] = true
		return " BLANK "
	})
	for {
		at := strings.Index(rest, "del(")
		if at < 0 {
			break
		}
		end, depth := at+len("del("), 1
		for ; end < len(rest) && depth > 0; end++ {
			switch rest[end] {
			case '(':
				depth++
			case ')':
				depth--
			}
		}
		inner := dataField.ReplaceAllStringFunc(rest[at:end], func(m string) string {
			blanked[dataField.FindStringSubmatch(m)[1]] = true
			return " BLANK "
		})
		rest = rest[:at] + strings.Replace(inner, "del(", "DEL(", 1) + rest[end:]
	}
	return blanked["data"] && blanked["stringData"] && !dataWord.MatchString(rest)
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

// segment is one simple command of a shell command line: its offsets and the
// operators before and after it ("|", "&&", ";", "$(", ")", …).
type segment struct {
	start, end    int
	before, after string
}

// heredoc is a here-document: the offset of its <<, its body, and whether its
// delimiter was quoted (suppressing shell expansion in the body).
type heredoc struct {
	op, start, end int
	quoted         bool
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
		op     int
		delim  string
		tabs   bool
		quoted bool
	}
	// frames: 't' top, 's' $( ), '"' double quotes, '`' backticks.
	stack := []byte{'t'}
	for i := 0; i < len(b); i++ {
		top := stack[len(stack)-1]
		c := b[i]
		if top == '"' {
			switch {
			case c == '\\' && i+1 < len(b) && b[i+1] == '\n':
				b[i], b[i+1] = '_', '_'
				i++
			case c == '\\' && i+1 < len(b):
				b[i], b[i+1] = '_', '_'
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
		case c == '\\' && i+1 < len(b) && b[i+1] == '\n':
			if i == 0 || strings.IndexByte(" \t\n;&|(", b[i-1]) >= 0 {
				b[i], b[i+1] = ' ', ' '
				p[i], p[i+1] = ' ', ' '
			} else {
				b[i], b[i+1] = '_', '_'
			}
			i++
		case c == '\\' && i+1 < len(b):
			b[i], b[i+1] = '_', '_'
			i++
		case c == '$' && i+1 < len(b) && b[i+1] == '$':
			i++ // $$ is the shell's pid: a quote after it is a plain one
		case c == '$' && i+1 < len(b) && b[i+1] == '\'':
			// $'…' ends at its first unescaped quote
			for i += 2; i < len(b) && s[i] != '\''; i++ {
				if s[i] == '\\' && i+1 < len(b) {
					b[i] = '_'
					i++
				}
				b[i] = '_'
			}
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
		case c == '(' && top == 's':
			stack = append(stack, 's')
		case c == ')' && top == 's', c == '`' && top == '`':
			stack = stack[:len(stack)-1]
		case c == '`':
			stack = append(stack, '`')
		case c == '#' && (i == 0 || strings.IndexByte(" \t\n;&|(", b[i-1]) >= 0 || (top == '`' && b[i-1] == '`')):
			for i < len(b) && b[i] != '\n' && (top != '`' || b[i] != '`') {
				b[i], p[i] = '_', ' '
				i++
			}
			i--
		case c == '<' && (i == 0 || s[i-1] != '<') && strings.HasPrefix(s[i:], "<<") && !strings.HasPrefix(s[i:], "<<<"):
			if m := heredocOp.FindStringSubmatch(s[i:]); m != nil {
				pending = append(pending, struct {
					op     int
					delim  string
					tabs   bool
					quoted bool
				}{i, m[2] + m[3] + m[4], m[1] == "-", m[2] != "" || m[3] != ""})
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
						docs = append(docs, heredoc{p.op, body, pos, p.quoted})
						if end < 0 {
							pos = len(s)
						} else {
							pos += end
						}
						break
					}
					if end < 0 {
						docs = append(docs, heredoc{p.op, body, len(s), p.quoted})
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
	p := sc.plain
	isBlankOrComment := func(k int) bool {
		return m[k] == ' ' || m[k] == '\t' || (m[k] == '_' && p[k] == ' ')
	}
	skipLinebreak := func(pos int) int {
		j := pos
		for j < len(m) && isBlankOrComment(j) {
			j++
		}
		if j < len(m) && m[j] == '\n' {
			for j < len(m) && (isBlankOrComment(j) || m[j] == '\n') {
				j++
			}
			return j
		}
		return pos
	}
	for i := 0; i < len(m); i++ {
		c := m[i]
		switch {
		case c == '|' && i+1 < len(m) && m[i+1] == '|', c == '&' && i+1 < len(m) && m[i+1] == '&':
			next := skipLinebreak(i + 2)
			cut(i, next, m[i:i+2])
			i = next - 1
		case c == '|' && i+1 < len(m) && m[i+1] == '&':
			next := skipLinebreak(i + 2)
			cut(i, next, "|")
			i = next - 1
		case c == '|' && i > 0 && m[i-1] == '>':
		case c == '|':
			next := skipLinebreak(i + 1)
			cut(i, next, "|")
			i = next - 1
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

// shellWords splits a simple command into its words with the quotes removed
// and $'…' escapes decoded.
func shellWords(s string) []string {
	var words []string
	var w strings.Builder
	in := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '$' && i+1 < len(s) && s[i+1] == '$':
			w.WriteString("$$") // the shell's pid: a quote after it is a plain one
			i++
			in = true
		case c == '$' && i+1 < len(s) && s[i+1] == '\'':
			for i += 2; i < len(s) && s[i] != '\''; {
				if s[i] != '\\' {
					w.WriteByte(s[i])
					i++
					continue
				}
				b, n := ansiC(s[i:])
				w.Write(b)
				i += n
			}
			in = true
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
				if s[i] == '\\' && i+1 < len(s) && s[i+1] == '\n' {
					i++
					continue
				}
				if s[i] == '\\' && i+1 < len(s) && strings.IndexByte(`"\$`+"`", s[i+1]) >= 0 {
					i++
				}
				w.WriteByte(s[i])
			}
			in = true
		case c == '\\' && i+1 < len(s) && s[i+1] == '\n':
			i++
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
