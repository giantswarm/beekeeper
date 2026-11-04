package config

import (
	"bytes"
	"cmp"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"

	"github.com/gofrs/flock"
	"gopkg.in/yaml.v3"
)

const (
	yamlNullTag    = "!!null"
	yamlMappingTag = "!!map"
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
	return Change(path, settings, nil)
}

// Change is Set with the dotted keys of unset removed in the same write,
// after the settings: a mapping an unset key leaves empty goes with it.
// Concurrent Change/Set calls serialize the entire read/modify/write transaction
// for the resolved target. Separate caller Get/Set operations are not atomic.
func Change(path string, settings []Setting, unset []string) (*Config, error) {
	if len(settings) == 0 && len(unset) == 0 {
		return nil, errors.New("no change")
	}
	// A configuration kept elsewhere behind a link (a dotfiles checkout)
	// is replaced where it lives, the link kept.
	target, err := filepath.EvalSymlinks(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		// Resolve the parent too when creating a new file, so a directory
		// alias and a direct path use the same lock from the first edit.
		parent, resolveErr := filepath.EvalSymlinks(filepath.Dir(path))
		if resolveErr != nil {
			return nil, resolveErr
		}
		target = filepath.Join(parent, filepath.Base(path))
	case err != nil:
		return nil, err
	}
	// Lock a persistent sidecar, never the configuration inode: rename replaces
	// that inode, and removing the sidecar would split waiting editors' locks.
	lock := flock.New(target+".lock", flock.SetPermissions(0o600))
	if err := lock.Lock(); err != nil {
		return nil, fmt.Errorf("lock configuration: %w", err)
	}
	defer func() { _ = lock.Unlock() }()
	raw, err := os.ReadFile(filepath.Clean(target)) //nolint:gosec // the configuration path comes from the user
	mode := fs.FileMode(0o600)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return nil, err
	default:
		if fi, err := os.Stat(target); err == nil { //nolint:gosec // the configuration path comes from the user
			mode = fi.Mode().Perm()
		}
	}
	out, err := apply(raw, settings, unset)
	if err != nil {
		return nil, err
	}
	c, err := parse(path, out)
	if err != nil {
		return nil, fmt.Errorf("%w, %s is unchanged: %w", ErrInvalid, path, err)
	}
	return c, writeAtomic(target, out, mode)
}

// apply is the document raw with every setting set and every key of unset
// removed.
func apply(raw []byte, settings []Setting, unset []string) ([]byte, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	if doc.Kind == 0 {
		doc = yaml.Node{Kind: yaml.DocumentNode}
	}
	if len(doc.Content) == 0 || doc.Content[0].Tag == yamlNullTag {
		// an empty file, or one of comments alone
		doc.Content = []*yaml.Node{{Kind: yaml.MappingNode, Tag: yamlMappingTag}}
	}
	for _, s := range settings {
		var v yaml.Node
		if err := yaml.Unmarshal([]byte(s.Value), &v); err != nil {
			return nil, fmt.Errorf("%s: %w", s.Key, err)
		}
		value := &yaml.Node{Kind: yaml.ScalarNode, Tag: yamlNullTag}
		if v.Kind == yaml.DocumentNode {
			value = v.Content[0]
		}
		if err := setKey(doc.Content[0], s.Key, value); err != nil {
			return nil, err
		}
	}
	for _, k := range unset {
		if err := unsetKey(doc.Content[0], strings.Split(k, ".")); err != nil {
			return nil, fmt.Errorf("%s: %w", k, err)
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
				next = &yaml.Node{Kind: yaml.MappingNode, Tag: yamlMappingTag}
			}
			m.Content = append(m.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: p}, next)
		}
		m = next
	}
	return nil
}

// Get is the value of the dotted key in the file at path as YAML, and
// whether the file has it; a missing file has no key. It reads the file as
// Set writes it, without loading the configuration.
func Get(path, key string) (string, bool, error) {
	raw, err := os.ReadFile(filepath.Clean(path))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "", false, nil
	case err != nil:
		return "", false, err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return "", false, err
	}
	if len(doc.Content) == 0 {
		return "", false, nil
	}
	m := doc.Content[0]
	for _, p := range strings.Split(key, ".") {
		if m.Kind != yaml.MappingNode {
			return "", false, nil
		}
		var next *yaml.Node
		for j := 0; j+1 < len(m.Content); j += 2 {
			if m.Content[j].Value == p {
				next = m.Content[j+1]
				break
			}
		}
		if next == nil {
			return "", false, nil
		}
		m = next
	}
	// The value alone: the key's comments stay in the file.
	v := *m
	v.HeadComment, v.LineComment, v.FootComment = "", "", ""
	out, err := yaml.Marshal(&v)
	if err != nil {
		return "", false, err
	}
	return strings.TrimSuffix(string(out), "\n"), true, nil
}

// SameValue reports whether two YAML values are the same value, however
// each is written ({a: 1} and a block mapping alike).
func SameValue(a, b string) bool {
	var x, y any
	if yaml.Unmarshal([]byte(a), &x) != nil || yaml.Unmarshal([]byte(b), &y) != nil {
		return a == b
	}
	return reflect.DeepEqual(x, y)
}

// unsetKey removes the key at path under the mapping m, and a mapping it
// leaves empty; a key that is missing already is no error.
func unsetKey(m *yaml.Node, path []string) error {
	if slices.Contains(path, "") {
		return errors.New("an empty key")
	}
	if m.Kind != yaml.MappingNode {
		return errors.New("the document is no map")
	}
	for j := 0; j+1 < len(m.Content); j += 2 {
		if m.Content[j].Value != path[0] {
			continue
		}
		if len(path) > 1 {
			next := m.Content[j+1]
			if next.Kind != yaml.MappingNode {
				return nil
			}
			if err := unsetKey(next, path[1:]); err != nil {
				return err
			}
			if len(next.Content) > 0 {
				return nil
			}
		}
		m.Content = append(m.Content[:j], m.Content[j+2:]...)
		return nil
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
	return os.Rename(f.Name(), path) //nolint:gosec // the configuration path comes from the user
}
