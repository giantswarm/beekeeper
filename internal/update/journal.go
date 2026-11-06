package update

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/gofrs/flock"
)

const (
	probationPrepared    = "prepared"
	probationPending     = "pending"
	probationPassed      = "passed"
	probationRollingBack = "rolling-back"
	probationRolledBack  = "rolled-back"
)

// Binary identifies bytes verified by the release installer. A digest alone is
// not release evidence: only an installed, successfully probated candidate can
// become a recovery target.
type Binary struct {
	Version string `json:"version"`
	Digest  string `json:"sha256"`
}

// Journal is durable before executable replacement. Schema is fenced because
// guessing at a future journal's semantics could destroy its recovery target.
type Journal struct {
	Schema      int               `json:"schema"`
	Displaced   Binary            `json:"displaced"`
	Previous    *Binary           `json:"previous,omitempty"`
	Candidate   Binary            `json:"candidate"`
	InstalledAt time.Time         `json:"installedAt"`
	Probation   string            `json:"probation"`            // prepared, pending, passed, rolling-back, rolled-back
	Quarantine  map[string]string `json:"quarantine,omitempty"` // digest -> reason
	// Watched says a recovery controller supervises this candidate's probation:
	// its verdict decides, and an update waits for it.
	Watched bool `json:"watched,omitempty"`
	// Observed is the controller's durable record of this probation.
	Observed *Observed `json:"observed,omitempty"`
}

type installation struct {
	target, dir string
	// external says the recovery directory lies outside the executable's, which
	// this account cannot write: the replacement then takes privilege.
	external bool
	lock     *flock.Flock
	ctx      context.Context //nolint:containedctx // bounds the privileged step of this one locked transaction
}

// externalRecoveryDir is an installation's recovery directory in this account's
// state, named by the executable's path.
func externalRecoveryDir(target string) (string, error) {
	base := os.Getenv("XDG_STATE_HOME")
	if !filepath.IsAbs(base) {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		base = filepath.Join(home, ".local", "state")
	}
	sum := sha256.Sum256([]byte(target))
	return filepath.Join(base, "beekeeper", "update", hex.EncodeToString(sum[:16])), nil
}

// recoveryDir is where target's journal lives: beside it where this account can
// write, else in its state. A journal already recorded keeps its place, so
// changed directory permissions never lose a recovery target.
func recoveryDir(target string) (string, bool, error) {
	beside := target + ".update"
	if _, err := os.Lstat(beside); err == nil {
		return beside, false, nil
	}
	external, externalErr := externalRecoveryDir(target)
	if externalErr == nil {
		if _, err := os.Lstat(external); err == nil {
			return external, true, nil
		}
	}
	err := os.Mkdir(beside, 0o700)
	if err == nil || errors.Is(err, os.ErrExist) {
		return beside, false, nil
	}
	if !errors.Is(err, os.ErrPermission) || runtime.GOOS == windowsOS {
		return "", false, err
	}
	if externalErr != nil {
		return "", false, errors.Join(err, externalErr)
	}
	return external, true, os.MkdirAll(external, 0o700)
}

func openInstallation(ctx context.Context, path string) (*installation, error) {
	target, err := filepath.EvalSymlinks(path)
	if err != nil {
		return nil, err
	}
	target, err = filepath.Abs(target)
	if err != nil {
		return nil, err
	}
	dir, external, err := recoveryDir(target)
	if err != nil {
		return nil, err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o077 != 0 {
		return nil, errors.New("unsafe update directory")
	}
	if err := syncDir(filepath.Dir(dir)); err != nil {
		return nil, err
	}
	if err := regularOrMissing(filepath.Join(dir, "lock")); err != nil {
		return nil, err
	}
	l := flock.New(filepath.Join(dir, "lock"))
	ok, err := l.TryLockContext(ctx, 20*time.Millisecond)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ErrUpdateLocked
	}
	return &installation{target: target, dir: dir, external: external, lock: l, ctx: ctx}, nil
}
func (i *installation) close() { _ = i.lock.Unlock() }
func regularOrMissing(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("refusing non-regular recovery file %s", filepath.Base(path))
	}
	return nil
}
func digest(path string) (string, error) {
	if err := regularOrMissing(path); err != nil {
		return "", err
	}
	f, err := os.Open(filepath.Clean(path))
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
func (i *installation) artifact(b Binary) (string, error) {
	raw, err := hex.DecodeString(b.Digest)
	if err != nil || len(raw) != sha256.Size {
		return "", errors.New("invalid recovery digest")
	}
	return filepath.Join(i.dir, b.Digest), nil
}
func (i *installation) verify(b Binary) (string, error) {
	p, err := i.artifact(b)
	if err != nil {
		return "", err
	}
	d, err := digest(p)
	if err != nil {
		return "", err
	}
	if d != b.Digest {
		return "", errModifiedRecovery
	}
	return p, nil
}
func (i *installation) read() (*Journal, error) {
	p := filepath.Join(i.dir, "journal.json")
	if err := regularOrMissing(p); err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(filepath.Clean(p))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var j Journal
	if err := json.Unmarshal(raw, &j); err != nil {
		return nil, err
	}
	if j.Schema != 1 {
		return nil, errors.New("unsupported install journal schema")
	}
	if _, err := i.artifact(j.Candidate); err != nil {
		return nil, err
	}
	if j.Previous != nil {
		if _, err := i.artifact(*j.Previous); err != nil {
			return nil, err
		}
	}
	switch j.Probation {
	case probationPrepared, probationPending, probationPassed, probationRollingBack, probationRolledBack:
	default:
		return nil, errors.New("unknown probation state")
	}
	return &j, nil
}

// journalRename is a fault-injection seam for crash and ENOSPC tests.
var journalRename = os.Rename
var journalWrite = func(f *os.File, raw []byte) error { _, err := f.Write(raw); return err }

func syncDir(path string) error {
	f, err := os.Open(filepath.Clean(path))
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	return f.Sync()
}
func durableFile(path string, raw []byte, mode os.FileMode) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".write-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(f.Name()) }()
	if err = f.Chmod(mode); err == nil {
		err = journalWrite(f, raw)
	}
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
	if err = journalRename(f.Name(), path); err != nil {
		return err
	}
	return syncDir(filepath.Dir(path))
}
func (i *installation) write(j *Journal) error {
	raw, err := json.MarshalIndent(j, "", "  ")
	if err != nil {
		return err
	}
	return durableFile(filepath.Join(i.dir, "journal.json"), append(raw, '\n'), 0o600)
}
func (i *installation) retain(path string, b Binary) error {
	p, err := i.artifact(b)
	if err != nil {
		return err
	}
	if _, err := os.Lstat(p); err == nil {
		_, err = i.verify(b)
		return err
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	raw, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return err
	}
	d := sha256.Sum256(raw)
	if hex.EncodeToString(d[:]) != b.Digest {
		return errors.New("binary changed while retaining it")
	}
	return durableFile(p, raw, 0o700)
}

// recover reconciles a crash on either side of the executable rename. It
// never guesses from a version string or overwrites unexpected installed bytes.
func (i *installation) recover(j *Journal) error {
	if j == nil {
		return nil
	}
	if j.Previous != nil {
		if _, err := i.verify(*j.Previous); err != nil {
			return err
		}
	}
	if _, err := i.verify(j.Candidate); err != nil {
		return err
	}
	d, err := digest(i.target)
	if err != nil {
		return err
	}
	if j.Probation == probationPrepared {
		if d == j.Candidate.Digest {
			j.Probation = probationPending
			return i.write(j)
		}
		if d != j.Displaced.Digest {
			return errors.New("installed binary changed during prepared installation")
		}
		return nil
	}
	if j.Probation == probationRollingBack {
		if j.Previous == nil || (d != j.Previous.Digest && d != j.Candidate.Digest) {
			return errors.New("unexpected executable during rollback")
		}
		return nil
	}
	expected := j.Candidate.Digest
	if j.Probation == probationRolledBack && j.Previous != nil {
		expected = j.Previous.Digest
	}
	if d != expected {
		return errors.New("installed binary differs from recovery journal")
	}
	return nil
}

// swap atomically puts source's bytes at the target. Privilege is taken only
// for an installation this account cannot write.
func (i *installation) swap(source string) error {
	if !i.external {
		return journalRename(source, i.target)
	}
	info, err := os.Stat(i.target)
	if err != nil {
		return err
	}
	return privilegedInstall(i.ctx, source, i.target, strconv.FormatUint(uint64(info.Mode().Perm()), 8))
}

// promote makes the installed candidate last-known-good. Repeating it is safe.
func (i *installation) promote(j *Journal) error {
	j.Probation = probationPassed
	if err := i.write(j); err != nil {
		return err
	}
	return i.collect(j)
}

// settleProbation ends a pending probation before an update replaces its
// candidate, so updates need nobody to run --complete-probation. The installed
// candidate running this update is the evidence of its health, with health
// repeating the local checks where given. A failed check quarantines it: it is
// never promoted, and replace moves past it to the verified release. A process
// of another version proves nothing about the candidate, which stays pending.
//
// This touches the journal alone. A recovery fence on the state stays until
// self-update --complete-probation lifts it.
func (i *installation) settleProbation(ctx context.Context, health Health) error {
	j, err := i.read()
	if err != nil || j == nil {
		return err
	}
	if err := i.recover(j); err != nil {
		return err
	}
	if j.Probation != probationPending || strings.TrimPrefix(j.Candidate.Version, "v") != strings.TrimPrefix(currentVersion(), "v") {
		return nil
	}
	if j.Watched {
		return ErrProbationWatched
	}
	if _, failed := j.Quarantine[j.Candidate.Digest]; failed {
		return nil
	}
	if health != nil {
		if err := health(ctx, i.target, j.Candidate.Version); err != nil {
			if j.Quarantine == nil {
				j.Quarantine = map[string]string{}
			}
			j.Quarantine[j.Candidate.Digest] = "post-install health checks failed: " + err.Error()
			return i.write(j)
		}
	}
	return i.promote(j)
}

func (i *installation) replace(staged, version string) error {
	j, err := i.read()
	if err != nil {
		return err
	}
	if err := i.recover(j); err != nil {
		return err
	}
	if held := j.Held(); held != "" {
		return fmt.Errorf("recovery failed, updates are held: %s", held)
	}
	d, err := digest(staged)
	if err != nil {
		return err
	}
	candidate := Binary{Version: version, Digest: d}
	var previous *Binary
	quarantine := map[string]string{}
	// movingPast says the installed binary is a quarantined candidate, not the
	// recovery target, which stays the last known good.
	movingPast := false
	if j != nil {
		quarantine = j.Quarantine
		if quarantine == nil {
			quarantine = map[string]string{}
		}
		if reason, ok := quarantine[d]; ok {
			return fmt.Errorf("candidate quarantined: %s", reason)
		}
		switch j.Probation {
		case probationPassed:
			previous = &j.Candidate
		case probationRolledBack:
			previous = j.Previous
		case probationPrepared: // A prepared install can be retried only with identical bytes.
			if j.Candidate != candidate {
				return errors.New("unfinished installation requires the same candidate")
			}
			previous = j.Previous
		default:
			if _, failed := quarantine[j.Candidate.Digest]; !failed || j.Probation != probationPending {
				return errors.New("complete or roll back pending probation before another update")
			}
			previous, movingPast = j.Previous, true
		}
	}
	if previous != nil {
		current, err := digest(i.target)
		if err != nil {
			return err
		}
		if current != previous.Digest && !movingPast {
			return errors.New("installed recovery target was modified")
		}
		if _, err := i.verify(*previous); err != nil {
			return err
		}
	}
	if err := i.retain(staged, candidate); err != nil {
		return err
	}
	oldDigest, err := digest(i.target)
	if err != nil {
		return err
	}
	displacedVersion := currentVersion()
	if previous != nil {
		displacedVersion = previous.Version
	}
	if movingPast {
		displacedVersion = j.Candidate.Version
	}
	j = &Journal{Schema: 1, Displaced: Binary{Version: displacedVersion, Digest: oldDigest}, Previous: previous, Candidate: candidate, InstalledAt: time.Now().UTC(), Probation: probationPrepared, Quarantine: quarantine, Watched: previous != nil && optionsFrom(i.ctx).Recovery != nil}
	if err := i.write(j); err != nil {
		return err
	}
	if j.Watched && !i.launchController(previous) {
		j.Watched = false
		if err := i.write(j); err != nil {
			return err
		}
	}
	f, err := os.Open(filepath.Clean(staged))
	if err != nil {
		return err
	}
	err = f.Sync()
	_ = f.Close()
	if err != nil {
		return err
	}
	if err := i.swap(staged); err != nil {
		return err
	}
	if err := syncDir(filepath.Dir(i.target)); err != nil {
		return err
	}
	j.Probation = probationPending
	return i.write(j)
}

// CompleteProbation promotes only the running version after the caller has
// established health. Interrupted completion is safe to repeat.
func CompleteProbation(ctx context.Context, path, version string) error {
	i, err := openInstallation(ctx, path)
	if err != nil {
		return err
	}
	defer i.close()
	j, err := i.read()
	if err != nil {
		return err
	}
	if j == nil {
		return errors.New("no installation journal")
	}
	if err := i.recover(j); err != nil {
		return err
	}
	if _, failed := j.Quarantine[j.Candidate.Digest]; failed {
		return errors.New("quarantined candidate cannot be promoted")
	}
	if j.Candidate.Version != version || (j.Probation != probationPending && j.Probation != probationPassed) {
		return errors.New("running version is not the probation candidate")
	}
	return i.promote(j)
}

// Rollback requires a guard that holds the state lock, verifies the previous
// version can write the CURRENT state, and invokes restore under that lock.
// No state snapshot is ever restored. A nil guard is refused.
func Rollback(ctx context.Context, path, reason string, guard func(previous, failed string, restore func() error) error) error {
	if guard == nil || reason == "" {
		return errors.New("rollback requires a state compatibility guard and reason")
	}
	i, err := openInstallation(ctx, path)
	if err != nil {
		return err
	}
	defer i.close()
	j, err := i.read()
	if err != nil {
		return err
	}
	if j == nil || j.Previous == nil {
		return errors.New("no verified recovery target; install a compatible signed release")
	}
	if err := i.recover(j); err != nil {
		return err
	}
	if j.Probation == probationRolledBack {
		return nil
	}
	if j.Observed != nil {
		j.Observed.Held = ""
	}
	if j.Probation != probationPending && j.Probation != probationRollingBack {
		return errors.New("only a pending candidate can be rolled back")
	}
	if v, err := releaseSemver(j.Candidate.Version); err != nil || v.Metadata() != "" {
		return errors.New("unversioned candidate state cannot be recovered automatically")
	}
	p, err := i.verify(*j.Previous)
	if err != nil {
		return err
	}
	if j.Quarantine == nil {
		j.Quarantine = map[string]string{}
	}
	j.Quarantine[j.Candidate.Digest] = reason
	if err := i.write(j); err != nil {
		return err
	}
	return guard(j.Previous.Version, j.Candidate.Version, func() error {
		// Quarantine is durable before replacement. Recovery uses this intent to
		// finish the transaction after a crash, without touching any state data.
		j.Probation = probationRollingBack
		if err := i.write(j); err != nil {
			return err
		}
		raw, err := os.ReadFile(filepath.Clean(p))
		if err != nil {
			return err
		}
		sum := sha256.Sum256(raw)
		if hex.EncodeToString(sum[:]) != j.Previous.Digest {
			return errors.New("recovery binary changed before restoration")
		}
		info, err := os.Stat(i.target)
		if err != nil {
			return err
		}
		if i.external {
			err = i.swap(p)
		} else {
			err = durableFile(i.target, raw, info.Mode().Perm())
		}
		if err != nil {
			return err
		}
		j.Probation = probationRolledBack
		return i.write(j)
	})
}

// collect removes only unreferenced digest artifacts, never the live recovery
// target. Temporary files and unknown names are not treated as owned artifacts.
func (i *installation) collect(j *Journal) error {
	entries, err := os.ReadDir(i.dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		name := entry.Name()
		raw, err := hex.DecodeString(name)
		if err != nil || len(raw) != sha256.Size || name == j.Candidate.Digest || (j.Previous != nil && name == j.Previous.Digest) {
			continue
		}
		path := filepath.Join(i.dir, name)
		if err := regularOrMissing(path); err != nil {
			return err
		}
		if err := os.Remove(path); err != nil {
			return err
		}
	}
	return syncDir(i.dir)
}

// Quarantine records failed post-install health without changing the binary.
// It also applies at bootstrap, when there is no verified recovery target.
func Quarantine(ctx context.Context, path, reason string) error {
	if reason == "" {
		return errors.New("quarantine requires a reason")
	}
	i, err := openInstallation(ctx, path)
	if err != nil {
		return err
	}
	defer i.close()
	j, err := i.read()
	if err != nil {
		return err
	}
	if j == nil {
		return errors.New("no installation journal")
	}
	if err := i.recover(j); err != nil {
		return err
	}
	if j.Probation != probationPending {
		return errors.New("only a pending candidate can be quarantined")
	}
	if j.Quarantine == nil {
		j.Quarantine = map[string]string{}
	}
	j.Quarantine[j.Candidate.Digest] = reason
	return i.write(j)
}
