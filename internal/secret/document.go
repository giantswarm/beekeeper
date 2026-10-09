package secret

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"slices"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
	"k8s.io/apimachinery/pkg/util/validation"
)

// mapTag and secretKind are the YAML tag of a mapping and the kind of a
// Kubernetes Secret.
const (
	mapTag     = "!!map"
	secretKind = "Secret"
)

// document is a decrypted SOPS file as a YAML tree, kept as nodes so that
// a copy keeps its keys' order and comments.
type document struct {
	root *yaml.Node
}

func parseDocument(raw []byte) (*document, error) {
	var n yaml.Node
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	if err := dec.Decode(&n); err != nil {
		return nil, errors.New("sops answered no YAML document")
	}
	var extra yaml.Node
	if dec.Decode(&extra) == nil {
		return nil, errors.New("more than one YAML document: beekeeper secret handles one per file")
	}
	if len(n.Content) != 1 || n.Content[0].Kind != yaml.MappingNode {
		return nil, errors.New("the document is no mapping")
	}
	return &document{root: n.Content[0]}, nil
}

// newDocument is an empty mapping.
func newDocument() *document {
	return &document{root: &yaml.Node{Kind: yaml.MappingNode, Tag: mapTag}}
}

func (d *document) encode() ([]byte, error) {
	var b bytes.Buffer
	enc := yaml.NewEncoder(&b)
	enc.SetIndent(2)
	if err := enc.Encode(d.root); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// leaves maps the dotted path of every scalar to its value.
func (d *document) leaves() map[string]string {
	out := map[string]string{}
	var walk func(n *yaml.Node, path string)
	walk = func(n *yaml.Node, path string) {
		join := func(k string) string {
			if path == "" {
				return k
			}
			return path + "." + k
		}
		switch n.Kind {
		case yaml.ScalarNode:
			out[path] = n.Value
		case yaml.MappingNode:
			for i := 0; i+1 < len(n.Content); i += 2 {
				walk(n.Content[i+1], join(n.Content[i].Value))
			}
		case yaml.SequenceNode:
			for i, c := range n.Content {
				walk(c, join(strconv.Itoa(i)))
			}
		case yaml.AliasNode:
			walk(n.Alias, path)
		}
	}
	walk(d.root, "")
	return out
}

// get is the scalar at a dotted path.
func (d *document) get(path string) (string, bool) {
	n := d.node(path)
	if n == nil || n.Kind != yaml.ScalarNode {
		return "", false
	}
	return n.Value, true
}

// node is the node at a dotted path, nil when absent.
func (d *document) node(path string) *yaml.Node {
	n := d.root
	for _, k := range strings.Split(path, ".") {
		if n = child(n, k); n == nil {
			return nil
		}
	}
	return n
}

// remove deletes the key or list item at a dotted path; an absent one
// stays absent.
func (d *document) remove(path string) {
	parent, k := d.root, path
	if i := strings.LastIndex(path, "."); i >= 0 {
		parent, k = d.node(path[:i]), path[i+1:]
	}
	switch {
	case parent == nil:
	case parent.Kind == yaml.MappingNode:
		for i := 0; i+1 < len(parent.Content); i += 2 {
			if parent.Content[i].Value == k {
				parent.Content = slices.Delete(parent.Content, i, i+2)
				return
			}
		}
	case parent.Kind == yaml.SequenceNode:
		if i, err := strconv.Atoi(k); err == nil && i >= 0 && i < len(parent.Content) {
			parent.Content = slices.Delete(parent.Content, i, i+1)
		}
	}
}

// child is the node under key k of a mapping, or index k of a sequence.
func child(n *yaml.Node, k string) *yaml.Node {
	switch n.Kind {
	case yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			if n.Content[i].Value == k {
				return n.Content[i+1]
			}
		}
	case yaml.SequenceNode:
		if i, err := strconv.Atoi(k); err == nil && i >= 0 && i < len(n.Content) {
			return n.Content[i]
		}
	}
	return nil
}

// set puts a string at a dotted path, creating the mappings on the way.
func (d *document) set(path, value string) error {
	if n := d.node(path); n != nil && n.Kind != yaml.ScalarNode {
		return fmt.Errorf("%q holds a %s, not a value", path, kindName(n.Kind))
	}
	return d.put(path, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value})
}

// put puts a node at a dotted path, replacing what is there and creating
// the mappings on the way.
func (d *document) put(path string, value *yaml.Node) error {
	keys := strings.Split(path, ".")
	n := d.root
	for i, k := range keys {
		if k == "" {
			return fmt.Errorf("%q: an empty key", path)
		}
		c := child(n, k)
		last := i == len(keys)-1
		if c != nil && !last && c.Kind == yaml.ScalarNode && c.Tag == "!!null" {
			// an empty key on the way, a skeleton's `stringData:`
			*c = yaml.Node{Kind: yaml.MappingNode, Tag: mapTag}
		}
		switch {
		case c == nil && n.Kind != yaml.MappingNode:
			return fmt.Errorf("%q: %s is no mapping", path, strings.Join(keys[:i], "."))
		case c == nil:
			c = &yaml.Node{Kind: yaml.MappingNode, Tag: mapTag}
			if last {
				c = &yaml.Node{}
			}
			n.Content = append(n.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: k}, c)
		}
		n = c
	}
	*n = *value
	return nil
}

// valuePath is where a value for path goes: a Kubernetes Secret holds its
// values under stringData or data, so any other path lands under
// stringData.
func (d *document) valuePath(path string) string {
	if k, _ := d.get("kind"); k != secretKind || strings.HasPrefix(path, "data.") || strings.HasPrefix(path, "stringData.") {
		return path
	}
	return "stringData." + path
}

func kindName(k yaml.Kind) string {
	switch k {
	case yaml.MappingNode:
		return "mapping"
	case yaml.SequenceNode:
		return "list"
	}
	return "node"
}

// rename sets a Kubernetes object's metadata.name and metadata.namespace;
// an empty one stays as it is.
func (d *document) rename(name, namespace string) error {
	if child(d.root, "metadata") == nil || child(d.root, "kind") == nil {
		return errors.New("no kind and metadata: --name and --namespace rewrite a Kubernetes object")
	}
	if name != "" {
		if err := d.set("metadata.name", name); err != nil {
			return err
		}
	}
	if namespace != "" {
		if err := d.set("metadata.namespace", namespace); err != nil {
			return err
		}
	}
	return nil
}

// NewSecret is the Kubernetes Secret a new SOPS file starts as.
type NewSecret struct {
	Name      string
	Namespace string
}

// check refuses an invalid name or namespace, and a file that exists:
// a new Secret starts a new file.
func (n NewSecret) check(file string) error {
	if errs := validation.IsDNS1123Subdomain(n.Name); len(errs) > 0 {
		return fmt.Errorf("--name %q: %s", n.Name, errs[0])
	}
	if errs := validation.IsDNS1123Label(n.Namespace); len(errs) > 0 {
		return fmt.Errorf("--namespace %q: %s", n.Namespace, errs[0])
	}
	if _, err := os.Stat(file); err == nil {
		return fmt.Errorf("%s exists: --name and --namespace start a new file", file)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// document is the Secret without values.
func (n NewSecret) document() *document {
	d := newDocument()
	for _, kv := range [][2]string{{"apiVersion", "v1"}, {"kind", secretKind}, {"metadata.name", n.Name}, {"metadata.namespace", n.Namespace}, {"type", "Opaque"}} {
		_ = d.set(kv[0], kv[1]) // fixed paths into an empty mapping
	}
	return d
}

// sopsMetadata reports whether raw carries sops metadata.
func sopsMetadata(raw []byte) bool {
	var doc struct {
		SOPS map[string]any `yaml:"sops"`
	}
	return yaml.Unmarshal(raw, &doc) == nil && doc.SOPS != nil
}

// skeleton is a plaintext Kubernetes Secret that holds no value yet, the
// start of a new SOPS file; nil for a file with sops metadata, which sops
// reads. Every other plaintext file is refused before sops sees it, by what
// it is (a ConfigMap, no YAML mapping, a Secret without metadata or holding
// a value), its content never quoted: sops' own "sops metadata not found"
// names neither the cause nor the way on.
func skeleton(raw []byte) (*document, error) {
	if sopsMetadata(raw) {
		return nil, nil
	}
	doc, err := parseDocument(raw)
	if err != nil {
		return nil, noSkeleton("no sops metadata and no YAML mapping")
	}
	k, _ := doc.get("kind")
	switch {
	case k == "":
		return nil, noSkeleton("a plaintext YAML document without a kind, no sops metadata")
	case k != secretKind:
		return nil, noSkeleton("a plaintext " + k + ", no sops metadata")
	case child(doc.root, "metadata") == nil:
		return nil, noSkeleton("a plaintext Secret without metadata, no sops metadata")
	}
	for p, v := range doc.leaves() {
		if v != "" && (strings.HasPrefix(p, "data.") || strings.HasPrefix(p, "stringData.")) {
			return nil, fmt.Errorf("a plaintext Secret holding a value at %s: a skeleton holds none, encrypt it or move it aside", p)
		}
	}
	return doc, nil
}

// noSkeleton is the refusal of a plaintext file that is no Secret skeleton:
// what the file is, and the way on.
func noSkeleton(what string) error {
	return fmt.Errorf("%s: a value goes into a sops-encrypted file or a plaintext Secret skeleton; pick the encrypted Secret, or start one in an absent file with --name and --namespace", what)
}

// Key is a key name and the length of its value, all an answer tells.
type Key struct {
	Name  string `json:"name"`
	Bytes int    `json:"bytes"`
}

// keys are the secret keys of a document with their lengths: a Kubernetes
// Secret's data (decoded) and stringData, every leaf of anything else.
func (d *document) keys() []Key {
	leaves := d.leaves()
	secret := false
	if k, ok := d.get("kind"); ok && k == secretKind {
		secret = true
	}
	var out []Key
	for _, p := range sortedKeys(leaves) {
		v := leaves[p]
		switch {
		case !secret:
		case strings.HasPrefix(p, "data."):
			if b, err := base64.StdEncoding.DecodeString(v); err == nil {
				v = string(b)
			}
		case strings.HasPrefix(p, "stringData."):
		default:
			continue
		}
		out = append(out, Key{Name: p, Bytes: len(v)})
	}
	return out
}
