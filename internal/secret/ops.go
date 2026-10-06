package secret

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/giantswarm/beekeeper/internal/guard"
)

// The states compare answers per key.
const (
	Equal     = "equal"
	Different = "different"
	OnlyA     = "only in a"
	OnlyB     = "only in b"
)

// Verdict is compare's answer for one key.
type Verdict struct {
	Key   string `json:"key"`
	State string `json:"state"`
}

// Compare answers per key whether a and b hold the same values. Two single
// references compare as one value; two whole files key by key.
func (o *Ops) Compare(ctx context.Context, a, b Ref) ([]Verdict, error) {
	if a.single() != b.single() {
		return nil, errors.New("compare one value with one value (file#path, op://…), or a whole file with a whole file")
	}
	va, err := o.values(ctx, a)
	if err != nil {
		return nil, err
	}
	vb, err := o.values(ctx, b)
	if err != nil {
		return nil, err
	}
	if a.single() {
		st := Different
		if va[a.String()] == vb[b.String()] {
			st = Equal
		}
		return []Verdict{{Key: a.String() + " " + b.String(), State: st}}, nil
	}
	var out []Verdict
	for _, k := range sortedKeys(va, vb) {
		x, inA := va[k]
		y, inB := vb[k]
		st := Equal
		switch {
		case !inB:
			st = OnlyA
		case !inA:
			st = OnlyB
		case x != y:
			st = Different
		}
		out = append(out, Verdict{Key: k, State: st})
	}
	return out, nil
}

// Print is a value's keyed fingerprint.
type Print struct {
	Key         string `json:"key"`
	Fingerprint string `json:"fingerprint"`
}

// Fingerprints are the keyed fingerprints of what r names.
func (o *Ops) Fingerprints(ctx context.Context, r Ref) ([]Print, error) {
	vs, err := o.values(ctx, r)
	if err != nil {
		return nil, err
	}
	out := make([]Print, 0, len(vs))
	for _, k := range sortedKeys(vs) {
		out = append(out, Print{Key: k, Fingerprint: o.Fingerprint(vs[k])})
	}
	return out, nil
}

// CopyFile copies a whole SOPS file to a new one, encrypted under the
// destination's creation rules, with a Kubernetes object's metadata.name
// and metadata.namespace rewritten when given. It answers the keys and
// their lengths.
func (o *Ops) CopyFile(ctx context.Context, src Ref, dst, name, namespace string) ([]Key, error) {
	if src.single() {
		return nil, fmt.Errorf("%s: copy into a file takes a whole SOPS file; one value goes to file#path", src)
	}
	if _, err := os.Stat(dst); err == nil {
		return nil, fmt.Errorf("%s exists: copy writes a new file, move the old one aside first", dst)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	doc, err := o.decrypt(ctx, src.File)
	if err != nil {
		return nil, err
	}
	if name != "" || namespace != "" {
		if err := doc.rename(name, namespace); err != nil {
			return nil, fmt.Errorf("%s: %w", src.File, err)
		}
	}
	if err := o.encrypt(ctx, doc, dst); err != nil {
		return nil, err
	}
	return doc.keys(), nil
}

// CopyValue copies one value into a path of a SOPS file, creating the file
// or the key when absent. It answers the value's length.
func (o *Ops) CopyValue(ctx context.Context, src, dst Ref) (int, error) {
	if dst.Op != "" || dst.Path == "" {
		return 0, fmt.Errorf("%s: one value goes to a SOPS path, file#path", dst)
	}
	v, err := o.value(ctx, src)
	if err != nil {
		return 0, err
	}
	return len(v), o.put(ctx, dst, v)
}

// put writes v at dst's path, the rest of its file kept.
func (o *Ops) put(ctx context.Context, dst Ref, v string) error {
	doc := newDocument()
	if _, err := os.Stat(dst.File); err == nil {
		if doc, err = o.decrypt(ctx, dst.File); err != nil {
			return err
		}
	}
	if err := doc.set(dst.Path, v); err != nil {
		return fmt.Errorf("%s: %w", dst.File, err)
	}
	return o.encrypt(ctx, doc, dst.File)
}

// Consumer allows a command to take a value on stdin: one that reads a
// secret from stdin and stores it without printing it.
func Consumer(argv []string) error {
	if len(argv) == 0 {
		return errors.New("no consumer: copy <ref> -- <command>")
	}
	if filepath.Base(argv[0]) == "gh" && len(argv) >= 3 && argv[1] == "secret" && argv[2] == "set" {
		return nil
	}
	for i, a := range argv[1:] {
		switch {
		case a == "--password-stdin":
			return nil
		case a == "--secret" && i+2 < len(argv) && strings.HasSuffix(argv[i+2], "=-"),
			strings.HasPrefix(a, "--secret=") && strings.HasSuffix(a, "=-"):
			return nil
		}
	}
	return fmt.Errorf("%q takes no value from beekeeper: the consumers are `gh secret set`, a command with --password-stdin and one with --secret <name>=-", strings.Join(argv, " "))
}

// CopyToConsumer runs an allowed consumer with the value on stdin. It
// answers the consumer's exit code and its output with the value redacted.
func (o *Ops) CopyToConsumer(ctx context.Context, src Ref, argv []string) (int, string, error) {
	if err := Consumer(argv); err != nil {
		return 0, "", err
	}
	v, err := o.value(ctx, src)
	if err != nil {
		return 0, "", err
	}
	return consume(ctx, v, src.String(), argv)
}

// consume runs a consumer with v on stdin, answering its exit code and its
// output with v redacted as ref.
func consume(ctx context.Context, v, ref string, argv []string) (int, string, error) {
	c := exec.CommandContext(ctx, argv[0], argv[1:]...) //nolint:gosec // an allow-listed consumer
	c.Stdin = strings.NewReader(v)
	var out bytes.Buffer
	c.Stdout, c.Stderr = &out, &out
	code := 0
	if err := c.Run(); err != nil {
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			return 0, "", err
		}
		code = ee.ExitCode()
	}
	return code, redact(out.String(), v, ref), nil
}

// redact replaces a value and its base64 forms in s, then what the token
// patterns match.
func redact(s, value, ref string) string {
	if value != "" {
		mark := "[redacted: " + ref + "]"
		for _, f := range []string{value,
			base64.StdEncoding.EncodeToString([]byte(value)),
			base64.RawStdEncoding.EncodeToString([]byte(value))} {
			s = strings.ReplaceAll(s, f, mark)
		}
	}
	s, _ = (&guard.Index{}).Redact(s)
	return s
}

// The character sets set --generate draws from.
var charsets = map[string]string{
	"alnum": "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789",
	"hex":   "0123456789abcdef",
	"ascii": "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789!#%+-.:=@^_~",
}

// Charsets names the character sets, sorted.
func Charsets() []string { return sortedKeys(charsets) }

// Generate draws a value of n characters from a named set.
func Generate(n int, charset string) (string, error) {
	set, ok := charsets[charset]
	if !ok {
		return "", fmt.Errorf("charset %q: one of %s", charset, strings.Join(Charsets(), ", "))
	}
	if n < 16 {
		return "", fmt.Errorf("length %d: at least 16", n)
	}
	b := make([]byte, n)
	for i := range b {
		k, err := rand.Int(rand.Reader, big.NewInt(int64(len(set))))
		if err != nil {
			return "", err
		}
		b[i] = set[k.Int64()]
	}
	return string(b), nil
}

// SetOptions are what set draws and where the value goes besides the SOPS
// path: the shared vault's field first, a lab's Secret and a consumer's
// stdin after it. Each is optional; the value never leaves the process
// otherwise.
type SetOptions struct {
	Length   int
	Charset  string
	Vault    Ref
	Secret   *KubeTarget
	Consumer []string
}

// SetResult is what set answers: the value's fingerprint and, with a
// consumer, its exit code and its output with the value redacted.
type SetResult struct {
	Key         string `json:"key"`
	Fingerprint string `json:"fingerprint"`
	Code        int    `json:"code,omitempty"`
	Output      string `json:"output,omitempty"`
}

// Set generates a value and writes it to the shared vault's field first
// when one is given, then into the SOPS path, then into a lab's Secret and
// a consumer's stdin when given. A value without a vault field lives in the
// SOPS file alone.
func (o *Ops) Set(ctx context.Context, dst Ref, opt SetOptions) (SetResult, error) {
	if dst.Op != "" || dst.Path == "" {
		return SetResult{}, fmt.Errorf("%s: set writes a SOPS path, file#path", dst)
	}
	if opt.Vault != (Ref{}) {
		if opt.Vault.Op == "" {
			return SetResult{}, fmt.Errorf("%s: the vault copy is an op://<vault>/<item>/<field>", opt.Vault)
		}
		if err := o.checkVault(opt.Vault); err != nil {
			return SetResult{}, err
		}
	}
	if opt.Secret != nil && opt.Secret.KindCluster() == "" {
		return SetResult{}, fmt.Errorf("%s: a Secret is written only into a kind lab's context, kind-<cluster>", opt.Secret.Context)
	}
	if opt.Consumer != nil {
		if err := Consumer(opt.Consumer); err != nil {
			return SetResult{}, err
		}
	}
	v, err := Generate(opt.Length, opt.Charset)
	if err != nil {
		return SetResult{}, err
	}
	if opt.Vault != (Ref{}) {
		if err := o.storeVault(ctx, opt.Vault, v); err != nil {
			return SetResult{}, err
		}
	}
	if err := o.put(ctx, dst, v); err != nil {
		if opt.Vault != (Ref{}) {
			return SetResult{}, fmt.Errorf("the vault holds the value, the SOPS file not: %w", err)
		}
		return SetResult{}, err
	}
	res := SetResult{Key: dst.String(), Fingerprint: o.Fingerprint(v)}
	if opt.Secret != nil {
		if err := o.toSecret(ctx, v, dst.String(), *opt.Secret); err != nil {
			return res, fmt.Errorf("%s holds the value, the Secret not (copy %s --to-secret %s): %w", dst, dst, opt.Secret, err)
		}
	}
	if opt.Consumer != nil {
		if res.Code, res.Output, err = consume(ctx, v, dst.String(), opt.Consumer); err != nil {
			return res, fmt.Errorf("%s holds the value, the consumer not (copy %s -- …): %w", dst, dst, err)
		}
	}
	return res, nil
}

// storeVault writes v into the concealed field of a vault item, creating
// the item or the field when absent. The item travels as JSON on stdin,
// never on a command line; op refuses piped input next to --template.
func (o *Ops) storeVault(ctx context.Context, r Ref, v string) error {
	parts := strings.SplitN(strings.TrimPrefix(r.Op, guard.OpRef), "/", 3)
	vault, title, field := parts[0], parts[1], parts[2]
	raw, err := o.op(ctx, nil, "item", "list", "--vault", vault, "--format", "json")
	if err != nil {
		return fmt.Errorf("%s: %w", r.Op, err)
	}
	var items []struct {
		ID    string `json:"id"`
		Title string `json:"title"`
	}
	if err := json.Unmarshal(raw, &items); err != nil {
		return fmt.Errorf("%s: op item list answered no list", r.Op)
	}
	id := ""
	for _, it := range items {
		if it.Title == title {
			if id != "" {
				return fmt.Errorf("%s: more than one item %q in %q", r.Op, title, vault)
			}
			id = it.ID
		}
	}
	item := map[string]any{"title": title, "category": "PASSWORD"}
	if id != "" {
		raw, err := o.op(ctx, nil, "item", "get", id, "--vault", vault, "--format", "json")
		if err != nil {
			return fmt.Errorf("%s: %w", r.Op, err)
		}
		item = map[string]any{}
		if err := json.Unmarshal(raw, &item); err != nil {
			return fmt.Errorf("%s: op item get answered no item", r.Op)
		}
	}
	setField(item, field, v)
	tmpl, err := json.Marshal(item)
	if err != nil {
		return err
	}
	args := []string{"item", "create", "-", "--vault", vault, "--format", "json"}
	if id != "" {
		args = []string{"item", "edit", id, "--vault", vault, "--format", "json"}
	}
	if _, err := o.op(ctx, bytes.NewReader(tmpl), args...); err != nil {
		return fmt.Errorf("%s: %w", r.Op, err)
	}
	return nil
}

// setField sets the concealed field labelled label in an item's JSON.
func setField(item map[string]any, label, v string) {
	fields, _ := item["fields"].([]any)
	for _, f := range fields {
		if m, ok := f.(map[string]any); ok && (m["label"] == label || m["id"] == label) {
			m["value"] = v
			m["type"] = "CONCEALED"
			return
		}
	}
	item["fields"] = append(fields, map[string]any{"id": label, "label": label, "type": "CONCEALED", "value": v})
}

// sortedKeys are the keys of the maps, merged and sorted.
func sortedKeys[V any](ms ...map[string]V) []string {
	var out []string
	for _, m := range ms {
		for k := range m {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return slices.Compact(out)
}
