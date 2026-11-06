package state

import (
	"encoding/json"
	"errors"
	"os"
)

// RecoverVersion executes a binary recovery while holding the state lock. It
// validates the live state with the recovery version's fence, without lowering
// the writer stamp or restoring a snapshot. Future schemas need an explicit
// lossless migration before this method can permit an older writer.
func (s *FileStore) RecoverVersion(version string, restore func() error) error {
	if _, ok := release(version); !ok {
		return errors.New("recovery requires a stable release version")
	}
	unlock, err := s.lock(false, lockWait)
	if err != nil {
		return err
	}
	defer unlock()
	reader := &FileStore{dir: s.dir, version: version}
	st, err := reader.load()
	if err != nil {
		return err
	}
	if st.Writer == nil {
		if _, err := os.Stat(s.path("state.json")); err == nil {
			return errors.New("unversioned live state requires an explicit compatibility migration")
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if st.Writer != nil {
		own, _ := release(version)
		writer, ok := release(st.Writer.Version)
		if !ok {
			return errors.New("state writer compatibility is unknown")
		}
		if own.LessThan(writer) {
			return errors.New("recovery binary is older than the live state writer; a lossless compatible migration is required")
		}
	}
	// Persist the ceiling before restoring the executable: candidate processes
	// already running must not advance the writer after the compatibility check.
	if err := s.writeRecoveryFence(version); err != nil {
		return err
	}
	return restore()
}

const recoveryFenceFile = "recovery-writer.json"

type recoveryFence struct {
	Maximum string `json:"maximum"`
}

func (s *FileStore) checkRecoveryFence() error {
	var fence recoveryFence
	found, err := s.ReadFile(recoveryFenceFile, &fence)
	if err != nil {
		return err
	}
	if !found {
		return nil
	}
	maximum, ok := release(fence.Maximum)
	if !ok {
		return errors.New("invalid recovery writer fence")
	}
	own, ok := release(s.version)
	if !ok || own.GreaterThan(maximum) {
		return errors.New("state writes fenced after binary recovery; complete probation of a compatible signed release before resuming newer writers")
	}
	return nil
}

func (s *FileStore) writeRecoveryFence(version string) error {
	raw, err := json.Marshal(recoveryFence{Maximum: version})
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(s.dir, ".recovery-fence-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(f.Name()) }()
	_, err = f.Write(raw)
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err := os.Rename(f.Name(), s.path(recoveryFenceFile)); err != nil {
		return err
	}
	dir, err := os.Open(s.dir)
	if err != nil {
		return err
	}
	defer func() { _ = dir.Close() }()
	return dir.Sync()
}

// ResumeRecoveredWrites removes the recovery ceiling after an operator has
// explicitly completed probation of a compatible installed release. It changes
// no work, approval, note, writer stamp, or event history.
func (s *FileStore) ResumeRecoveredWrites(version string) error {
	unlock, err := s.lock(false, lockWait)
	if err != nil {
		return err
	}
	defer unlock()
	var fence recoveryFence
	found, err := s.ReadFile(recoveryFenceFile, &fence)
	if err != nil {
		return err
	}
	if !found {
		return nil
	}
	if _, ok := release(version); !ok {
		return errors.New("resuming writes requires a stable release")
	}
	reader := &FileStore{dir: s.dir, version: version}
	st, err := reader.load()
	if err != nil {
		return err
	}
	if st.Writer != nil {
		own, _ := release(version)
		writer, ok := release(st.Writer.Version)
		if !ok || own.LessThan(writer) {
			return errors.New("installed release cannot write live state")
		}
	}
	if err := os.Remove(s.path(recoveryFenceFile)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	dir, err := os.Open(s.dir)
	if err != nil {
		return err
	}
	defer func() { _ = dir.Close() }()
	return dir.Sync()
}
