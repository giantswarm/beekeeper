package lease

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

// labConfigFile is the claim-time copy of a lab's agentlab.yaml in its
// lease directory; it goes with the lease.
const labConfigFile = "agentlab.yaml"

// LabConfig is a lab's agentlab.yaml as the claim of its lease found it.
type LabConfig struct {
	Path string `json:"path"`
	// SHA256 is the hash of the parsed configuration, so a comment agentlab
	// rewrites (its version line) is no change.
	SHA256      string   `json:"sha256"`
	ValuesFiles []string `json:"valuesFiles,omitempty"`
}

// LabChange is a lab's configuration a release left different from its
// claim-time state, kept for the next claim of the lab.
type LabChange struct {
	By   string   `json:"by"`
	At   string   `json:"at"`
	Keys []string `json:"keys"`
	// Why is the holder's declaration (release --keep-config), empty for a
	// change released with --force and not declared.
	Why string `json:"why,omitempty"`
}

// Text says what the change is, for a claim and a status.
func (c LabChange) Text() string {
	why := "not declared"
	if c.Why != "" {
		why = c.Why
	}
	return fmt.Sprintf("left changed by %q at %s in %s: %s", c.By, c.At, strings.Join(c.Keys, ", "), why)
}

// ReadLabConfig reads the agentlab.yaml at path: its record and its bytes.
func ReadLabConfig(path string) (LabConfig, []byte, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // the lab's own configuration
	if err != nil {
		return LabConfig{}, nil, err
	}
	tree, err := parseLab(raw)
	if err != nil {
		return LabConfig{}, nil, fmt.Errorf("%s: %w", path, err)
	}
	canon, err := yaml.Marshal(tree)
	if err != nil {
		return LabConfig{}, nil, err
	}
	sum := sha256.Sum256(canon)
	c := LabConfig{Path: path, SHA256: hex.EncodeToString(sum[:])}
	if p, ok := tree["platform"].(map[string]any); ok {
		if vf, ok := p["valuesFiles"].([]any); ok {
			for _, f := range vf {
				c.ValuesFiles = append(c.ValuesFiles, fmt.Sprint(f))
			}
		}
	}
	return c, raw, nil
}

func parseLab(raw []byte) (map[string]any, error) {
	tree := map[string]any{}
	if err := yaml.Unmarshal(raw, &tree); err != nil {
		return nil, err
	}
	return tree, nil
}

// LabDiff names the keys in which two agentlab.yaml files differ, as dotted
// paths to the first value that is no map (a list is one value), sorted.
func LabDiff(was, now []byte) ([]string, error) {
	a, err := parseLab(was)
	if err != nil {
		return nil, err
	}
	b, err := parseLab(now)
	if err != nil {
		return nil, err
	}
	var keys []string
	diffTree("", a, b, &keys)
	slices.Sort(keys)
	return keys, nil
}

func diffTree(prefix string, a, b map[string]any, keys *[]string) {
	names := slices.Collect(maps.Keys(a))
	for k := range b {
		if _, ok := a[k]; !ok {
			names = append(names, k)
		}
	}
	for _, k := range names {
		path := prefix + k
		va, vb := a[k], b[k]
		ma, aMap := va.(map[string]any)
		mb, bMap := vb.(map[string]any)
		switch {
		case aMap && bMap:
			diffTree(path+".", ma, mb, keys)
		case !reflect.DeepEqual(va, vb):
			*keys = append(*keys, path)
		}
	}
}

// KeepLabConfig keeps raw, the lab's agentlab.yaml at the claim of res, in
// the lease of res.
func (d Dir) KeepLabConfig(res string, raw []byte) error {
	return os.WriteFile(filepath.Join(d.path(res), labConfigFile), raw, 0o600)
}

// ClaimedLabConfig is the agentlab.yaml the claim of res kept.
func (d Dir) ClaimedLabConfig(res string) ([]byte, error) {
	return os.ReadFile(filepath.Join(d.path(res), labConfigFile))
}

// changePath is where a released lab's change waits for the next claim:
// beside the leases, a dot file List skips.
func (d Dir) changePath(res string) string {
	return filepath.Join(string(d), ".labchange-"+res+".json")
}

// LeaveLabChange keeps c for the next claim of res.
func (d Dir) LeaveLabChange(res string, c LabChange) error {
	raw, err := json.Marshal(c)
	if err != nil {
		return err
	}
	return os.WriteFile(d.changePath(res), append(raw, '\n'), 0o600)
}

// LabChangeOf is the change a release of res left for its next claim, nil
// when none waits.
func (d Dir) LabChangeOf(res string) (*LabChange, error) {
	raw, err := os.ReadFile(d.changePath(res))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	c := &LabChange{}
	if err := json.Unmarshal(raw, c); err != nil {
		return nil, fmt.Errorf("%s: %w", d.changePath(res), err)
	}
	return c, nil
}

// DropLabChange ends the wait of the change left for res: its claim took it.
func (d Dir) DropLabChange(res string) error {
	if err := os.Remove(d.changePath(res)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// RestoreLabConfig writes the claim-time agentlab.yaml of res back to path
// in one rename; the configuration it replaces is kept as path.released.
// It returns where that copy is.
func (d Dir) RestoreLabConfig(res, path string) (string, error) {
	raw, err := d.ClaimedLabConfig(res)
	if err != nil {
		return "", err
	}
	mode := os.FileMode(0o600)
	if fi, err := os.Stat(path); err == nil {
		mode = fi.Mode().Perm()
	}
	aside := path + ".released"
	if cur, err := os.ReadFile(path); err == nil { //nolint:gosec // the lab's own configuration
		if err := os.WriteFile(aside, cur, mode); err != nil { //nolint:gosec // beside the lab's own configuration
			return "", err
		}
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".agentlab-*.yaml")
	if err != nil {
		return "", err
	}
	tmp := f.Name()
	_, err = f.Write(raw)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(tmp, mode)
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	return aside, nil
}
