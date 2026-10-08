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
	if a.Single() != b.Single() {
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
	if a.Single() {
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

// Fingerprints are the keyed fingerprints of what r names, of the one value
// in o.Encode when an encoding is set.
func (o *Ops) Fingerprints(ctx context.Context, r Ref) ([]Print, error) {
	if !o.Encode.IsZero() {
		v, err := o.encoded(ctx, r)
		if err != nil {
			return nil, err
		}
		return []Print{{Key: r.String() + " (" + o.Encode.String() + ")", Fingerprint: o.Fingerprint(v)}}, nil
	}
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
	if src.Single() {
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
	v, err := o.encoded(ctx, src)
	if err != nil {
		return 0, err
	}
	return len(v), o.put(ctx, dst, v)
}

// Pair is one value of a copy into a new file: the reference it is read
// from and the path it takes there.
type Pair struct {
	Src  Ref
	Path string
}

// ParsePair reads <ref>=<path>, the path after the last "=": the reference
// names one value (op://…, file#path), the path is a key of the new file.
func ParsePair(s string) (Pair, error) {
	i := strings.LastIndex(s, "=")
	if i < 0 {
		return Pair{}, fmt.Errorf("%q: a value goes in as <ref>=<path>", s)
	}
	src, err := ParseRef(s[:i])
	if err != nil {
		return Pair{}, err
	}
	p := Pair{Src: src, Path: s[i+1:]}
	switch {
	case !src.Single():
		return Pair{}, fmt.Errorf("%q: name one value (file#path or op://…), not a whole file", s)
	case p.Path == "" || strings.ContainsAny(p.Path, "#/"):
		return Pair{}, fmt.Errorf("%q: the path after = is a dotted key of the new file", s)
	}
	return p, nil
}

// CopyValues writes several values into a new SOPS file in one
// encryption: sops needs only the recipients of the file's creation rule,
// nothing is decrypted, so no age identity of the destination is needed. An
// absent file starts as the Secret nw names (an empty document when nil), a
// plaintext Secret skeleton is filled; an encrypted file is refused. Every
// path is checked against the creation rule before a value is read. It
// answers the keys and their lengths.
func (o *Ops) CopyValues(ctx context.Context, pairs []Pair, file string, nw *NewSecret) ([]Key, error) {
	if len(pairs) == 0 {
		return nil, errors.New("copy <ref>=<path>… <file>: no value named")
	}
	if raw, err := os.ReadFile(file); err == nil && sopsMetadata(raw) { //nolint:gosec // the caller's SOPS file
		return nil, fmt.Errorf("%s is encrypted: copy <ref>=<path>… writes a new file in one encryption; "+
			"one value goes into an existing file with copy <ref> <file#path>", file)
	} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	if nw != nil {
		if err := nw.check(file); err != nil {
			return nil, err
		}
	}
	doc, _, err := o.target(ctx, Ref{File: file, Path: pairs[0].Path}, nw)
	if err != nil {
		return nil, err
	}
	paths := make([]string, len(pairs))
	for i, p := range pairs {
		if paths[i], err = place(doc, file, p.Path); err != nil {
			return nil, err
		}
		if slices.Contains(paths[:i], paths[i]) {
			return nil, fmt.Errorf("%s: %s is named twice", file, paths[i])
		}
	}
	vs := make([]string, len(pairs))
	for i, p := range pairs {
		if vs[i], err = o.value(ctx, p.Src); err != nil {
			return nil, err
		}
	}
	for i := range pairs {
		if err := doc.set(paths[i], vs[i]); err != nil {
			return nil, fmt.Errorf("%s: %w", file, err)
		}
	}
	if err := o.encrypt(ctx, doc, file); err != nil {
		return nil, err
	}
	return doc.keys(), nil
}

// put writes v at dst's path, the rest of its file kept.
func (o *Ops) put(ctx context.Context, dst Ref, v string) error {
	doc, dst, err := o.target(ctx, dst, nil)
	if err != nil {
		return err
	}
	return o.write(ctx, doc, dst, v)
}

// target is the document a value for dst goes into and the path it takes
// there. An absent file starts as the Secret nw names, an empty document
// when nil; a plaintext Kubernetes Secret without values, a skeleton, is
// filled like a file absent so far, and any other plaintext file is refused
// by what it is. A Secret's value goes under stringData
// unless the path names data or stringData. A path the file's creation rule
// would leave in plaintext is refused before any value is written.
func (o *Ops) target(ctx context.Context, dst Ref, nw *NewSecret) (*document, Ref, error) {
	doc := newDocument()
	if nw != nil {
		doc = nw.document()
	}
	raw, err := os.ReadFile(dst.File)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return nil, dst, err
	default:
		skel, err := skeleton(raw)
		switch {
		case err != nil:
			return nil, dst, fmt.Errorf("%s: %w", dst.File, err)
		case skel != nil:
			doc = skel
		default:
			if doc, err = o.decrypt(ctx, dst.File); err != nil {
				return nil, dst, err
			}
		}
	}
	if dst.Path, err = place(doc, dst.File, dst.Path); err != nil {
		return nil, dst, err
	}
	return doc, dst, nil
}

// place is the path a value for path takes in doc, the document of file,
// refused when file's creation rule would leave it in plaintext.
func place(doc *document, file, path string) (string, error) {
	path = doc.valuePath(path)
	cfg, rel, err := sopsTarget(file)
	if err != nil {
		return "", err
	}
	rule, err := ruleFor(cfg, rel)
	if err != nil {
		return "", err
	}
	if rule != nil {
		if err := rule.check(path); err != nil {
			return "", fmt.Errorf("%s: %w (%s)", file, err, cfg)
		}
	}
	return path, nil
}

// write sets v at dst's path of doc and encrypts doc into dst's file.
func (o *Ops) write(ctx context.Context, doc *document, dst Ref, v string) error {
	if err := doc.set(dst.Path, v); err != nil {
		return fmt.Errorf("%s: %w", dst.File, err)
	}
	return o.encrypt(ctx, doc, dst.File)
}

// Consumer allows a command to take a value on stdin: one that reads a
// secret from stdin and stores it without printing it, or kubectl exec -i
// handing stdin to such a command in a pod.
func Consumer(argv []string) error {
	if len(argv) == 0 {
		return errors.New("no consumer: copy <ref> -- <command>")
	}
	if filepath.Base(argv[0]) == "kubectl" {
		return kubectlExec(argv)
	}
	if stdinReader(argv) {
		return nil
	}
	return fmt.Errorf("%q takes no value from beekeeper: the consumers are `gh secret set`, `garage json-api <endpoint> -`, "+
		"a command with --password-stdin or --secret <name>=-, and `kubectl exec -i --context <context> <pod> -- <one of them>`", strings.Join(argv, " "))
}

// stdinReader reports whether a command reads a secret from stdin and
// stores it without printing it.
func stdinReader(argv []string) bool {
	switch filepath.Base(argv[0]) {
	case "gh":
		return len(argv) >= 3 && argv[1] == "secret" && argv[2] == "set"
	case "garage":
		// the JSON request on stdin, "-" its only argument after the endpoint
		return len(argv) == 4 && argv[1] == "json-api" && argv[3] == "-"
	}
	for i, a := range argv[1:] {
		switch {
		case a == "--password-stdin":
			return true
		case a == "--secret" && i+2 < len(argv) && strings.HasSuffix(argv[i+2], "=-"),
			strings.HasPrefix(a, "--secret=") && strings.HasSuffix(a, "=-"):
			return true
		}
	}
	return false
}

// kubectlExec allows kubectl exec -i with an explicit --context, no TTY
// (which would echo stdin) and no verbosity, whose command in the pod is a
// stdin reader: the value travels on the exec stream alone, never on an
// argv.
func kubectlExec(argv []string) error {
	cmd := strings.Join(argv, " ")
	dash := slices.Index(argv, "--")
	if len(argv) < 2 || argv[1] != "exec" || dash < 0 {
		return fmt.Errorf("%q: kubectl takes a value only as kubectl exec -i --context <context> <pod> -- <command>", cmd)
	}
	stdin := false
	for _, a := range argv[2:dash] {
		switch {
		case a == "-i", a == "--stdin", a == "--stdin=true":
			stdin = true
		case a == "-t", a == "--tty", strings.HasPrefix(a, "--tty="), strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "--") && len(a) > 2 && strings.ContainsAny(a[1:], "it"):
			return fmt.Errorf("%q: no TTY and no combined short flags, a TTY echoes stdin: kubectl exec -i", cmd)
		case a == "-v", a == "--v", strings.HasPrefix(a, "-v="), strings.HasPrefix(a, "--v="):
			return fmt.Errorf("%q: no verbosity, kubectl's request log stays off", cmd)
		}
	}
	switch {
	case !stdin:
		return fmt.Errorf("%q: the value goes on the exec stream, kubectl exec -i", cmd)
	case ConsumerContext(argv) == "":
		return fmt.Errorf("%q: name the cluster, --context <context>", cmd)
	case dash+1 == len(argv) || !stdinReader(argv[dash+1:]):
		return fmt.Errorf("%q: the command in the pod reads the value on stdin: `garage json-api <endpoint> -`, `gh secret set`, "+
			"or one with --password-stdin or --secret <name>=-", cmd)
	}
	return nil
}

// ConsumerContext is the kube context a kubectl exec consumer names, ""
// for any other consumer.
func ConsumerContext(argv []string) string {
	if len(argv) < 2 || filepath.Base(argv[0]) != "kubectl" {
		return ""
	}
	end := slices.Index(argv, "--")
	if end < 0 {
		end = len(argv)
	}
	for i, a := range argv[:end] {
		if a == "--context" && i+1 < end {
			return argv[i+1]
		}
		if c, ok := strings.CutPrefix(a, "--context="); ok {
			return c
		}
	}
	return ""
}

// Stdin shapes what a consumer reads: the value alone, or with Field set,
// the JSON object Template with the value at Field (garage json-api's
// request, for one).
type Stdin struct {
	Template string
	Field    string
}

// Check refuses a template that is no JSON object or already holds Field.
func (s Stdin) Check() error {
	if s.Template == "" && s.Field == "" {
		return nil
	}
	if s.Field == "" {
		return errors.New("--stdin-json takes --stdin-field, the key the value goes to")
	}
	obj := map[string]any{}
	if s.Template != "" {
		if err := json.Unmarshal([]byte(s.Template), &obj); err != nil {
			return errors.New("--stdin-json: no JSON object")
		}
	}
	if _, ok := obj[s.Field]; ok {
		return fmt.Errorf("--stdin-json holds %q already: the value goes there", s.Field)
	}
	return nil
}

// input is what the consumer reads for v.
func (s Stdin) input(v string) (string, error) {
	if s.Field == "" {
		return v, nil
	}
	obj := map[string]any{}
	if s.Template != "" {
		if err := json.Unmarshal([]byte(s.Template), &obj); err != nil {
			return "", errors.New("--stdin-json: no JSON object")
		}
	}
	obj[s.Field] = v
	b, err := json.Marshal(obj)
	return string(b), err
}

// CopyToConsumer runs an allowed consumer with the value on stdin, shaped
// by in. It answers the consumer's exit code and its output with the value
// redacted.
func (o *Ops) CopyToConsumer(ctx context.Context, src Ref, argv []string, in Stdin) (int, string, error) {
	if err := Consumer(argv); err != nil {
		return 0, "", err
	}
	if err := in.Check(); err != nil {
		return 0, "", err
	}
	v, err := o.encoded(ctx, src)
	if err != nil {
		return 0, "", err
	}
	return consume(ctx, v, src.String(), argv, in)
}

// consume runs a consumer with v on stdin, shaped by in, answering its
// exit code and its output with v redacted as ref.
func consume(ctx context.Context, v, ref string, argv []string, in Stdin) (int, string, error) {
	input, err := in.input(v)
	if err != nil {
		return 0, "", err
	}
	c := exec.CommandContext(ctx, argv[0], argv[1:]...) //nolint:gosec // an allow-listed consumer
	c.Stdin = strings.NewReader(input)
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
		quoted, _ := json.Marshal(value)
		for _, f := range []string{value, strings.Trim(string(quoted), `"`),
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
	Stdin    Stdin
	// New is the Secret a file absent so far starts as, its name and
	// namespace; nil starts an empty document.
	New *NewSecret
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
// a consumer's stdin when given, these three in o.Encode. A value without a
// vault field lives in the SOPS file alone.
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
		if err := opt.Stdin.Check(); err != nil {
			return SetResult{}, err
		}
	}
	if opt.New != nil {
		if err := opt.New.check(dst.File); err != nil {
			return SetResult{}, err
		}
	}
	doc, dst, err := o.target(ctx, dst, opt.New)
	if err != nil {
		return SetResult{}, err
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
	// the vault keeps the generated value, every other home its encoded form
	v = o.Encode.Apply(v)
	if err := o.write(ctx, doc, dst, v); err != nil {
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
		if res.Code, res.Output, err = consume(ctx, v, dst.String(), opt.Consumer, opt.Stdin); err != nil {
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
	items, err := o.vaultItems(ctx, vault)
	if err != nil {
		return fmt.Errorf("%s: %w", r.Op, err)
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

// vaultItem is what a vault's item listing says of one item: metadata, no
// field and no value.
type vaultItem struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}

// vaultItems lists the items of vault, metadata only.
func (o *Ops) vaultItems(ctx context.Context, vault string) ([]vaultItem, error) {
	raw, err := o.op(ctx, nil, "item", "list", "--vault", vault, "--format", "json")
	if err != nil {
		return nil, err
	}
	var items []vaultItem
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, errors.New("op item list answered no list")
	}
	return items, nil
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
