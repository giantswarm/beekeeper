// Package secret is beekeeper secret: the credential operations beekeeper
// runs in its own process so that no agent reads a value. sops and op run
// here, the values they answer stay in memory for the one operation, and
// what an operation returns is key names, lengths, equality and keyed
// fingerprints, never a value. Nothing is written in plaintext: a SOPS file
// is written encrypted, from sops' own output.
package secret

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/giantswarm/beekeeper/internal/guard"
)

// Runner runs a credential tool in dir with stdin, its environment plus
// env, and returns its stdout. Its error must carry nothing the tool
// printed but a redacted first line.
type Runner func(ctx context.Context, dir string, env []string, stdin io.Reader, name string, args ...string) ([]byte, error)

// Ops are the operations over one configuration.
type Ops struct {
	Run Runner
	// Vault is the shared 1Password vault, the only one an op:// reference
	// may name; empty refuses every op:// reference.
	Vault string
	// Token is the vault's service account token, which only op's own
	// environment gets; empty refuses every op:// reference.
	Token string
	// Session reads the vault through the person's own signed-in op
	// session (OP_SESSION_* in the caller's environment) instead of a
	// service account: the way to a vault no service account can be
	// granted, such as a person's Employee vault.
	Session bool
	// Fingerprint is the keyed hash fingerprint answers with.
	Fingerprint func(value string) string
	// Apply writes a key of a Secret; nil is [ApplySecret].
	Apply SecretApplier
	// Ages are the age identities of the shared vault for the SOPS files
	// sops' own sources hold none for.
	Ages []AgeIdentity
}

// opTimeout bounds one read of the shared vault: op that answers nothing
// in time fails like a locked vault, never hangs the caller.
const opTimeout = time.Minute

// Exec is the Runner of the real tools.
func Exec(ctx context.Context, dir string, env []string, stdin io.Reader, name string, args ...string) ([]byte, error) {
	c := exec.CommandContext(ctx, name, args...) //nolint:gosec // sops and op, beekeeper's own calls
	c.Dir = dir
	if env != nil {
		c.Env = append(os.Environ(), env...)
	}
	c.Stdin = stdin
	var stderr bytes.Buffer
	c.Stderr = &stderr
	out, err := c.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return nil, fmt.Errorf("%s: exit %d (%s)", name, ee.ExitCode(), guard.FirstLine(stderr.String()))
		}
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	return out, nil
}

// Ref names a value or a set of values: an op:// field of the shared
// vault, a SOPS file, or one path in a SOPS file (file#a.b.c, sops:// in
// front optional).
type Ref struct {
	Op   string
	File string
	Path string
}

// ParseRef reads a reference.
func ParseRef(s string) (Ref, error) {
	if strings.HasPrefix(s, guard.OpRef) {
		parts := strings.Split(strings.TrimPrefix(s, guard.OpRef), "/")
		if len(parts) < 3 || slicesHasEmpty(parts) {
			return Ref{}, fmt.Errorf("%q: an op reference is op://<vault>/<item>/<field>", s)
		}
		return Ref{Op: s}, nil
	}
	s = strings.TrimPrefix(s, guard.SOPSRef)
	file, path, _ := strings.Cut(s, "#")
	if file == "" {
		return Ref{}, fmt.Errorf("%q: no file", s)
	}
	return Ref{File: file, Path: path}, nil
}

func slicesHasEmpty(ss []string) bool {
	for _, s := range ss {
		if s == "" {
			return true
		}
	}
	return false
}

// String is the reference as a log and an answer name it.
func (r Ref) String() string {
	switch {
	case r.Op != "":
		return r.Op
	case r.Path != "":
		return r.File + "#" + r.Path
	}
	return r.File
}

// single is whether the reference names one value.
func (r Ref) single() bool { return r.Op != "" || r.Path != "" }

// vault is the vault an op:// reference names.
func (r Ref) vault() string {
	v, _, _ := strings.Cut(strings.TrimPrefix(r.Op, guard.OpRef), "/")
	return v
}

// checkVault refuses an op:// reference outside the shared vault, with
// [ErrVault] when the vault is not configured.
func (o *Ops) checkVault(r Ref) error {
	switch {
	case r.Op == "":
		return nil
	case o.Vault == "":
		return fmt.Errorf("%w: %s: no shared vault is configured (secret.vault): beekeeper reads no op:// reference", ErrVault, r.Op)
	case o.Token == "" && !o.Session:
		return fmt.Errorf("%w: %s: no service account token (secret.tokenFile): beekeeper reads the shared vault only through its own service account", ErrVault, r.Op)
	case r.vault() != o.Vault:
		return fmt.Errorf("%s: beekeeper reads only the shared vault %q", r.Op, o.Vault)
	}
	return nil
}

// values reads what r names: one value under its own name for a single
// reference, every leaf under its dotted path for a whole file.
func (o *Ops) values(ctx context.Context, r Ref) (map[string]string, error) {
	if err := o.checkVault(r); err != nil {
		return nil, err
	}
	if r.Op != "" {
		ctx, cancel := context.WithTimeout(ctx, opTimeout)
		defer cancel()
		out, err := o.op(ctx, nil, "read", "--no-newline", r.Op)
		if ctx.Err() != nil {
			return nil, fmt.Errorf("%w: %s: op answered nothing in %s", ErrVault, r.Op, opTimeout)
		}
		if err != nil {
			return nil, fmt.Errorf("%w: %s: %w", ErrVault, r.Op, err)
		}
		return map[string]string{r.Op: string(out)}, nil
	}
	doc, err := o.decrypt(ctx, r.File)
	if err != nil {
		return nil, err
	}
	if r.Path == "" {
		return doc.leaves(), nil
	}
	v, ok := doc.get(r.Path)
	if !ok {
		return nil, fmt.Errorf("%s: no value at %s", r.File, r.Path)
	}
	return map[string]string{r.String(): v}, nil
}

// value reads the one value a single reference names.
func (o *Ops) value(ctx context.Context, r Ref) (string, error) {
	if !r.single() {
		return "", fmt.Errorf("%s: name one value (file#path or op://…), not a whole file", r)
	}
	vs, err := o.values(ctx, r)
	if err != nil {
		return "", err
	}
	return vs[r.String()], nil
}

// op runs op as the service account, or as the person in session mode.
func (o *Ops) op(ctx context.Context, stdin io.Reader, args ...string) ([]byte, error) {
	if o.Session {
		return o.asPerson(ctx, stdin, args...)
	}
	return o.Run(ctx, "", []string{"OP_SERVICE_ACCOUNT_TOKEN=" + o.Token}, stdin, "op", args...)
}

// decrypt reads a SOPS file into its document.
func (o *Ops) decrypt(ctx context.Context, file string) (*document, error) {
	env, err := o.ageEnv(ctx, file)
	if err != nil {
		return nil, err
	}
	out, err := o.Run(ctx, "", env, nil, "sops", "decrypt", "--output-type", "yaml", file)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", file, err)
	}
	doc, err := parseDocument(out)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", file, err)
	}
	return doc, nil
}

// encrypt writes doc to file encrypted under the creation rules of the
// .sops.yaml nearest above file. The plaintext goes to sops on stdin; the
// file is written from sops' output only, through a rename.
func (o *Ops) encrypt(ctx context.Context, doc *document, file string) error {
	abs, err := filepath.Abs(file)
	if err != nil {
		return err
	}
	cfg, err := sopsConfig(filepath.Dir(abs))
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(filepath.Dir(cfg), abs)
	if err != nil {
		return err
	}
	plain, err := doc.encode()
	if err != nil {
		return err
	}
	out, err := o.Run(ctx, filepath.Dir(cfg), nil, bytes.NewReader(plain), "sops", "--config", cfg,
		"encrypt", "--filename-override", rel, "--input-type", "yaml", "/dev/stdin")
	if err != nil {
		return fmt.Errorf("%s: %w", file, err)
	}
	return writeFile(abs, out)
}

// sopsConfig is the .sops.yaml in dir or the nearest directory above it.
func sopsConfig(dir string) (string, error) {
	for d := dir; ; d = filepath.Dir(d) {
		p := filepath.Join(d, ".sops.yaml")
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
		if filepath.Dir(d) == d {
			return "", fmt.Errorf("no .sops.yaml in %s or above: its creation rules name the recipients", dir)
		}
	}
}

// writeFile replaces path with data, keeping an existing file's mode.
func writeFile(path string, data []byte) error {
	mode := os.FileMode(0o644)
	if fi, err := os.Stat(path); err == nil {
		mode = fi.Mode().Perm()
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) //nolint:errcheck // gone after the rename
	if _, err := tmp.Write(data); err != nil {
		tmp.Close() //nolint:errcheck,gosec // the write error wins
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close() //nolint:errcheck,gosec // the chmod error wins
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
