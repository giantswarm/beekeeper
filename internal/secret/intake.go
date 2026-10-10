package secret

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/giantswarm/beekeeper/internal/guard"
)

// The person's store: a value a web page showed once, or a file it
// downloaded, into a field of the shared vault. The value is read into
// beekeeper's memory from the clipboard or the file and written through
// op's stdin; afterwards the clipboard is cleared and the file shredded.
// Nothing of it reaches a command line, a file or a log.

// MaxStoreBytes bounds a stored value: a private key or a certificate
// chain fits, a file that is no credential does not.
const MaxStoreBytes = 32 << 10

// StoreValue writes v into the vault field dst, the item or the field
// created when absent, and answers the field, the value's length and its
// fingerprint.
func (o *Ops) StoreValue(ctx context.Context, dst Ref, v string) (Stored, error) {
	if dst.Op == "" {
		return Stored{}, fmt.Errorf("%s: store writes a field of the shared vault, op://<vault>/<item>/<field>", dst)
	}
	if v == "" {
		return Stored{}, fmt.Errorf("%s: nothing to store, the value is empty", dst.Op)
	}
	if len(v) > MaxStoreBytes {
		return Stored{}, fmt.Errorf("%s: %d bytes is more than a field takes (%d)", dst.Op, len(v), MaxStoreBytes)
	}
	if err := o.checkVault(dst); err != nil {
		return Stored{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, 3*opTimeout)
	defer cancel()
	if err := o.storeVault(ctx, dst, v); err != nil {
		if ctx.Err() != nil {
			return Stored{}, fmt.Errorf("%w: %s: op answered nothing in %s", ErrVault, dst.Op, 3*opTimeout)
		}
		return Stored{}, err
	}
	return Stored{Ref: dst.Op, Bytes: len(v), Fingerprint: o.fingerprint(v)}, nil
}

// Writable refuses a field no store could write: outside the shared vault,
// or with no way into it ([ErrVault]).
func (o *Ops) Writable(dst Ref) error {
	if dst.Op == "" {
		return fmt.Errorf("%s: store writes a field of the shared vault, op://<vault>/<item>/<field>", dst)
	}
	return o.checkVault(dst)
}

// ItemField is one field of a store into a vault item: its label, its value
// and whether it is configuration, a plain string, rather than a secret,
// which is concealed.
type ItemField struct {
	Label string `json:"label"`
	Value string `json:"value"`
	Plain bool   `json:"plain,omitempty"`
}

// MaxItemBytes bounds the values of one store into an item together: a
// GitHub App's credentials, its private key included, fit many times over.
const MaxItemBytes = 4 * MaxStoreBytes

// ParseItem reads op://<vault>/<item>, an item of the shared vault.
func ParseItem(s string) (vault, item string, err error) {
	parts := strings.Split(strings.TrimPrefix(s, guard.OpRef), "/")
	if !strings.HasPrefix(s, guard.OpRef) || len(parts) != 2 || slicesHasEmpty(parts) {
		return "", "", fmt.Errorf("%q: an item is op://<vault>/<item>", s)
	}
	return parts[0], parts[1], nil
}

// StoreFields writes fields into the item titled item of vault in one op
// call, the item or a field created when absent, and answers each field's
// reference, length and fingerprint. Every field is checked before any is
// written: a label or value that is empty, a value or the item over its
// bound, a vault not the shared one.
func (o *Ops) StoreFields(ctx context.Context, vault, item string, fields []ItemField) ([]Stored, error) {
	ref := guard.OpRef + vault + "/" + item
	if len(fields) == 0 {
		return nil, fmt.Errorf("%s: nothing to store, no field", ref)
	}
	total := 0
	for _, f := range fields {
		switch {
		case f.Label == "" || strings.ContainsAny(f.Label, "/\n"):
			return nil, fmt.Errorf("%s: a field's label names it, not %q", ref, f.Label)
		case f.Value == "":
			return nil, fmt.Errorf("%s/%s: nothing to store, the value is empty", ref, f.Label)
		case len(f.Value) > MaxStoreBytes:
			return nil, fmt.Errorf("%s/%s: %d bytes is more than a field takes (%d)", ref, f.Label, len(f.Value), MaxStoreBytes)
		}
		total += len(f.Value)
	}
	if total > MaxItemBytes {
		return nil, fmt.Errorf("%s: %d bytes is more than one store into an item takes (%d)", ref, total, MaxItemBytes)
	}
	if err := o.checkVault(Ref{Op: ref + "/" + fields[0].Label}); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 3*opTimeout)
	defer cancel()
	if err := o.storeVaultFields(ctx, vault, item, ref, fields); err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("%w: %s: op answered nothing in %s", ErrVault, ref, 3*opTimeout)
		}
		return nil, err
	}
	out := make([]Stored, 0, len(fields))
	for _, f := range fields {
		out = append(out, Stored{Ref: ref + "/" + f.Label, Bytes: len(f.Value), Fingerprint: o.fingerprint(f.Value)})
	}
	return out, nil
}

// fingerprint is o.Fingerprint of v, "" without a key.
func (o *Ops) fingerprint(v string) string {
	if o.Fingerprint == nil {
		return ""
	}
	return o.Fingerprint(v)
}

// Clipboard reads the Wayland clipboard through wl-paste, trimmed of the
// whitespace a selection drags along; "" when nothing is copied.
func Clipboard(ctx context.Context, run Runner) (string, error) {
	out, err := run(ctx, "", nil, nil, "wl-paste", "--no-newline")
	if err != nil {
		// exit 1: "Nothing is copied"
		if strings.Contains(err.Error(), "exit 1 (") {
			return "", nil
		}
		return "", fmt.Errorf("the clipboard: %w (wl-clipboard reads the Wayland clipboard)", err)
	}
	return strings.TrimSpace(string(out)), nil
}

// ClearClipboard empties the Wayland clipboard (wl-copy --clear), so that a
// stored value does not wait there for the next paste.
func ClearClipboard(ctx context.Context, run Runner) error {
	if _, err := run(ctx, "", nil, nil, "wl-copy", "--clear"); err != nil {
		return fmt.Errorf("the clipboard still holds the value: %w", err)
	}
	return nil
}

// ReadValueFile reads a file holding one value, of at most MaxStoreBytes.
func ReadValueFile(path string) (string, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if fi.Size() > MaxStoreBytes {
		return "", fmt.Errorf("%s: %d bytes is more than a field takes (%d): no credential file is that large", path, fi.Size(), MaxStoreBytes)
	}
	raw, err := os.ReadFile(path) //nolint:gosec // the file the person named
	if err != nil {
		return "", err
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return "", fmt.Errorf("%s: the file is empty", path)
	}
	return string(raw), nil
}

// NewestFile is the newest regular file in dir whose name matches the glob
// pattern, "" when none.
func NewestFile(dir, pattern string) (string, error) {
	matches, err := filepath.Glob(filepath.Join(dir, pattern))
	if err != nil {
		return "", err
	}
	var newest string
	var at time.Time
	for _, m := range matches {
		fi, err := os.Stat(m)
		if err != nil || !fi.Mode().IsRegular() {
			continue
		}
		if newest == "" || fi.ModTime().After(at) {
			newest, at = m, fi.ModTime()
		}
	}
	return newest, nil
}

// Shred overwrites path with random bytes and removes it: a downloaded key
// file leaves no plaintext behind once the vault holds it.
func Shred(path string) error {
	f, err := os.OpenFile(path, os.O_WRONLY, 0) //nolint:gosec // the person's own download
	if err != nil {
		return err
	}
	fi, err := f.Stat()
	if err == nil {
		_, err = io.CopyN(f, rand.Reader, fi.Size())
	}
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("shred %s: %w", path, err)
	}
	return os.Remove(path)
}
