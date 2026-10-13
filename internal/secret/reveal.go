package secret

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"

	"gopkg.in/yaml.v3"

	"github.com/giantswarm/beekeeper/internal/guard"
)

// ErrSecretLike is reveal's refusal of a leaf the classifier takes for a
// secret.
var ErrSecretLike = errors.New("looks secret")

// Field is a revealed leaf of a SOPS file: its dotted path and its value,
// configuration the classifier found no secret in.
type Field struct {
	Path  string `json:"path"`
	Value string `json:"value"`
}

// Reveal answers the leaves of a SOPS file under paths in plaintext: every
// scalar a path names, or under it. Each one is classified first, and one
// that looks secret refuses the whole call, named by its path and the
// reason, never its value.
func (o *Ops) Reveal(ctx context.Context, file string, paths []string) ([]Field, error) {
	if len(paths) == 0 {
		return nil, errors.New("reveal <file> <path>…: no path named")
	}
	doc, err := o.decrypt(ctx, file)
	if err != nil {
		return nil, err
	}
	var out []Field
	for _, p := range paths {
		leaves, err := o.revealed(doc, file, p)
		if err != nil {
			return nil, err
		}
		out = append(out, leaves...)
	}
	return out, nil
}

// revealed are the classified leaves of doc under path.
func (o *Ops) revealed(doc *document, file, path string) ([]Field, error) {
	n := doc.node(path)
	if n == nil {
		return nil, fmt.Errorf("%s: no value at %s", file, path)
	}
	leaves := (&document{root: n}).leaves()
	secrets := doc.secretValues()
	out := make([]Field, 0, len(leaves))
	for _, k := range sortedKeys(leaves) {
		p := path
		if k != "" {
			p += "." + k
		}
		why := o.secretLike(p, leaves[k], publicID(p))
		if s, ok := secrets[leaves[k]]; why == "" && ok && publicID(p) {
			why = fmt.Sprintf("an id equal to the secret at %s", s)
		}
		if why != "" {
			return nil, fmt.Errorf("%w: %s#%s: %s; reveal answers configuration only, copy and compare move and check a secret", ErrSecretLike, file, p, why)
		}
		out = append(out, Field{Path: p, Value: leaves[k]})
	}
	return out, nil
}

// RevealTo writes the configuration under path of a SOPS file into dst, a
// path of a plaintext YAML file (created when absent, its other keys kept),
// and answers the leaves written, which it never prints. Without write it
// answers what it would write. A destination that holds the same answers
// unchanged and writes nothing.
func (o *Ops) RevealTo(ctx context.Context, file, path string, dst Ref, write bool) (RevealResult, error) {
	res := RevealResult{To: dst.String()}
	if dst.Op != "" || dst.IsKube() || dst.Path == "" {
		return res, fmt.Errorf("%s: --to names a path of a plaintext YAML file, file#path", dst)
	}
	if same, err := sameFile(file, dst.File); err != nil || same {
		return res, errors.Join(err, fmt.Errorf("%s: the destination is the SOPS file itself", dst.File))
	}
	plain := newDocument()
	raw, err := os.ReadFile(dst.File)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return res, err
	case sopsMetadata(raw):
		return res, fmt.Errorf("%s is a SOPS file: reveal writes configuration into a plaintext file", dst.File)
	default:
		if plain, err = parseDocument(raw); err != nil {
			return res, fmt.Errorf("%s: %w", dst.File, err)
		}
	}
	doc, err := o.decrypt(ctx, file)
	if err != nil {
		return res, err
	}
	fields, err := o.revealed(doc, file, path)
	if err != nil {
		return res, err
	}
	for _, f := range fields {
		res.Paths = append(res.Paths, f.Path)
	}
	src := doc.node(path)
	if cur := plain.node(dst.Path); cur != nil && sameNode(cur, src) {
		res.Unchanged = true
		return res, nil
	}
	if !write {
		return res, nil
	}
	if err := plain.put(dst.Path, clone(src)); err != nil {
		return res, fmt.Errorf("%s: %w", dst.File, err)
	}
	out, err := plain.encode()
	if err != nil {
		return res, err
	}
	res.Written = true
	return res, writeFile(dst.File, out)
}

// RevealResult is what reveal --to answers: the destination, the leaves
// it carries, and whether it was written or held them already.
type RevealResult struct {
	To        string   `json:"to"`
	Paths     []string `json:"paths"`
	Written   bool     `json:"written"`
	Unchanged bool     `json:"unchanged"`
}

// UnsetResult is what unset answers: the paths removed (or, in a dry run,
// to remove), those the file holds no longer, and the key names it keeps.
type UnsetResult struct {
	File    string   `json:"file"`
	Removed []string `json:"removed"`
	Absent  []string `json:"absent"`
	Kept    []string `json:"kept"`
	Written bool     `json:"written"`
}

// Unset removes paths from a SOPS file with sops unset in beekeeper's
// process: the file keeps its recipients, sops renews its MAC and
// lastmodified. A path the file holds no longer is answered absent, so a
// second run removes nothing; without write nothing is removed. It answers
// the key names the file keeps, never a value.
func (o *Ops) Unset(ctx context.Context, file string, paths []string, write bool) (UnsetResult, error) {
	res := UnsetResult{File: file}
	if len(paths) == 0 {
		return res, errors.New("unset <file> <path>…: no path named")
	}
	named := make([]unsetTarget, 0, len(paths))
	for _, p := range paths {
		keys := splitPath(p)
		if keys[0] == "sops" || slices.Contains(keys, "") {
			return res, fmt.Errorf("%q: unset takes a dotted key path of the document, not sops' metadata", p)
		}
		named = append(named, unsetTarget{path: p, keys: keys})
	}
	doc, err := o.decrypt(ctx, file)
	if err != nil {
		return res, err
	}
	var held []unsetTarget
	for _, t := range unsetOrder(named) {
		if _, n, keys := doc.walk(t.keys); n != nil {
			held = append(held, unsetTarget{path: t.path, keys: keys})
		} else {
			res.Absent = append(res.Absent, t.path)
		}
	}
	var indices []string
	for _, t := range unsetOrder(held) {
		res.Removed = append(res.Removed, t.path)
		indices = append(indices, doc.sopsIndex(t.keys))
		doc.remove(t.path)
	}
	res.Kept = sortedKeys(doc.leaves())
	if !write || len(res.Removed) == 0 {
		return res, nil
	}
	env, err := o.ageEnv(ctx, file)
	if err != nil {
		return res, err
	}
	for i, p := range res.Removed {
		args := append(append([]string{"unset"}, sopsTypes(file, true)...), file, indices[i])
		if _, err := o.Run(ctx, "", env, nil, "sops", args...); err != nil {
			return res, fmt.Errorf("%s: unset %s: %w", file, p, err)
		}
	}
	res.Written = true
	return res, nil
}

// unsetTarget is a path unset names and the keys it resolves to, as the
// document holds them: credentials.json one key whichever way the path
// spells it.
type unsetTarget struct {
	path string
	keys []string
}

// unsetOrder is targets in the order they can be removed one by one: one
// under another named one goes (its parent removes it), as does a second
// spelling of the same keys, and of a list's items the later ones first,
// so that no index shifts before its turn.
func unsetOrder(targets []unsetTarget) []unsetTarget {
	out := slices.Clone(targets)
	slices.SortStableFunc(out, func(a, b unsetTarget) int { return -compareKeys(a.keys, b.keys) })
	out = slices.CompactFunc(out, func(a, b unsetTarget) bool { return slices.Equal(a.keys, b.keys) })
	return slices.DeleteFunc(out, func(t unsetTarget) bool {
		return slices.ContainsFunc(out, func(u unsetTarget) bool {
			return len(u.keys) < len(t.keys) && slices.Equal(u.keys, t.keys[:len(u.keys)])
		})
	})
}

// compareKeys orders key paths key by key, numbers as numbers.
func compareKeys(as, bs []string) int {
	for i := 0; i < len(as) && i < len(bs); i++ {
		x, errX := strconv.Atoi(as[i])
		y, errY := strconv.Atoi(bs[i])
		switch {
		case errX == nil && errY == nil && x != y:
			return x - y
		case as[i] != bs[i]:
			return strings.Compare(as[i], bs[i])
		}
	}
	return len(as) - len(bs)
}

// sopsIndex is the node keys name, keys as [document.walk] answers them,
// in sops' index syntax: ["stringData"]["credentials.json"], a list's item
// by its number, ["a"][0]["b"].
func (d *document) sopsIndex(keys []string) string {
	var b strings.Builder
	n := d.root
	for _, k := range keys {
		if n.Kind == yaml.SequenceNode {
			b.WriteString("[" + k + "]")
		} else {
			b.WriteString("[" + strconv.Quote(k) + "]")
		}
		n = child(n, k)
	}
	return b.String()
}

// secretKey is a key whose value is a secret whatever it looks like.
var secretKey = regexp.MustCompile(`(?i)secret|passw(or)?d|pwd|token|key|credential|private|cookie|salt|hmac|cert`)

// publicIDKey is a leaf naming an OAuth client, public in every authorize
// URL however random it looks.
var publicIDKey = regexp.MustCompile(`(?i)^(id|client_?id)$`)

// peersKey is a list of client ids, each item public like publicIDKey.
var peersKey = regexp.MustCompile(`(?i)^(trusted_?)?peers$`)

// publicID reports whether the leaf at path is a client id: a key named id
// or clientID, or an item of a trustedPeers or peers list.
func publicID(path string) bool {
	ks := strings.Split(path, ".")
	last := ks[len(ks)-1]
	if publicIDKey.MatchString(last) {
		return true
	}
	_, err := strconv.Atoi(last)
	return err == nil && len(ks) > 1 && peersKey.MatchString(ks[len(ks)-2])
}

// secretValues maps every non-empty value under a key named like a secret
// to its path, the first in path order: a client id equal to one is a
// secret filed under an id's name.
func (d *document) secretValues() map[string]string {
	leaves := d.leaves()
	out := map[string]string{}
	for _, p := range sortedKeys(leaves) {
		if _, seen := out[leaves[p]]; seen || leaves[p] == "" {
			continue
		}
		if slices.ContainsFunc(strings.Split(p, "."), secretKey.MatchString) {
			out[leaves[p]] = p
		}
	}
	return out
}

// secretLike is why a leaf at path looks like a secret, "" when it looks
// like configuration: a key on its path named like a secret, a value the
// value scanner matches (a token pattern or an indexed secret), a URL
// carrying a password, or a run of key-like entropy, which a client id
// (id) may have.
func (o *Ops) secretLike(path, v string, id bool) string {
	for _, k := range strings.Split(path, ".") {
		if secretKey.MatchString(k) {
			return fmt.Sprintf("under the key %q, named like a secret", k)
		}
	}
	ix := o.Index
	if ix == nil {
		ix = &guard.Index{}
	}
	if _, found := ix.Redact(v); len(found) > 0 {
		return "the value scanner matches it (" + found[0].Name() + ")"
	}
	if u, err := url.Parse(v); err == nil && u.User != nil {
		if _, ok := u.User.Password(); ok {
			return "a URL carrying a password"
		}
	}
	if id {
		return ""
	}
	for _, run := range strings.FieldsFunc(v, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '+' && r != '=' }) {
		if keyLike(run) {
			return fmt.Sprintf("a run of %d characters with a key's entropy", len(run))
		}
	}
	return ""
}

// keyLike reports whether a run of letters and digits looks generated: at
// least 16 characters with letters and several digits (hex, alnum) or
// several upper and lower case letters, at a key's entropy, or 32 characters at a key's entropy
// whatever their classes. Words, host names and short ids do not.
func keyLike(run string) bool {
	if len(run) < 16 {
		return false
	}
	count := func(in func(rune) bool) int {
		n := 0
		for _, r := range run {
			if in(r) {
				n++
			}
		}
		return n
	}
	digits, lower, upper := count(unicode.IsDigit), count(unicode.IsLower), count(unicode.IsUpper)
	e := entropy(run)
	switch {
	case e >= 3.0 && digits >= 2 && lower+upper > 0:
		return true
	case e >= 3.5 && lower >= 3 && upper >= 3:
		return true
	}
	return e >= 3.5 && len(run) >= 32
}

// entropy is the Shannon entropy of s in bits per character.
func entropy(s string) float64 {
	n := map[rune]int{}
	for _, r := range s {
		n[r]++
	}
	var e float64
	for _, c := range n {
		p := float64(c) / float64(len(s))
		e -= p * math.Log2(p)
	}
	return e
}

// sameFile reports whether a and b name one file.
func sameFile(a, b string) (bool, error) {
	x, err := filepath.Abs(a)
	if err != nil {
		return false, err
	}
	y, err := filepath.Abs(b)
	return x == y, err
}

// sameNode reports whether two YAML nodes hold the same document.
func sameNode(a, b *yaml.Node) bool {
	x, errX := yaml.Marshal(a)
	y, errY := yaml.Marshal(b)
	return errX == nil && errY == nil && string(x) == string(y)
}

// clone is a deep copy of a YAML node, without comments: they stay in the
// SOPS file.
func clone(n *yaml.Node) *yaml.Node {
	c := &yaml.Node{Kind: n.Kind, Style: n.Style, Tag: n.Tag, Value: n.Value}
	if n.Kind == yaml.AliasNode {
		return clone(n.Alias)
	}
	for _, x := range n.Content {
		c.Content = append(c.Content, clone(x))
	}
	return c
}
