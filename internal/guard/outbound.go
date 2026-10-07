package guard

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

// The outbound guard refuses a tool call that would send a secret value off
// the machine: a gh or devctl call, a commit message, a push, an upload, a
// connector (MCP) call, a Write or Edit of a configured plan path. It scans
// the text for token patterns (gitleaks' rules, by their names) and for the
// configured phrases, and names what matched, never the match: the refusal
// itself reaches the transcript. A line marked gitleaks:allow is skipped, as
// gitleaks does, for a test fixture that only looks like a token.

// Outbound is what the outbound guard checks beyond the built-in patterns.
type Outbound struct {
	// Phrases never leave the machine (case-insensitive).
	Phrases []string
	// Paths are globs of the files whose Write and Edit are outbound; a
	// directory covers what is under it.
	Paths []string
	// StoreDeny are the secret-store writes refused.
	StoreDeny []StoreRule
	// Git runs git in dir and returns its output, at most pushLimit bytes;
	// nil runs the git binary.
	Git func(dir string, args ...string) ([]byte, error)
}

// StoreRule refuses a secret-store write whose vault and item match the
// globs; an empty glob matches any.
type StoreRule struct{ Vault, Item string }

// allowMarker exempts a line from the scan.
const allowMarker = "gitleaks:allow"

// mcpPrefix starts every connector (MCP) tool's name: their input leaves
// the machine.
const mcpPrefix = "mcp__"

// pushLimit bounds what a push scan reads of the commits it sends.
const pushLimit = 32 << 20

// tokenRule is a gitleaks rule: its id, the keywords one of which a match
// contains (a cheap prefilter), and the pattern.
type tokenRule struct {
	id       string
	keywords []string
	re       *regexp.Regexp
}

func rule(id, pattern string, keywords ...string) tokenRule {
	return tokenRule{id, keywords, regexp.MustCompile(pattern)}
}

var tokenRules = []tokenRule{
	rule("github-pat", `\bghp_[0-9A-Za-z]{36}\b`, "ghp_"),
	rule("github-oauth", `\bgho_[0-9A-Za-z]{36}\b`, "gho_"),
	rule("github-app-token", `\b(?:ghu|ghs)_[0-9A-Za-z]{36}\b`, "ghu_", "ghs_"),
	rule("github-refresh-token", `\bghr_[0-9A-Za-z]{36}\b`, "ghr_"),
	rule("github-fine-grained-pat", `\bgithub_pat_[0-9A-Za-z_]{82}\b`, "github_pat_"),
	rule("gitlab-pat", `\bglpat-[0-9A-Za-z_-]{20}\b`, "glpat-"),
	rule("aws-access-token", `\b(?:A3T[A-Z0-9]|AKIA|ASIA|ABIA|ACCA)[A-Z2-7]{16}\b`, "A3T", "AKIA", "ASIA", "ABIA", "ACCA"),
	rule("gcp-api-key", `\bAIza[0-9A-Za-z_-]{35}\b`, "AIza"),
	rule("slack-token", `\bxox[abposr]-[0-9A-Za-z-]{10,}`, "xox"),
	rule("slack-webhook-url", `hooks\.slack\.com/(?:services|workflows|triggers)/[0-9A-Za-z+/_-]{20,}`, "hooks.slack.com"),
	rule("anthropic-api-key", `\bsk-ant-[a-z]+\d{2}-[0-9A-Za-z_-]{80,}`, "sk-ant-"),
	rule("openai-api-key", `\bsk-(?:proj|svcacct|admin)-[0-9A-Za-z_-]{20,}`, "sk-proj-", "sk-svcacct-", "sk-admin-"),
	rule("npm-access-token", `\bnpm_[0-9A-Za-z]{36}\b`, "npm_"),
	rule("1password-service-account-token", `\bops_eyJ[0-9A-Za-z+/=_-]{50,}`, "ops_eyJ"),
	rule("age-secret-key", `AGE-SECRET-KEY-1[0-9A-Z]{58}`, "AGE-SECRET-KEY-1"),
	rule("private-key", `-----BEGIN[ A-Z0-9_-]{0,100}PRIVATE KEY(?: BLOCK)?-----`, "PRIVATE KEY"),
	rule("jwt", `\beyJ[0-9A-Za-z_-]{10,}\.eyJ[0-9A-Za-z_-]{10,}\.[0-9A-Za-z_-]{10,}`, "eyJ"),
	// A password in a URL; a placeholder ($VAR, <token>, {x}, %s, ***) is none.
	rule(urlCredentials, `\b[a-z][a-z0-9+.-]*://[^\s/:@'"]+:[^\s/@'"$<{%*][^\s/@'"]*@[0-9A-Za-z]`, "://"),
}

// hits are the names of what text carries: the rule ids and the numbers of
// the phrases, in order and once each.
func (o Outbound) hits(text string) []string {
	var out []string
	for _, r := range tokenRules {
		if !slices.ContainsFunc(r.keywords, func(k string) bool { return strings.Contains(text, k) }) {
			continue
		}
		for _, m := range r.re.FindAllStringIndex(text, -1) {
			if !allowed(text, m[0]) {
				out = append(out, r.id)
				break
			}
		}
	}
	lower := strings.ToLower(text)
	for i, p := range o.Phrases {
		if p = strings.ToLower(strings.TrimSpace(p)); p != "" && found(lower, p) {
			out = append(out, fmt.Sprintf("phrase %d of outbound.phrases", i+1))
		}
	}
	return out
}

// found reports whether text holds p on a line not marked gitleaks:allow.
func found(text, p string) bool {
	for from := 0; ; {
		k := strings.Index(text[from:], p)
		if k < 0 {
			return false
		}
		if !allowed(text, from+k) {
			return true
		}
		from += k + len(p)
	}
}

// allowed reports whether the line holding offset at is marked gitleaks:allow.
func allowed(text string, at int) bool {
	start := strings.LastIndexByte(text[:at], '\n') + 1
	end := strings.IndexByte(text[at:], '\n')
	if end < 0 {
		end = len(text) - at
	}
	return strings.Contains(text[start:at+end], allowMarker)
}

func outboundReason(what string, hits []string) string {
	return "Refused by beekeeper's outbound guard: " + what + " carries " + strings.Join(hits, ", ") +
		", and it would leave the machine. Take the value out (a secret goes by reference: its store, vault and item, never its value) " +
		"and send it again; do not echo or search for the value. A test fixture that only looks like a token: mark its line with " + allowMarker + "."
}

// toolRefusal decides a non-Bash call: a connector call's input, and a Write,
// Edit or NotebookEdit of a configured path.
func (o Outbound) toolRefusal(tool string, input map[string]any) string {
	var text, what string
	switch {
	case strings.HasPrefix(tool, mcpPrefix):
		text, what = strings.Join(stringValues(input), "\n"), "the "+tool+" call"
	case tool == writeTool || tool == editTool || tool == multiEditTool || tool == notebookTool:
		file, _ := input[filePathKey].(string)
		if file == "" {
			file, _ = input[notebookPathKey].(string)
		}
		if !o.covers(file) {
			return ""
		}
		// What the call writes; old_string is in the file already.
		var parts []string
		for _, k := range []string{"content", "new_string", "new_source"} {
			parts = append(parts, stringValues(input[k])...)
		}
		edits, _ := input["edits"].([]any)
		for _, e := range edits {
			if m, ok := e.(map[string]any); ok {
				parts = append(parts, stringValues(m["new_string"])...)
			}
		}
		text, what = strings.Join(parts, "\n"), "the "+tool+" of "+file
	default:
		return ""
	}
	if h := o.hits(text); len(h) > 0 {
		return outboundReason(what, h)
	}
	return ""
}

// stringValues are the string values in v, at any depth.
func stringValues(v any) []string {
	switch t := v.(type) {
	case string:
		return []string{t}
	case map[string]any:
		var out []string
		for _, x := range t {
			out = append(out, stringValues(x)...)
		}
		return out
	case []any:
		var out []string
		for _, x := range t {
			out = append(out, stringValues(x)...)
		}
		return out
	}
	return nil
}

// covers reports whether file is one of the configured paths.
func (o Outbound) covers(file string) bool {
	if file == "" {
		return false
	}
	file = filepath.Clean(file)
	for _, p := range o.Paths {
		p = filepath.Clean(p)
		if ok, _ := filepath.Match(p, file); ok || strings.HasPrefix(file, p+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// sends is what an outbound command in a Bash command line sends: its name,
// the directory it runs in, the files it uploads, for a push the refs it
// publishes, and for an HTTP client the body (its data arguments; nil, the
// command line is what it sends).
type sends struct {
	what, dir string
	files     []string
	push      []string
	body      []string
}

// bashRefusal decides a Bash command: the command line (here-documents
// included), the files its outbound commands send and, for a git push, the
// commits it would publish; a refused secret-store write.
func (o Outbound) bashRefusal(cmd, cwd string) string {
	return o.scanBash(cmd, cwd, 0)
}

func (o Outbound) scanBash(cmd, cwd string, depth int) string {
	if depth > 4 {
		return ""
	}
	sc := scanShell(cmd)
	var all []sends
	for _, sg := range sc.segments() {
		words := shellWords(sc.plain[sg.start:sg.end])
		if len(words) > 1 && words[0] == "cd" {
			cwd = resolve(cwd, words[1])
		}
		if r := o.storeRefusal(words); r != "" {
			return r
		}
		if s, ok := outbound(words, cwd); ok {
			all = append(all, s)
		}
		// A command string or here-document a shell runs is a command line
		// of its own.
		if len(words) == 0 || !shells[path.Base(words[0])] {
			continue
		}
		for _, arg := range words[1:] {
			if strings.ContainsAny(arg, " \t\n") {
				if r := o.scanBash(arg, cwd, depth+1); r != "" {
					return r
				}
			}
		}
		for _, h := range sc.heredocs {
			if h.op >= sg.start && h.op < sg.end {
				if r := o.scanBash(cmd[h.start:h.end], cwd, depth+1); r != "" {
					return r
				}
			}
		}
	}
	if len(all) == 0 {
		return ""
	}
	for _, s := range all {
		text, part := cmd, "the command line of `"+s.what+"`"
		if s.body != nil {
			text, part = strings.Join(s.body, "\n"), "the body `"+s.what+"` sends"
		}
		if h := o.hits(text + "\n" + catFiles(text, s.dir)); len(h) > 0 {
			return outboundReason(part, h)
		}
		for _, f := range s.files {
			if h := o.hits(readFile(f, s.dir)); len(h) > 0 {
				return outboundReason(f+", which `"+s.what+"` sends,", h)
			}
		}
		if s.push != nil {
			if h := o.hits(o.pushed(s.dir, s.push)); len(h) > 0 {
				return outboundReason("a commit `"+s.what+"` would publish", h)
			}
		}
	}
	return ""
}

// catSub is a command substitution that reads a file: $(cat f), $(< f).
var catSub = regexp.MustCompile(`\$\(\s*(?:cat\s+|<\s*)([^\s();&|<>]+)\s*\)`)

// catFiles are the contents of the files cmd reads in $(cat …) or $(< …).
func catFiles(cmd, cwd string) string {
	var b strings.Builder
	for _, m := range catSub.FindAllStringSubmatch(cmd, -1) {
		b.WriteString(readFile(strings.Trim(m[1], `'"`), cwd))
		b.WriteByte('\n')
	}
	return b.String()
}

// readFile is the start of a file the hook reads to scan, "" when unreadable.
func readFile(name, cwd string) string {
	if name == "" || name == "-" {
		return ""
	}
	f, err := os.Open(resolve(cwd, name)) //nolint:gosec // a file the command itself sends
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()
	b, _ := io.ReadAll(io.LimitReader(f, pushLimit))
	return string(b)
}

// fileFlags take the name of a file whose content a command sends.
var fileFlags = map[string]bool{"--body-file": true, "--notes-file": true, flagInput: true, "--file": true, "-F": true, "-T": true, "--upload-file": true}

// curlData are curl's flags that send a body.
var curlData = map[string]bool{"-d": true, "--data": true, "--data-raw": true, "--data-binary": true, "--data-urlencode": true,
	"--json": true, "-F": true, "--form": true, "-T": true, "--upload-file": true, "--post-data": true, "--post-file": true}

// resolve is name (~/ allowed) relative to dir.
func resolve(dir, name string) string {
	if name = expandHome(name); filepath.IsAbs(name) {
		return name
	}
	return filepath.Join(dir, name)
}

// duration is a wrapper's number or duration argument (timeout 600, 5m).
var duration = regexp.MustCompile(`^\d+(?:\.\d+)?[smhd]?$`)

// wrappers run the command after them (with their own options).
var wrappers = map[string]bool{"env": true, "command": true, "exec": true, "nohup": true, "time": true, "sudo": true,
	"nice": true, "timeout": true, "xargs": true, "setsid": true, "stdbuf": true, "ionice": true}

// outbound returns what the simple command words sends off the machine.
func outbound(words []string, cwd string) (sends, bool) {
	k := 0
	for k < len(words) && (wrappers[path.Base(words[k])] || strings.HasPrefix(words[k], "-") ||
		strings.Contains(words[k], "=") || duration.MatchString(words[k])) {
		k++
	}
	if k == len(words) {
		return sends{}, false
	}
	name, args := path.Base(words[k]), words[k+1:]
	switch name {
	case "gh", "devctl":
		return sends{what: strings.Join(words[k:min(len(words), k+3)], " "), dir: cwd, files: sentFiles(args)}, true
	case "curl", "wget", "http", "https", "xh":
		// Only the body: a header or URL carries the client's own credential
		// to the service it is for.
		if body := httpBody(args); body != nil {
			return sends{what: name, dir: cwd, files: sentFiles(args), body: body}, true
		}
	case "git":
		return gitSends(args, cwd)
	}
	return sends{}, false
}

// httpBody are the values of an HTTP client's data flags, nil for none.
func httpBody(args []string) []string {
	var out []string
	for i, a := range args {
		name, val, eq := strings.Cut(a, "=")
		switch {
		case !curlData[name]:
		case eq && strings.HasPrefix(name, "--"):
			out = append(out, val)
		case i+1 < len(args):
			out = append(out, args[i+1])
		default:
			out = append(out, "")
		}
	}
	return out
}

// sentFiles are the files named by a file flag, by @file or by field=@file.
func sentFiles(args []string) []string {
	var out []string
	for i, a := range args {
		name, val, eq := strings.Cut(a, "=")
		switch {
		case fileFlags[name] && eq:
			out = append(out, val)
		case fileFlags[a] && i+1 < len(args):
			out = append(out, args[i+1])
		case strings.HasPrefix(a, "@"):
			out = append(out, a[1:])
		case strings.Contains(a, "=@"):
			_, f, _ := strings.Cut(a, "=@")
			out = append(out, f)
		case name == "--post-file" && eq:
			out = append(out, val)
		}
	}
	return out
}

// gitValue are git's global options that take a value.
var gitValue = map[string]bool{"-C": true, "-c": true, "--git-dir": true, "--work-tree": true, "--namespace": true}

// gitSends: a commit (its message), a tag, a remote's URL, a push (the
// commits it publishes).
func gitSends(args []string, cwd string) (sends, bool) {
	dir := cwd
	i := 0
	for ; i < len(args) && strings.HasPrefix(args[i], "-"); i++ {
		if gitValue[args[i]] && i+1 < len(args) {
			if args[i] == "-C" {
				dir = resolve(dir, args[i+1])
			}
			i++
		}
	}
	if i == len(args) {
		return sends{}, false
	}
	sub, rest := args[i], args[i+1:]
	s := sends{what: "git " + sub, dir: dir}
	switch sub {
	case "commit":
		s.files = sentFiles(rest)
	case "tag", "remote":
	case "push":
		s.push = pushRefs(rest)
	default:
		return sends{}, false
	}
	return s, true
}

// pushRefs are the local refs a push's arguments publish: HEAD by default,
// every branch for --all or --mirror, a refspec's source otherwise.
func pushRefs(args []string) []string {
	var pos []string
	for _, a := range args {
		switch {
		case a == "--all" || a == "--mirror" || a == "--branches":
			return []string{"--branches"}
		case a == "--tags":
			pos = append(pos, "--tags")
		case !strings.HasPrefix(a, "-"):
			pos = append(pos, a)
		}
	}
	var refs []string
	for i, p := range pos {
		if i == 0 && p != "--tags" { // the remote
			continue
		}
		src, _, _ := strings.Cut(strings.TrimPrefix(p, "+"), ":")
		if src != "" {
			refs = append(refs, src)
		}
	}
	if len(refs) == 0 {
		return []string{"HEAD"}
	}
	return refs
}

// pushed is the text of the commits a push would publish that no remote has
// yet: their messages and added lines, removed lines left out.
func (o Outbound) pushed(dir string, refs []string) string {
	args := append([]string{"log", "-p", "--no-color", "--no-ext-diff", "-U0", "--format=%B"}, refs...)
	args = append(args, "--not", "--remotes")
	git := o.Git
	if git == nil {
		git = runGit
	}
	out, err := git(dir, args...)
	if err != nil && len(out) == 0 {
		return ""
	}
	var b strings.Builder
	for line := range strings.SplitSeq(string(out), "\n") {
		if !strings.HasPrefix(line, "-") {
			b.WriteString(line)
			b.WriteByte('\n')
		}
	}
	return b.String()
}

// runGit runs git within the hook's time, reading at most pushLimit bytes.
func runGit(dir string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	c := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...) //nolint:gosec // git with fixed subcommands
	out, err := c.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := c.Start(); err != nil {
		return nil, err
	}
	b, rerr := io.ReadAll(io.LimitReader(out, pushLimit))
	_ = c.Process.Kill()
	_ = c.Wait()
	return b, rerr
}

// storeRefusal refuses a secret-store write a deny rule matches: op item or
// document create and edit, vault kv put and patch.
func (o Outbound) storeRefusal(words []string) string {
	if len(o.StoreDeny) == 0 {
		return ""
	}
	for k, w := range words {
		args := words[k+1:]
		var vault, item, what string
		var known bool
		switch path.Base(w) {
		case "op":
			sub := nonFlags(args)
			if len(sub) < 2 || (sub[0] != "item" && sub[0] != "document") || (sub[1] != "create" && sub[1] != "edit") {
				continue
			}
			what = "op " + sub[0] + " " + sub[1]
			vault, known = flagValue(args, "--vault")
			item, _ = flagValue(args, "--title")
			if sub[1] == "edit" && len(sub) > 2 {
				item = sub[2]
			}
		case "vault":
			sub := nonFlags(args)
			if len(sub) < 3 || sub[0] != "kv" || (sub[1] != "put" && sub[1] != "patch") {
				continue
			}
			what = "vault kv " + sub[1]
			item = strings.Trim(sub[2], "/")
			if vault, known = flagValue(args, "-mount"); !known {
				vault, item, _ = strings.Cut(item, "/")
				known = true
			}
		default:
			continue
		}
		for i, r := range o.StoreDeny {
			if match(r.Vault, vault, known) && match(r.Item, item, true) {
				return fmt.Sprintf("Refused by beekeeper's outbound guard: `%s` writes to %s/%s, which storeDeny rule %d (vault %q, item %q) forbids. "+
					"That credential does not belong in this store; leave it where it is kept.", what, orAny(vault), orAny(item), i+1, r.Vault, r.Item)
			}
		}
		return ""
	}
	return ""
}

// match reports whether a store rule's glob matches value; an empty glob or
// an unknown value matches.
func match(glob, value string, known bool) bool {
	if glob == "" || !known {
		return true
	}
	ok, _ := path.Match(strings.ToLower(glob), strings.ToLower(value))
	return ok
}

func orAny(s string) string {
	if s == "" {
		return "?"
	}
	return s
}

// flagValue is the value of a flag given as --flag value or --flag=value.
func flagValue(args []string, flag string) (string, bool) {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1], true
		}
		if v, ok := strings.CutPrefix(a, flag+"="); ok {
			return v, true
		}
	}
	return "", false
}
