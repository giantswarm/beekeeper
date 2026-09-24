package alerts

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/gofrs/flock"
)

// State is the alert baseline: every installation's last set, and the watch
// that owns it, so two watches during a hand-over never split the NEW and
// RESOLVED lines between them.
type State struct {
	Owner         *Owner                   `json:"owner,omitempty"`
	Installations map[string]*Installation `json:"installations"`
}

// Owner is the process that reads the alerts and keeps the baseline.
type Owner struct {
	PID   int       `json:"pid"`
	Since time.Time `json:"since"`
}

// Store keeps the baseline in alerts.json in beekeeper's state directory.
// Only the process holding alerts.lock reads alerts into it.
type Store struct {
	dir  string
	lock *flock.Flock
}

// NewStore returns the baseline kept in dir.
func NewStore(dir string) *Store {
	return &Store{dir: dir, lock: flock.New(filepath.Join(dir, "alerts.lock"))}
}

func (s *Store) path() string { return filepath.Join(s.dir, "alerts.json") }

// Own makes this process the baseline's owner unless another one is; then it
// returns false and that owner. Owning lasts until Release or the process ends.
func (s *Store) Own() (bool, *Owner, error) {
	if s.lock.Locked() {
		return true, nil, nil
	}
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return false, nil, err
	}
	ok, err := s.lock.TryLock()
	if err != nil {
		return false, nil, err
	}
	st, lerr := s.Load()
	if !ok {
		if lerr != nil || st.Owner == nil {
			return false, &Owner{}, lerr
		}
		return false, st.Owner, nil
	}
	if lerr != nil {
		_ = s.Release()
		return false, nil, lerr
	}
	st.Owner = &Owner{PID: os.Getpid(), Since: time.Now().UTC()}
	return true, nil, s.Save(st)
}

// Release gives the baseline up.
func (s *Store) Release() error {
	if !s.lock.Locked() {
		return nil
	}
	return s.lock.Unlock()
}

// Load reads the baseline; a missing file is an empty one.
func (s *Store) Load() (*State, error) {
	st := &State{}
	raw, err := os.ReadFile(s.path())
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return nil, err
	default:
		if err := json.Unmarshal(raw, st); err != nil {
			return nil, fmt.Errorf("%s: %w", s.path(), err)
		}
	}
	if st.Installations == nil {
		st.Installations = map[string]*Installation{}
	}
	return st, nil
}

// Save writes the baseline with one rename.
func (s *Store) Save(st *State) error {
	raw, err := json.Marshal(st)
	if err != nil {
		return err
	}
	tmp := s.path() + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path())
}

// Import adds the per-installation baselines in dir (one <installation>.json
// each, as {"reachable": …, "alerts": {…}}) for every installation the
// baseline does not know yet, so a switch from another watcher prints no
// burst of NEW lines. It returns the imported installations.
func Import(st *State, dir string) ([]string, error) {
	files, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return nil, err
	}
	var done []string
	for _, f := range files {
		name := strings.TrimSuffix(filepath.Base(f), ".json")
		if _, ok := st.Installations[name]; ok {
			continue
		}
		raw, err := os.ReadFile(filepath.Clean(f))
		if err != nil {
			return done, err
		}
		in := &Installation{Reachable: true}
		if err := json.Unmarshal(raw, in); err != nil {
			return done, fmt.Errorf("%s: %w", f, err)
		}
		st.Installations[name] = in
		done = append(done, name)
	}
	slices.Sort(done)
	return done, nil
}
