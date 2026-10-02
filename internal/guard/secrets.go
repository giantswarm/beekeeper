package guard

import (
	"path"
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
// in an agent session. Every other shape that touches a secret is refused:
// false positives beat leaks.

const (
	kubectlCmd = "kubectl"
	sopsCmd    = "sops"
	helmCmd    = "helm"
	verbGet    = "get"
	verbSet    = "set"
	verbCreate = "create"
	verbEdit   = "edit"
	verbList   = "list"
	flagOutput = "--output"
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
}

// secretOps names the beekeeper secret operations every refusal points to.
const secretOps = "Equality: `beekeeper secret compare <a> <b>` or `beekeeper secret fingerprint <ref>`. " +
	"Changes: `beekeeper secret copy <src.sops.yaml> <dst.sops.yaml> [--name n --namespace ns]`, " +
	"`copy <ref> <file#path>`, `copy <ref> -- <consumer>`, `set <file> <path> --generate --vault op://…`. " +
	"Rotations: `beekeeper secret rotate op://… --generate` (a value beekeeper made), `rotate op://…` (a value its issuer " +
	"rotated into the vault), `rotate platform://<installation>/<capability>/<name> --reason …` (a platform manager credential)."

func (l leak) reason() string {
	at := strings.Join(strings.Fields(l.at), " ")
	if len(at) > 200 {
		at = at[:200] + "…"
	}
	return "Refused: `" + at + "` (" + l.what + ") would put secret values where an agent can read them: the transcript " +
		"and from there the model API, a file, a variable, a hash or a diff. Only key names, metadata and lengths reach an agent, " +
		"and a value goes only to a consumer that prints nothing of it:\n" + l.safe + "\n" + secretOps
}

var (
	kubectlSafe = "  kubectl get secret <name> -o json | jq '.data|keys'\n" +
		"  kubectl get secrets -o name\n" +
		"  kubectl describe secret <name>   (sizes only)\n" +
		"  kubectl get secret <name> -o jsonpath='{.data.<key>}' | base64 -d | wc -c   (the length)"
	sopsSafe = "  sops runs only in beekeeper, never in an agent session, encryption included.\n" +
		"  yq '.stringData|keys' <file>   (a SOPS file keeps its key names in plaintext)"
	opSafe = "  op run -- <command>   (masks the values in the output; the command it runs is checked on its own)\n" +
		"  Every other op command runs only in beekeeper, never in an agent session: beekeeper secret reads the shared vault."
	cryptSafe  = "  Decryption runs only in beekeeper, never in an agent session."
	vaultSafe  = "  vault kv get -format=json <path> | jq '.data.data|keys'\n  vault kv metadata get <path>\n  vault kv list <path>"
	base64Safe = "  … | base64 -d | wc -c   (the length)"
	renderSafe = "  … | yq 'del(.data, .stringData)'   (the render with its Secret data blanked)\n" +
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
	shells   = map[string]bool{"sh": true, "bash": true, "zsh": true, "ksh": true, "dash": true, "ssh": true, "eval": true, "watch": true}
	hashCmds = map[string]bool{"sha256sum": true, "sha1sum": true, "sha224sum": true, "sha384sum": true, "sha512sum": true, "md5sum": true, "b2sum": true, "cksum": true, "shasum": true}
	diffCmds = map[string]bool{"diff": true, "cmp": true, "colordiff": true, "sdiff": true, "diff3": true, "vimdiff": true, "delta": true, "difft": true}
	// wrapperValues: the options of wrappers that take a value.
	wrapperValues = map[string]bool{"-u": true, "-g": true, "-n": true, "-s": true, "-k": true, "-C": true, "-P": true, "-L": true, "-I": true, "-E": true, "-d": true, "-a": true, "--user": true, "--group": true, "--signal": true, "--kill-after": true, "--chdir": true, "--unset": true}
)

// kubectl flags that take a value, so that the value is not taken for the
// verb or a resource.
var kubectlValue = map[string]bool{
	"-n": true, "--namespace": true, "--context": true, "--kubeconfig": true, "--cluster": true, "--user": true,
	"-s": true, "--server": true, "--as": true, "--as-group": true, "--token": true, "--request-timeout": true,
	"-l": true, "--selector": true, "-o": true, flagOutput: true, "--field-selector": true, "--template": true,
	"-f": true, "--filename": true, "-c": true, "--container": true, "--sort-by": true, "--chunk-size": true, "--label-columns": true, "-L": true,
	"--raw": true,
}

// secretLeak returns the leak cmd would expose, nil when it exposes none.
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
func sourceLeak(words []string, before string, aliases map[string]bool) *leak {
	if l := procFileLeak(words); l != nil {
		return l
	}
	if l := toolLeak(words); l != nil {
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
			if k > 0 && subcommandHosts[path.Base(words[k-1])] {
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

// commandAt returns the index of the word the simple command runs, past its
// assignments and wrappers (sudo, env, timeout, xargs, beekeeper run, …).
func commandAt(words []string) int {
	for k := 0; k < len(words); k++ {
		w, name := words[k], path.Base(words[k])
		switch {
		case envWord.MatchString(w), w == "--", duration.MatchString(w), wrappers[name]:
		case strings.HasPrefix(w, "-"):
			if wrapperValues[w] {
				k++
			}
		case name == "beekeeper" && k+1 < len(words) && words[k+1] == "run":
			k++
		default:
			return k
		}
	}
	return len(words)
}

// toolLeak returns the leak of a credential tool that runs only in beekeeper
// (sops, op, decryption), of a hash or a diff of a secret file, or of a
// render fed with secret values.
func toolLeak(words []string) *leak {
	k := commandAt(words)
	if k == len(words) {
		return nil
	}
	name, args := path.Base(words[k]), words[k+1:]
	sub := nonFlags(args)
	switch {
	case name == sopsCmd:
		return &leak{what: "sops, which runs only in beekeeper", safe: sopsSafe, never: true}
	case name == "op":
		// op run masks the values in its command's output; the command it
		// runs is a command of its own, with the same guard.
		if len(sub) > 0 && sub[0] == "run" && !hasAny(args, "--no-masking") {
			if i := slices.Index(args, "--"); i >= 0 {
				return toolLeak(args[i+1:])
			}
			return nil
		}
		return &leak{what: "op, which runs only in beekeeper", safe: opSafe, never: true}
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
	case diffCmds[name], name == "git" && len(sub) > 0 && sub[0] == "diff" && hasAny(args, "--no-index"):
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
			case "--template":
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
	case sub[0] == "status", sub[0] == "version", sub[0] == verbList,
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
		return ok && (filterSafe(filter) || l.render && blanksSecrets(filter))
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
