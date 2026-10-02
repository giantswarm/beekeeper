package secret

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
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
	return &document{root: &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}}
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
	n := d.root
	for _, k := range strings.Split(path, ".") {
		n = child(n, k)
		if n == nil {
			return "", false
		}
	}
	if n.Kind != yaml.ScalarNode {
		return "", false
	}
	return n.Value, true
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
	keys := strings.Split(path, ".")
	n := d.root
	for i, k := range keys {
		if k == "" {
			return fmt.Errorf("%q: an empty key", path)
		}
		c := child(n, k)
		last := i == len(keys)-1
		switch {
		case c == nil && n.Kind != yaml.MappingNode:
			return fmt.Errorf("%q: %s is no mapping", path, strings.Join(keys[:i], "."))
		case c == nil:
			c = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
			if last {
				c = &yaml.Node{}
			}
			n.Content = append(n.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: k}, c)
		case last && c.Kind != yaml.ScalarNode:
			return fmt.Errorf("%q holds a %s, not a value", path, kindName(c.Kind))
		}
		n = c
	}
	*n = yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value}
	return nil
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
	if k, ok := d.get("kind"); ok && k == "Secret" {
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
