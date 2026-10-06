package guard

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"regexp"
	"strings"

	"github.com/giantswarm/beekeeper/internal/sandbox"
)

// vaultEnv matches the names of the variables that carry a 1Password
// session or token: a signed-in CLI session, a service account's token, a
// Connect server's token.
const vaultEnv = "OP_SESSION_[A-Za-z0-9_]+|OP_SERVICE_ACCOUNT_TOKEN|OP_CONNECT_TOKEN"

// VaultVar matches the name of a variable that carries a vault credential.
var VaultVar = regexp.MustCompile("^(?:" + vaultEnv + ")$")

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
// The directories of path go first on PATH, in their order, each once (a
// leading ~/ is the home directory): an agent's own programs, such as a gh
// that acts with a short-lived token, shadow the person's. The directories
// of drop leave PATH wherever they are, one an inherited PATH carries
// included: in the agent sandbox an agent's gh link to devctl would read a
// keychain the sandbox closes.
// With unheld, the shell picks per command: a command the sandbox runtime
// holds ($SANDBOX_RUNTIME) leaves drop, any other runs unheld, the shell
// lines that put an unheld environment back on the host, and puts path
// first: Claude Code can apply the sandbox policy's environment without
// its sandbox.
// It always drops the vault credentials of vaultEnv from the environment:
// no agent command runs with a vault session or token.
// Every line ends with exit status 0.
func Prelude(unalias []string, literalGlobs bool, path, drop []string, unheld string) string {
	var b strings.Builder
	b.WriteString(preludeBegin + "\n")
	fmt.Fprintf(&b, "for _bk_v in $(env | sed -nE 's/^(%s)=.*/\\1/p'); do unset \"$_bk_v\"; done; unset _bk_v\n", vaultEnv)
	if len(unalias) > 0 {
		fmt.Fprintf(&b, "for _bk_c in %s; do unalias \"$_bk_c\" 2>/dev/null; unset -f \"$_bk_c\" 2>/dev/null; done; unset _bk_c\n",
			strings.Join(unalias, " "))
	}
	if literalGlobs {
		b.WriteString(`if [ -n "${ZSH_VERSION-}" ]; then setopt no_nomatch; elif [ -n "${BASH_VERSION-}" ]; then shopt -u failglob nullglob; fi` + "\n")
	}
	if unheld != "" {
		b.WriteString(`if [ -z "${` + sandbox.Runtime + `-}" ]; then` + "\n" + unheld)
		writePath(&b, path)
		b.WriteString("else\n")
		writeDrop(&b, drop)
		b.WriteString("fi\n")
	} else {
		writeDrop(&b, drop)
		writePath(&b, path)
	}
	b.WriteString(preludeEnd + "\n")
	return b.String()
}

// writeDrop removes the directories of drop from PATH.
func writeDrop(b *strings.Builder, drop []string) {
	for _, d := range drop {
		dir := strings.ReplaceAll(expandHome(d), "'", `'\''`)
		fmt.Fprintf(b, "case \":$PATH:\" in *':%[1]s:'*) _bk_p=$(printf %%s \"$PATH\" | awk -v RS=: -v ORS=: -v d='%[1]s' '$0 != d'); PATH=${_bk_p%%:}; export PATH; unset _bk_p ;; esac\n", dir)
	}
}

// writePath puts the directories of path first on PATH, in their order.
func writePath(b *strings.Builder, path []string) {
	for i := len(path) - 1; i >= 0; i-- {
		dir := strings.ReplaceAll(expandHome(path[i]), "'", `'\''`)
		fmt.Fprintf(b, "case \":$PATH:\" in *':%[1]s:'*) ;; *) PATH='%[1]s':\"$PATH\"; export PATH ;; esac\n", dir)
	}
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
