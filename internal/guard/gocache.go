package guard

import (
	"os"
	"path"
	"strings"
)

// The Go cache guard refuses a Bash command that clears the Go caches the
// machine's builds share: go clean -cache or -modcache, or a delete of
// either cache's directory, of every entry in it or of a directory that
// holds it. A build that runs meanwhile fails on the entries it loses;
// beekeeper cache clear go waits for the runs first. A go clean whose
// GOCACHE or GOMODCACHE assignment points at another cache passes, and so
// does a delete inside a cache (one module, one entry).

// goCacheFix is what every refusal offers instead.
const goCacheFix = "A build running meanwhile fails on the entries it loses. Run `beekeeper cache clear go` " +
	"(`--modcache` for the module cache too), which waits for the beekeeper runs that hold the cache and holds back new ones."

// goCacheRefusal returns why cmd clears a shared Go cache, "" when it does
// not.
func (h Hook) goCacheRefusal(cmd string) string {
	if h.GoCache == "" && h.GoModCache == "" {
		return ""
	}
	return h.scanGoCaches(cmd, 0)
}

func (h Hook) scanGoCaches(cmd string, depth int) string {
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
			continue
		}
		name := path.Base(words[k])
		if shells[name] {
			for _, a := range words[k+1:] {
				if strings.ContainsAny(a, " \t\n;|&") {
					if r := h.scanGoCaches(a, depth+1); r != "" {
						return r
					}
				}
			}
		}
		at := strings.Join(raw[k:], " ")
		if name == "go" {
			if what := h.goClean(words[:k], words[k+1:]); what != "" {
				return "Refused: `" + at + "` clears " + what + ". " + goCacheFix
			}
		}
		for _, t := range deleteTargets(name, raw[k+1:]) {
			if what := h.cacheUnder(strings.Join(shellWords(t), "")); what != "" {
				return "Refused: `" + at + "` deletes " + what + ". " + goCacheFix
			}
		}
	}
	return ""
}

// goClean returns the shared cache a go command clears: go clean with
// -cache or -modcache, unless an assignment before it (env) points the
// cache elsewhere.
func (h Hook) goClean(env, args []string) string {
	if len(args) == 0 || args[0] != "clean" {
		return ""
	}
	for _, a := range args[1:] {
		flag, val, _ := strings.Cut(strings.TrimPrefix(a, "-"), "=")
		if !strings.HasPrefix(a, "-") || val == falseWord {
			continue
		}
		switch flag {
		case "-cache", "cache":
			if h.shared(env, "GOCACHE", h.GoCache) {
				return "the Go build cache " + h.GoCache + " every session's builds share"
			}
		case "-modcache", "modcache":
			if h.shared(env, "GOMODCACHE", h.GoModCache) {
				return "the Go module cache " + h.GoModCache + " every session's builds share"
			}
		}
	}
	return ""
}

// shared says the command's assignments (env) leave key at dir, the shared
// cache: none sets it, or one sets it to dir.
func (h Hook) shared(env []string, key, dir string) bool {
	if dir == "" {
		return false
	}
	for _, w := range env {
		if v, ok := strings.CutPrefix(w, key+"="); ok {
			return h.resolve(v) == dir
		}
	}
	return true
}

// cacheUnder returns the shared cache a delete of target removes: the
// target is the cache, every entry in it, or a directory that holds it.
func (h Hook) cacheUnder(target string) string {
	t := strings.TrimSuffix(strings.TrimSuffix(h.resolve(target), "/*"), "/")
	if t == "" || !strings.HasPrefix(t, "/") {
		return ""
	}
	t = path.Clean(t)
	for _, c := range []struct{ dir, what string }{{h.GoCache, "the Go build cache "}, {h.GoModCache, "the Go module cache "}} {
		if c.dir == "" {
			continue
		}
		for d := path.Clean(c.dir); d != "/" && d != "."; d = path.Dir(d) {
			if ok, _ := path.Match(t, d); ok || t == d {
				return c.what + c.dir + " every session's builds share"
			}
		}
	}
	return ""
}

// resolve expands what names the home directory or a Go cache at the start
// of a word: ~, $HOME, $GOCACHE, $GOMODCACHE and go env's answers for them.
func (h Hook) resolve(w string) string {
	home, _ := os.UserHomeDir()
	for _, p := range []struct {
		names []string
		to    string
	}{
		{[]string{"~", "$HOME", "${HOME}"}, home},
		{[]string{"$GOCACHE", "${GOCACHE}", "$(go env GOCACHE)", "`go env GOCACHE`"}, h.GoCache},
		{[]string{"$GOMODCACHE", "${GOMODCACHE}", "$(go env GOMODCACHE)", "`go env GOMODCACHE`"}, h.GoModCache},
	} {
		for _, n := range p.names {
			if rest, ok := strings.CutPrefix(w, n); ok && p.to != "" && (rest == "" || rest[0] == '/') {
				return p.to + rest
			}
		}
	}
	return w
}
