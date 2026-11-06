package state

import (
	"encoding/json"
	"errors"
	"os"

	"github.com/Masterminds/semver/v3"
)

// RecoverVersion executes a binary recovery while holding the state lock. It
// validates the live state with the recovery version's fence, without lowering
// the writer stamp or restoring a snapshot. Future schemas need an explicit
// lossless migration before this method can permit an older writer. failed is
// the release being rolled back: the fence stops its processes, not a later
// release. A prerelease neither stamps the state nor is judged by the stamp, so
// a prerelease candidate cannot have advanced the writer past its recovery.
func (s *FileStore) RecoverVersion(version, failed string, restore func() error) error {
	own, ok := fenceVersion(version)
	if !ok {
		return errors.New("recovery requires a release version")
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
	if st.Writer == nil && own.Prerelease() == "" {
		if _, err := os.Stat(s.path("state.json")); err == nil {
			return errors.New("unversioned live state requires an explicit compatibility migration")
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if st.Writer != nil {
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
	if err := s.writeRecoveryFence(version, failed); err != nil {
		return err
	}
	return restore()
}

// fenceVersion is v as a version the recovery fence can order: a release or a
// prerelease, no build metadata. A development build has none.
func fenceVersion(v string) (*semver.Version, bool) {
	r, err := semver.NewVersion(v)
	if err != nil || r.Metadata() != "" {
		return nil, false
	}
	return r, true
}

const recoveryFenceFile = "recovery-writer.json"

type recoveryFence struct {
	Maximum string `json:"maximum"`
	// Failed is the rolled-back release. A release past it is a later one, whose
	// processes the fence must not stop: it would never pass its probation.
	Failed string `json:"failed,omitempty"`
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
	maximum, ok := fenceVersion(fence.Maximum)
	if !ok {
		return errors.New("invalid recovery writer fence")
	}
	own, ok := fenceVersion(s.version)
	if ok && !own.GreaterThan(maximum) {
		return nil
	}
	if failed, known := fenceVersion(fence.Failed); ok && known && own.GreaterThan(failed) {
		return nil
	}
	return errors.New("state writes fenced after binary recovery; complete probation of a compatible signed release before resuming newer writers")
}

func (s *FileStore) writeRecoveryFence(version, failed string) error {
	raw, err := json.Marshal(recoveryFence{Maximum: version, Failed: failed})
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
	own, ok := fenceVersion(version)
	if !ok {
		return errors.New("resuming writes requires a release version")
	}
	reader := &FileStore{dir: s.dir, version: version}
	st, err := reader.load()
	if err != nil {
		return err
	}
	if st.Writer != nil {
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
