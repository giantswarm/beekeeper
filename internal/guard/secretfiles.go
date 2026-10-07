package guard

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

// Files known to hold secret values are never read whole by an agent: a cat,
// a Read, a less, a jq . or a grep that prints their lines puts the values in
// the transcript. Their metadata passes, and so does a jq or yq read that
// strips every credential key the file holds, or keeps only key names.

// SecretFiles are the files every session's hook guards, globs allowed, ~/
// for the home directory; secret.files adds to them.
var SecretFiles = []string{
	"~/.omp/agent/models.yml",
	"~/.omp/agent/config.yml",
	"~/.omp/agent/agent.db",
	"~/.claude/.credentials.json",
	"~/.config/gh/hosts.yml",
	"~/.docker/config.json",
	"~/.netrc",
	"~/.git-credentials",
}

var (
	// credentialKey: the last word of a mapping key whose value is a
	// credential (apiKey, X-Auth-Token, client_secret; not maxTokens), which
	// a key-stripped read deletes.
	credentialKey = regexp.MustCompile(`(?i)(?:key|token|secret|password|passwd|passphrase|credentials?|bearer|auth|authorization|cookie)$`)
	// stripFilter: a jq filter whose first stage deletes keys wherever they
	// are (del(.. | .apiKey?, .token?)); group 1 the key list, group 2 the
	// rest of the filter.
	stripFilter = regexp.MustCompile(`^\s*del\(\s*\.\.\s*\|\s*([^()|]+)\)\s*(?:\|(.*))?$`)
	// keyRef: one key of stripFilter's list (.k?, ."k"?, .["k"]?).
	keyRef = regexp.MustCompile(`^\.(?:"([^"]+)"|\[\s*"([^"]+)"\s*\]|([A-Za-z_]\w*))\??$`)
	// reread: a filter part that reads the input again or another input.
	reread = regexp.MustCompile(`\binputs?\b|\binput_filename\b|\$__loc__|\$ENV|\benv\b|\$__prog`)
	// plainKey: a key jq names without quotes.
	plainKey = regexp.MustCompile(`^[A-Za-z_]\w*$`)
	// metaCmds print a file's metadata or move it, never its content.
	metaCmds = map[string]bool{
		"ls": true, "stat": true, "file": true, "wc": true, "du": true, "test": true, "[": true, "[[": true,
		"realpath": true, "readlink": true, "dirname": true, "basename": true, "touch": true, "chmod": true,
		"chown": true, findCmd: true, "lsof": true, "fuser": true, "namei": true, "getfacl": true, "mv": true,
		"rm": true, "trash": true, "trash-put": true, "omp": true, selfCmd: true,
	}
	grepCmds = map[string]bool{"grep": true, "egrep": true, "fgrep": true, "rg": true, "ugrep": true, "ug": true, "ag": true, "zgrep": true}
	// grepQuiet: a grep option that prints counts, file names or nothing.
	grepQuiet     = regexp.MustCompile(`^(?:--(?:files-with(?:out)?-matches?|count|quiet|silent)|-[a-zA-Z]*[lLcq][a-zA-Z]*)$`)
	grepRecursive = regexp.MustCompile(`^(?:--(?:recursive|dereference-recursive)|-[a-zA-Z]*[rR][a-zA-Z]*)$`)
)

// secretFiles matches paths against the guarded files.
type secretFiles struct {
	// globs are absolute patterns.
	globs []string
	// home is the home directory; a directory that contains a guarded file
	// counts for a recursive search only below it.
	home string
}

// newSecretFiles takes the built-in list and extra, ~/ expanded.
func newSecretFiles(extra []string) secretFiles {
	home, _ := os.UserHomeDir()
	f := secretFiles{home: home}
	for _, g := range slices.Concat(SecretFiles, extra) {
		if g = expandHome(g); filepath.IsAbs(g) {
			f.globs = append(f.globs, filepath.Clean(g))
		}
	}
	return f
}

// match returns the guarded file p names, "" for none: p itself, a file a
// glob in p expands to, or what a symlink at p points to.
func (f secretFiles) match(p string) string {
	if p == "" {
		return ""
	}
	cands := []string{p}
	if strings.ContainsAny(p, "*?[") {
		m, _ := filepath.Glob(p)
		cands = append(cands, m...)
	}
	for _, c := range cands {
		real, err := filepath.EvalSymlinks(c)
		for _, x := range []string{c, real} {
			if x == "" || err != nil && x == real {
				continue
			}
			for _, g := range f.globs {
				if ok, _ := filepath.Match(g, x); ok {
					return x
				}
			}
		}
	}
	return ""
}

// under returns a guarded file below the directory dir, "" for none; dir
// counts only below the home directory, so a search of the whole home or
// the root is no read of its dot-directories.
func (f secretFiles) under(dir string) string {
	if f.home == "" || !strings.HasPrefix(dir, f.home+string(filepath.Separator)) {
		return ""
	}
	for _, g := range f.globs {
		if strings.HasPrefix(g, dir+string(filepath.Separator)) {
			if m, _ := filepath.Glob(g); len(m) > 0 {
				return m[0]
			}
			if !strings.ContainsAny(g, "*?[") {
				return g
			}
		}
	}
	return ""
}

// wordPath is the path a shell word names, relative to dir: quotes gone,
// ~/ and $HOME expanded, a redirection's < stripped; "" for no path.
func wordPath(w, dir string) string {
	w = strings.TrimPrefix(w, "<")
	for _, h := range []string{"$HOME", "${HOME}"} {
		if rest, ok := strings.CutPrefix(w, h); ok && (rest == "" || rest[0] == '/') {
			w = "~" + rest
		}
	}
	w = expandHome(w)
	if w == "" || strings.HasPrefix(w, "-") || strings.Contains(w, "://") {
		return ""
	}
	if !filepath.IsAbs(w) {
		if dir == "" {
			return ""
		}
		w = filepath.Join(dir, w)
	}
	return filepath.Clean(w)
}

// fileLeak returns the leak of a simple command that reads a guarded file
// whole: one that names it and is not a metadata command, a grep that
// prints only counts or names, or a jq or yq read that strips it.
func (g secretGuard) fileLeak(words []string, dirs []string) *leak {
	k := commandAt(words)
	if k == len(words) || len(g.files.globs) == 0 {
		return nil
	}
	name, args := path.Base(words[k]), words[k+1:]
	if metaCmds[name] {
		return nil
	}
	isGrep := grepCmds[name]
	recursive := name == "rg" || name == "ag" || isGrep && slices.ContainsFunc(args, grepRecursive.MatchString)
	file := ""
	for i, w := range words {
		if i > 0 && (words[i-1] == ">" || words[i-1] == ">>" || words[i-1] == ">|") || strings.HasPrefix(w, ">") {
			continue // a write target, no read
		}
		if _, v, ok := strings.Cut(w, "="); ok {
			w = v // --config=<file>, F=<file>
		}
		for _, d := range dirs {
			p := wordPath(w, d)
			if file = g.files.match(p); file == "" && recursive {
				file = g.files.under(p)
			}
			if file != "" {
				break
			}
		}
		if file != "" {
			break
		}
	}
	if file == "" {
		return nil
	}
	switch {
	case isGrep && slices.ContainsFunc(args, grepQuiet.MatchString):
		return nil
	case name == "jq" || name == "gojq" || name == "yq":
		if filter, ok := jqFilter(args); ok && (filterSafe(filter) || strips(filter, file)) {
			return nil
		}
	}
	return &leak{what: "a read of " + file + ", a file that holds secret values", safe: fileReadSafe(file), file: file}
}

// strips reports whether a jq filter deletes, wherever they are, every
// credential key file holds, and reads nothing again after. A file that is
// no YAML or JSON has no key-stripped read.
func strips(filter, file string) bool {
	m := stripFilter.FindStringSubmatch(filter)
	if m == nil || reread.MatchString(m[2]) {
		return false
	}
	deleted := map[string]bool{}
	for ref := range strings.SplitSeq(m[1], ",") {
		r := keyRef.FindStringSubmatch(strings.TrimSpace(ref))
		if r == nil {
			return false
		}
		deleted[r[1]+r[2]+r[3]] = true
	}
	keys, ok := credentialKeys(file)
	if !ok {
		return false
	}
	for _, k := range keys {
		if !deleted[k] {
			return false
		}
	}
	return true
}

// credentialKeys are the mapping keys of file whose names say they hold a
// credential, sorted; false when file is no YAML or JSON document. Only
// keys are kept, never a value.
func credentialKeys(file string) ([]string, bool) {
	raw, err := os.ReadFile(file) //nolint:gosec // a guarded file, parsed for its key names
	if err != nil {
		return nil, os.IsNotExist(err)
	}
	var doc yaml.Node
	if yaml.Unmarshal(raw, &doc) != nil || doc.Kind != yaml.DocumentNode {
		return nil, false
	}
	seen := map[string]bool{}
	var walk func(n *yaml.Node)
	walk = func(n *yaml.Node) {
		if n.Kind == yaml.MappingNode {
			for i := 0; i+1 < len(n.Content); i += 2 {
				if k := n.Content[i].Value; credentialKey.MatchString(k) {
					seen[k] = true
				}
			}
		}
		for _, c := range n.Content {
			walk(c)
		}
	}
	walk(&doc)
	if len(doc.Content) == 0 || doc.Content[0].Kind == yaml.ScalarNode {
		return nil, false // a bare value, no document of keys
	}
	keys := make([]string, 0, len(seen))
	for k := range seen {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys, true
}

// fileReadSafe names the reads of file that pass, the strip filter built
// from the credential keys the file holds.
func fileReadSafe(file string) string {
	keys, ok := credentialKeys(file)
	if !ok {
		return "  " + file + " is no YAML or JSON file: no read of it passes. Its metadata does (ls -l, stat, wc -c), " +
			"and `beekeeper secret` handles the value."
	}
	refs := make([]string, 0, len(keys))
	for _, k := range keys {
		if plainKey.MatchString(k) {
			refs = append(refs, "."+k+"?")
		} else {
			refs = append(refs, `."`+k+`"?`)
		}
	}
	strip := "  yq 'keys' " + file + "   (the top-level key names)\n"
	if len(refs) > 0 {
		strip = fmt.Sprintf("  yq -y 'del(.. | %s)' %s   (the file with every credential key stripped)\n", strings.Join(refs, ", "), file) + strip
	}
	return strip + "  grep -c|-l <pattern> " + file + "   (counts or names, no lines)\n" +
		"  The value itself only ever moves through `beekeeper secret`."
}

// secretFileRefusal refuses a Read of a guarded file, or a Grep that prints
// lines of one.
func (h Hook) secretFileRefusal(ev event) string {
	f := newSecretFiles(h.SecretFiles)
	cwd := ev.CWD
	file := ""
	switch ev.ToolName {
	case readTool:
		p, _ := ev.ToolInput[filePathKey].(string)
		file = f.match(wordPath(p, cwd))
	case grepTool:
		if mode, _ := ev.ToolInput[outputModeKey].(string); mode != contentKey {
			return ""
		}
		p, _ := ev.ToolInput[pathKey].(string)
		if p == "" {
			p = cwd
		}
		p = wordPath(p, cwd)
		if file = f.match(p); file == "" {
			file = f.under(p)
			if glob, _ := ev.ToolInput["glob"].(string); file != "" && glob != "" {
				if ok, _ := filepath.Match(glob, filepath.Base(file)); !ok {
					file = ""
				}
			}
		}
	default:
		return ""
	}
	if file == "" {
		return ""
	}
	return leak{what: ev.ToolName + " of " + file + ", a file that holds secret values", at: ev.ToolName, safe: fileReadSafe(file)}.reason()
}
