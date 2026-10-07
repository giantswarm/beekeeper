package config

import (
	"bytes"
	"cmp"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// ErrInvalid refuses a Set whose result does not load.
var ErrInvalid = errors.New("refused")

// Setting is one key of a Set: a dotted path of mapping keys
// (secret.store.read) and its value in YAML.
type Setting struct {
	Key, Value string
}

// Set writes every setting into the file at path in one write: the result
// is validated before it replaces the file, which a reader sees either
// before or after, never between two settings. A key whose maps do not
// exist yet is created; comments and the order of the other keys stay. It
// returns the configuration written.
func Set(path string, settings []Setting) (*Config, error) {
	if len(settings) == 0 {
		return nil, errors.New("no setting")
	}
	// A configuration kept elsewhere behind a link (a dotfiles checkout)
	// is replaced where it lives, the link kept.
	target, err := filepath.EvalSymlinks(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		target = path
	case err != nil:
		return nil, err
	}
	raw, err := os.ReadFile(filepath.Clean(target))
	mode := fs.FileMode(0o600)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return nil, err
	default:
		if fi, err := os.Stat(target); err == nil {
			mode = fi.Mode().Perm()
		}
	}
	out, err := apply(raw, settings)
	if err != nil {
		return nil, err
	}
	c, err := parse(path, out)
	if err != nil {
		return nil, fmt.Errorf("%w, %s is unchanged: %w", ErrInvalid, path, err)
	}
	return c, writeAtomic(target, out, mode)
}

// apply is the document raw with every setting set.
func apply(raw []byte, settings []Setting) ([]byte, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	if doc.Kind == 0 {
		doc = yaml.Node{Kind: yaml.DocumentNode}
	}
	if len(doc.Content) == 0 || doc.Content[0].Tag == "!!null" {
		// an empty file, or one of comments alone
		doc.Content = []*yaml.Node{{Kind: yaml.MappingNode, Tag: "!!map"}}
	}
	for _, s := range settings {
		var v yaml.Node
		if err := yaml.Unmarshal([]byte(s.Value), &v); err != nil {
			return nil, fmt.Errorf("%s: %w", s.Key, err)
		}
		value := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!null"}
		if v.Kind == yaml.DocumentNode {
			value = v.Content[0]
		}
		if err := setKey(doc.Content[0], s.Key, value); err != nil {
			return nil, err
		}
	}
	var b bytes.Buffer
	enc := yaml.NewEncoder(&b)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return nil, err
	}
	return b.Bytes(), enc.Close()
}

// setKey sets the dotted key under the mapping m to value.
func setKey(m *yaml.Node, key string, value *yaml.Node) error {
	parts := strings.Split(key, ".")
	for i, p := range parts {
		if p == "" {
			return fmt.Errorf("%s: an empty key", key)
		}
		if m.Kind != yaml.MappingNode {
			return fmt.Errorf("%s: %s is no map", key, cmp.Or(strings.Join(parts[:i], "."), "the document"))
		}
		var next *yaml.Node
		for j := 0; j+1 < len(m.Content); j += 2 {
			if m.Content[j].Value == p {
				next = m.Content[j+1]
				if i == len(parts)-1 {
					// the key's own comments stay with it
					value.HeadComment, value.LineComment = next.HeadComment, next.LineComment
					m.Content[j+1] = value
					return nil
				}
				break
			}
		}
		if next == nil {
			next = value
			if i < len(parts)-1 {
				next = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
			}
			m.Content = append(m.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: p}, next)
		}
		m = next
	}
	return nil
}

// writeAtomic replaces the file at path with b in one rename.
func writeAtomic(path string, b []byte, mode fs.FileMode) error {
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name()) //nolint:errcheck // gone after the rename
	if _, err := f.Write(b); err != nil {
		f.Close() //nolint:errcheck,gosec // the write failed already
		return err
	}
	if err := f.Chmod(mode); err != nil {
		f.Close() //nolint:errcheck,gosec // the chmod failed already
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close() //nolint:errcheck,gosec // the sync failed already
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}
