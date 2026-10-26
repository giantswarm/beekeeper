package secret

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"

	"github.com/giantswarm/beekeeper/internal/guard"
)

// SSHRef starts a remote file target: ssh://[<user>@]<host>[:<port>]/<path>.
const SSHRef = "ssh://"

// TemplateValue is the placeholder of a --template the value replaces.
const TemplateValue = "{{value}}"

// RemoteFile is a file on a host reached by ssh that one value is written
// into: its path, mode and owner, and whether the write runs under sudo.
type RemoteFile struct {
	User string
	Host string
	Port string
	Path string
	// Mode is the file's octal mode, 0600 when empty.
	Mode string
	// Owner is user[:group]; empty keeps the ssh user's.
	Owner string
	// Sudo runs the write under sudo -n, by the host's own sudo rules.
	Sudo bool
	// Template is the file's content with the value at TemplateValue;
	// empty writes the value alone.
	Template string
}

var (
	sshHost  = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]*$`)
	sshUser  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.-]*$`)
	fileMode = regexp.MustCompile(`^0?[0-7]{3,4}$`)
	owner    = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.-]*(?::[A-Za-z_][A-Za-z0-9_.-]*)?$|^\d+(?::\d+)?$`)
	safePath = regexp.MustCompile(`^/[A-Za-z0-9_.@%+=:,/-]+$`)
)

// IsSSH reports whether s names a remote file, ssh://….
func IsSSH(s string) bool { return strings.HasPrefix(s, SSHRef) }

// ParseRemoteFile reads ssh://[<user>@]<host>[:<port>]/<absolute path>: a
// host that is no ssh option and an absolute, clean path of plain
// characters, so nothing in either reaches the remote shell but a file
// name.
func ParseRemoteFile(s string) (RemoteFile, error) {
	if !IsSSH(s) {
		return RemoteFile{}, fmt.Errorf("%s: a remote file is ssh://[<user>@]<host>[:<port>]/<path>", s)
	}
	u, err := url.Parse(s)
	if err != nil || u.Opaque != "" || u.RawQuery != "" || u.Fragment != "" {
		return RemoteFile{}, fmt.Errorf("%s: a remote file is ssh://[<user>@]<host>[:<port>]/<path>", s)
	}
	f := RemoteFile{Host: u.Hostname(), Port: u.Port(), Path: u.Path}
	if u.User != nil {
		if _, ok := u.User.Password(); ok {
			return RemoteFile{}, fmt.Errorf("%s: no password in a remote file's URL, ssh authenticates by its keys", s)
		}
		f.User = u.User.Username()
		if !sshUser.MatchString(f.User) {
			return RemoteFile{}, fmt.Errorf("%s: user %q: letters, digits, _ . -", s, f.User)
		}
	}
	switch {
	case !sshHost.MatchString(f.Host) && net.ParseIP(f.Host) == nil:
		return RemoteFile{}, fmt.Errorf("%s: host %q: a name or an IPv4 address that starts with a letter or digit, or an IPv6 address in brackets", s, f.Host)
	case f.Port != "":
		if p, err := strconv.Atoi(f.Port); err != nil || p < 1 || p > 65535 {
			return RemoteFile{}, fmt.Errorf("%s: port %q", s, f.Port)
		}
	}
	if !safePath.MatchString(f.Path) || path.Clean(f.Path) != f.Path || f.Path == "/" {
		return RemoteFile{}, fmt.Errorf("%s: the path is an absolute, clean file path of letters, digits and _ . @ %% + = : , / -", s)
	}
	return f, nil
}

func (f RemoteFile) String() string {
	host := f.Host
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	if f.User != "" {
		host = f.User + "@" + host
	}
	if f.Port != "" {
		host += ":" + f.Port
	}
	return SSHRef + host + f.Path
}

// mode is the file's mode, 0600 by default.
func (f RemoteFile) mode() string {
	if f.Mode == "" {
		return "0600"
	}
	return f.Mode
}

// Check refuses a mode, an owner or a template beekeeper would not write:
// every check runs before the value is read.
func (f RemoteFile) Check() error {
	if !fileMode.MatchString(f.mode()) {
		return fmt.Errorf("--mode %q: an octal mode, 0600 for one", f.Mode)
	}
	if f.Owner != "" && !owner.MatchString(f.Owner) {
		return fmt.Errorf("--owner %q: user[:group], names or numeric ids", f.Owner)
	}
	if f.Template != "" {
		if n := strings.Count(f.Template, TemplateValue); n != 1 {
			return fmt.Errorf("--template holds %s %d times: once, where the value goes", TemplateValue, n)
		}
		if shapes := guard.CredentialShapes(strings.Replace(f.Template, TemplateValue, "", 1)); shapes != nil {
			return fmt.Errorf("%w: the template has the shape of a credential (%s); it carries public lines, the secret goes by its reference", ErrCredentialShape, strings.Join(shapes, ", "))
		}
	}
	return nil
}

// content is what the file holds for v: the template with v in it, else
// v alone.
func (f RemoteFile) content(v string) string {
	if f.Template == "" {
		return v
	}
	return strings.Replace(f.Template, TemplateValue, v, 1)
}

// Argv is the ssh command line that writes n bytes read on its stdin into
// f: no TTY, no prompt, the host after "--", and the remote command a fixed
// script with every argument quoted. The value is never in it.
func (f RemoteFile) Argv(n int) []string {
	argv := []string{"ssh", "-T", "-o", "BatchMode=yes"}
	if f.Port != "" {
		argv = append(argv, "-p", f.Port)
	}
	if f.User != "" {
		argv = append(argv, "-l", f.User)
	}
	return append(argv, "--", f.Host, f.Script(n))
}

// Script is the remote command that writes n bytes from stdin into f: a
// temp file next to the target under umask 077, its length checked against
// n (a dropped connection ends the stream early), owner then mode set (chown
// clears setuid and setgid, so chmod comes last), then moved into place. A
// target that is a directory is refused before the write and again before
// the move, which would put the file inside it. A failure or a signal
// removes the temp file and leaves the target as it was; on success it
// prints nothing.
//
// The login shell of the ssh user parses the command before sh runs it:
// "sh -c '<script>'" with the script holding no backslash, newline or "!",
// and its own quotes outside the quoted parts ('\''), reads the same in a
// POSIX shell, fish and csh.
func (f RemoteFile) Script(n int) string {
	dir, base := path.Split(f.Path)
	chown := ""
	if f.Owner != "" {
		chown = "chown " + shellQuote(f.Owner) + ` "$t"; `
	}
	notDir := "if [ -d " + shellQuote(f.Path) + ` ]; then echo "the target is a directory: nothing changed" >&2; exit 1; fi; `
	s := "set -eu; umask 077; " + notDir +
		"t=$(mktemp " + shellQuote(dir+"."+base+".beekeeper.XXXXXX") + "); " +
		`trap 'rm -f -- "$t"' EXIT; trap 'exit 1' HUP INT TERM PIPE; ` +
		`cat >"$t"; ` +
		`[ "$(wc -c <"$t" | tr -d " ")" = ` + strconv.Itoa(n) + ` ] || { echo "short stream: nothing changed" >&2; exit 1; }; ` +
		chown + "chmod " + f.mode() + ` "$t"; ` + notDir +
		`mv -f -- "$t" ` + shellQuote(f.Path) + "; trap - EXIT"
	sh := "sh -c " + shellQuote(s)
	if f.Sudo {
		return "sudo -n " + sh
	}
	return sh
}

// RemoteResult is what a remote write answers: where, how and how much,
// and the value's keyed fingerprint, never the value.
type RemoteResult struct {
	File        string `json:"file"`
	Mode        string `json:"mode"`
	Owner       string `json:"owner,omitempty"`
	Bytes       int    `json:"bytes"`
	Length      int    `json:"length"`
	Fingerprint string `json:"fingerprint"`
}

// ErrEmptyValue refuses to write an empty or blank value, which a broken
// source answers more often than a credential holds.
var ErrEmptyValue = errors.New("the value is empty or blank: nothing written")

// CopyToRemote writes one value into a file on a host over ssh, the value
// (in o.Encode, inside f's template) on ssh's stdin alone. Every check runs
// before the value is read; a source that resolves to nothing fails before
// ssh runs.
func (o *Ops) CopyToRemote(ctx context.Context, src Ref, f RemoteFile) (RemoteResult, error) {
	if !src.Single() {
		return RemoteResult{}, fmt.Errorf("%s: a remote file takes one value: file#path, op://…, k8s://…, file://…#path or cmd://…", src)
	}
	if err := f.Check(); err != nil {
		return RemoteResult{}, err
	}
	v, err := o.encoded(ctx, src)
	if err != nil {
		return RemoteResult{}, err
	}
	if strings.TrimSpace(v) == "" {
		return RemoteResult{}, fmt.Errorf("%s: %w", src, ErrEmptyValue)
	}
	body := f.content(v)
	argv := f.Argv(len(body))
	if _, err := o.Run(ctx, "", nil, strings.NewReader(body), argv[0], argv[1:]...); err != nil {
		return RemoteResult{}, fmt.Errorf("%s: not written, the target kept: %s", f, redact(err.Error(), v, src.String()))
	}
	res := RemoteResult{File: f.String(), Mode: f.mode(), Owner: f.Owner, Bytes: len(body), Length: len(v)}
	if o.Fingerprint != nil {
		res.Fingerprint = o.Fingerprint(v)
	}
	return res, nil
}
