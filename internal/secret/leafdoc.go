package secret

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

// leafDoc is a string leaf of a SOPS file that a dotted path reaches
// through: dex-operator's credentials, a block scalar holding a small YAML
// document. held are the leaf's keys as the file holds them, rest the keys
// left inside the document it holds.
type leafDoc struct {
	node *yaml.Node
	held []string
	rest []string
}

// at is the leaf's dotted path.
func (l leafDoc) at() string { return strings.Join(l.held, ".") }

// inLeaf is the string leaf keys reach before their end, the keys of a
// skeleton's empty key (`stringData:`) passed as a mapping to come; false
// when keys end on a node or reach nothing.
func (d *document) inLeaf(keys []string) (leafDoc, bool) {
	n := d.root
	var held []string
	for i := 0; i < len(keys); {
		if n.Kind == yaml.AliasNode {
			n = n.Alias
		}
		if n.Kind == yaml.ScalarNode {
			if n.Tag == "!!null" {
				return leafDoc{}, false
			}
			return leafDoc{node: n, held: held, rest: keys[i:]}, true
		}
		c, width := childAt(n, keys[i:])
		if c == nil {
			return leafDoc{}, false
		}
		held = append(held, strings.Join(keys[i:i+width], "."))
		n, i = c, i+width
	}
	return leafDoc{}, false
}

// leafValue is the value path names inside the document a string leaf on
// the way holds, one level down as a Kubernetes key's content reads; a path
// that reaches no such leaf, or nothing inside it, fails naming the shape,
// never a value.
func (d *document) leafValue(path string) (string, error) {
	l, ok := d.inLeaf(splitPath(path))
	if !ok {
		return "", fmt.Errorf("no value at %s", path)
	}
	return lookup([]byte(l.node.Value), l.at(), l.rest)
}

// setInLeaf sets one key of the YAML mapping document a string leaf
// holds to value: the key's line is rewritten in place, an absent key
// appended as one line, every other line kept byte for byte. The edited
// document is read again and refused unless the key alone changed, naming
// keys, never a value.
func (l leafDoc) set(value string) error {
	raw := l.node.Value
	at := l.at()
	root, err := parseNode([]byte(raw))
	if err != nil || root.Kind != yaml.MappingNode || root.Style&yaml.FlowStyle != 0 || len(root.Content) == 0 {
		return fmt.Errorf("%s holds %s: a key is set inside a block YAML mapping only", at, nodeShape(l.node))
	}
	doc := &document{root: root}
	key := strings.Join(l.rest, ".")
	if child(root, key) == nil && len(l.rest) > 1 {
		return fmt.Errorf("%s: set writes a top-level key of the document %s holds, which has the keys %s",
			joinPath(at, key), at, namedKeys(doc.topKeys()))
	}
	line, err := oneLine(value)
	if err != nil {
		return fmt.Errorf("%s: %w", joinPath(at, key), err)
	}
	edited, err := setLine(root, raw, key, line)
	if err != nil {
		return fmt.Errorf("%s: %w", joinPath(at, key), err)
	}
	if err := sameBesides(doc, edited, key, value); err != nil {
		return fmt.Errorf("%s: %w; nothing written", joinPath(at, key), err)
	}
	l.node.Value = edited
	return nil
}

// oneLine is v as a YAML scalar on one line: plain when YAML reads it back
// as the same string, quoted otherwise. A value of several lines is
// refused.
func oneLine(v string) (string, error) {
	b, err := yaml.Marshal(&yaml.Node{Kind: yaml.ScalarNode, Tag: strTag, Value: v})
	if err != nil {
		return "", err
	}
	s := strings.TrimSuffix(string(b), "\n")
	if strings.Contains(s, "\n") {
		return "", errors.New("a key inside a document takes a value of one line")
	}
	return s, nil
}

// setLine is raw, the text of root, with key's line holding value, a YAML
// scalar of one line: an existing key keeps its indent, its key text and
// its comment, an absent one is appended at the indent of the first key.
func setLine(root *yaml.Node, raw, key, value string) (string, error) {
	lines := strings.Split(raw, "\n")
	for i := 0; i+1 < len(root.Content); i += 2 {
		k, v := root.Content[i], root.Content[i+1]
		if k.Value != key {
			continue
		}
		if v.Kind != yaml.ScalarNode || v.Line != k.Line || v.Style&(yaml.LiteralStyle|yaml.FoldedStyle) != 0 {
			return "", fmt.Errorf("the key holds %s, not a value on its line", nodeShape(v))
		}
		text := []rune(strings.TrimSuffix(lines[k.Line-1], "\r"))
		cr := strings.TrimPrefix(lines[k.Line-1], string(text))
		comment := v.LineComment
		if comment == "" {
			comment = k.LineComment
		}
		var head string
		if v.Value == "" && v.Style == 0 {
			// key: with nothing after it but a comment
			head = string(text)
			if comment != "" {
				head = head[:strings.LastIndex(head, comment)]
			}
			head = strings.TrimRight(head, " \t") + " "
		} else {
			head = string(text[:v.Column-1])
		}
		if comment != "" {
			value += " " + comment
		}
		lines[k.Line-1] = head + value + cr
		return strings.Join(lines, "\n"), nil
	}
	first := root.Content[0]
	indent := string([]rune(lines[first.Line-1])[:first.Column-1])
	k, err := oneLine(key)
	if err != nil {
		return "", err
	}
	added := indent + k + ": " + value
	if strings.HasSuffix(raw, "\n") {
		return raw + added + "\n", nil
	}
	return raw + "\n" + added, nil
}

// sameBesides refuses an edit of before into edited that changed anything
// but key, or left key without value, naming the keys, never a value.
func sameBesides(before *document, edited, key, value string) error {
	after, err := parseDocument([]byte(edited))
	if err != nil {
		return fmt.Errorf("the edited document reads as no YAML mapping")
	}
	if v := child(after.root, key); v == nil || v.Kind != yaml.ScalarNode || v.Value != value {
		return errors.New("the edited document does not hold the value at the key")
	}
	var changed []string
	for _, k := range before.topKeys() {
		if k != key && !sameNode(child(before.root, k), child(after.root, k)) {
			changed = append(changed, k)
		}
	}
	for _, k := range after.topKeys() {
		if k != key && child(before.root, k) == nil {
			changed = append(changed, k)
		}
	}
	if len(after.topKeys()) != len(before.topKeys())+boolInt(child(before.root, key) == nil) {
		changed = append(changed, key)
	}
	if len(changed) > 0 {
		slices.Sort(changed)
		return fmt.Errorf("the edit would change %s besides the key", namedKeys(slices.Compact(changed)))
	}
	return nil
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
