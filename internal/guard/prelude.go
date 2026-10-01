package guard

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
)

// preludeBegin and preludeEnd enclose beekeeper's prelude in a session's
// environment file, which other hooks may write to as well.
const (
	preludeBegin = "# beekeeper: agent shell prelude"
	preludeEnd   = "# beekeeper: end of agent shell prelude"
)

// Prelude is the shell code an agent shell runs before each command, in zsh
// and bash alike: it removes the aliases and shell functions of unalias, so
// each name runs the tool on PATH, and with literalGlobs an unmatched glob
// stays as written instead of failing the command. Claude Code sources it
// before parsing the command, so a removed alias is not expanded in it.
// Every line ends with exit status 0. Empty when there is nothing to do.
func Prelude(unalias []string, literalGlobs bool) string {
	if len(unalias) == 0 && !literalGlobs {
		return ""
	}
	var b strings.Builder
	b.WriteString(preludeBegin + "\n")
	if len(unalias) > 0 {
		fmt.Fprintf(&b, "for _bk_c in %s; do unalias \"$_bk_c\" 2>/dev/null; unset -f \"$_bk_c\" 2>/dev/null; done; unset _bk_c\n",
			strings.Join(unalias, " "))
	}
	if literalGlobs {
		b.WriteString(`if [ -n "${ZSH_VERSION-}" ]; then setopt no_nomatch; elif [ -n "${BASH_VERSION-}" ]; then shopt -u failglob nullglob; fi` + "\n")
	}
	b.WriteString(preludeEnd + "\n")
	return b.String()
}

// WithPrelude is env, a session's environment file, with prelude in place
// of the one an earlier start wrote; the rest of the file stays.
func WithPrelude(env, prelude string) string {
	if i := strings.Index(env, preludeBegin+"\n"); i >= 0 {
		if j := strings.Index(env[i:], preludeEnd+"\n"); j >= 0 {
			env = env[:i] + env[i+j+len(preludeEnd)+1:]
		}
	}
	if prelude == "" {
		return env
	}
	if env != "" && !strings.HasSuffix(env, "\n") {
		env += "\n"
	}
	return env + prelude
}

// WritePrelude puts prelude into the environment file at path, written
// only when it changes.
func WritePrelude(path, prelude string) error {
	raw, err := os.ReadFile(path) //nolint:gosec // the session's environment file Claude Code names
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	next := WithPrelude(string(raw), prelude)
	if next == string(raw) {
		return nil
	}
	return os.WriteFile(path, []byte(next), 0o600) //nolint:gosec // the session's environment file Claude Code names
}
