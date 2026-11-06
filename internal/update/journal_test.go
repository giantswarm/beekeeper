package update

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func recoveryFixture(t *testing.T) (*installation, string) {
	t.Helper()
	dir := t.TempDir()
	target := filepath.Join(dir, "beekeeper")
	if err := os.WriteFile(target, []byte("bootstrap"), 0o750); err != nil { //nolint:gosec // an executable fixture
		t.Fatal(err)
	}
	i, err := openInstallation(context.Background(), target)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(i.close)
	return i, dir
}
func stageRecovery(t *testing.T, dir, content string) string {
	t.Helper()
	f, err := os.CreateTemp(dir, "candidate-*")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(content); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(f.Name(), 0o750); err != nil { //nolint:gosec // an executable fixture
		t.Fatal(err)
	}
	return f.Name()
}
func readRecovery(t *testing.T, i *installation) *Journal {
	t.Helper()
	j, err := i.read()
	if err != nil {
		t.Fatal(err)
	}
	return j
}
func assertRecoveryBytes(t *testing.T, path, want string) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Clean(path))
	if err != nil || string(raw) != want {
		t.Fatalf("bytes %q, error %v; want %q", raw, err, want)
	}
}
func passedRecovery(t *testing.T, i *installation, dir string) {
	t.Helper()
	if err := i.replace(stageRecovery(t, dir, "good"), "v1.0.0"); err != nil {
		t.Fatal(err)
	}
	i.close()
	if err := CompleteProbation(context.Background(), i.target, "v1.0.0"); err != nil {
		t.Fatal(err)
	}
	ok, err := i.lock.TryLock()
	if err != nil || !ok {
		t.Fatalf("relock %v %v", ok, err)
	}
}
func TestJournalBootstrapPromotionAndRollback(t *testing.T) {
	i, dir := recoveryFixture(t)
	passedRecovery(t, i, dir)
	j := readRecovery(t, i)
	if j.Previous != nil || j.Displaced.Version == "" || j.Displaced.Digest == "" || j.InstalledAt.IsZero() || j.Probation != probationPassed {
		t.Fatalf("bootstrap %+v", j)
	}
	if err := i.replace(stageRecovery(t, dir, "bad"), "v2.0.0"); err != nil {
		t.Fatal(err)
	}
	j = readRecovery(t, i)
	if j.Previous == nil || j.Previous.Version != "v1.0.0" || j.Probation != probationPending {
		t.Fatalf("journal %+v", j)
	}
	if err := i.replace(stageRecovery(t, dir, "third"), "v3.0.0"); err == nil {
		t.Fatal("replaced during probation")
	}
	i.close()
	guard := func(version string, restore func() error) error {
		if version != "v1.0.0" {
			t.Fatal(version)
		}
		return restore()
	}
	for range 2 {
		if err := Rollback(context.Background(), i.target, "health check failed", guard); err != nil {
			t.Fatal(err)
		}
	}
	assertRecoveryBytes(t, i.target, "good")
	j = readRecovery(t, i)
	if j.Probation != probationRolledBack || len(j.Quarantine) != 1 {
		t.Fatalf("journal %+v", j)
	}
}
func TestRecoveryRejectsModifiedBackupAndStateGuardFailure(t *testing.T) {
	for _, tamper := range []bool{false, true} {
		t.Run(map[bool]string{false: "state incompatible", true: "modified backup"}[tamper], func(t *testing.T) {
			i, dir := recoveryFixture(t)
			passedRecovery(t, i, dir)
			if err := i.replace(stageRecovery(t, dir, "candidate"), "v2.0.0"); err != nil {
				t.Fatal(err)
			}
			j := readRecovery(t, i)
			if tamper {
				p, _ := i.artifact(*j.Previous)
				if err := os.WriteFile(p, []byte("tampered"), 0o700); err != nil { //nolint:gosec // an executable fixture
					t.Fatal(err)
				}
			}
			i.close()
			err := Rollback(context.Background(), i.target, "failed", func(string, func() error) error { return errors.New("state incompatible") })
			if err == nil {
				t.Fatal("unsafe recovery accepted")
			}
			assertRecoveryBytes(t, i.target, "candidate")
		})
	}
}
func TestJournalCrashBoundaries(t *testing.T) {
	// Fail each durable rename: retain, prepare, replace, and record probation.
	for boundary := 1; boundary <= 4; boundary++ {
		t.Run(strconv.Itoa(boundary), func(t *testing.T) {
			i, dir := recoveryFixture(t)
			staged := stageRecovery(t, dir, "candidate")
			original := journalRename
			calls := 0
			journalRename = func(from, to string) error {
				calls++
				if calls == boundary {
					return errors.New("interrupted")
				}
				return original(from, to)
			}
			t.Cleanup(func() { journalRename = original })
			if err := i.replace(staged, "v1.0.0"); err == nil {
				t.Fatal("missing interruption")
			}
			journalRename = original
			j := readRecovery(t, i)
			for range 2 {
				if err := i.recover(j); err != nil {
					t.Fatal(err)
				}
			}
			if boundary == 4 {
				assertRecoveryBytes(t, i.target, "candidate")
				if j.Probation != probationPending {
					t.Fatal(j.Probation)
				}
			} else {
				assertRecoveryBytes(t, i.target, "bootstrap")
				if err := i.replace(staged, "v1.0.0"); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}
func TestRollbackCrashBoundaries(t *testing.T) {
	for boundary := 1; boundary <= 4; boundary++ {
		t.Run(strconv.Itoa(boundary), func(t *testing.T) {
			i, dir := recoveryFixture(t)
			passedRecovery(t, i, dir)
			if err := i.replace(stageRecovery(t, dir, "bad"), "v2.0.0"); err != nil {
				t.Fatal(err)
			}
			i.close()
			original := journalRename
			calls := 0
			journalRename = func(from, to string) error {
				calls++
				if calls == boundary {
					return errors.New("crash")
				}
				return original(from, to)
			}
			t.Cleanup(func() { journalRename = original })
			guard := func(_ string, restore func() error) error { return restore() }
			if err := Rollback(context.Background(), i.target, "failed", guard); err == nil {
				t.Fatal("missing crash")
			}
			journalRename = original
			for range 2 {
				if err := Rollback(context.Background(), i.target, "failed", guard); err != nil {
					t.Fatal(err)
				}
			}
			assertRecoveryBytes(t, i.target, "good")
		})
	}
}
func TestJournalDiskFullAndConcurrentExclusion(t *testing.T) {
	i, dir := recoveryFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	if other, err := openInstallation(ctx, i.target); err == nil {
		other.close()
		t.Fatal("concurrent updater acquired lock")
	}
	original := journalWrite
	t.Cleanup(func() { journalWrite = original })
	journalWrite = func(*os.File, []byte) error { return syscall.ENOSPC }
	if err := i.replace(stageRecovery(t, dir, "new"), "v1.0.0"); !errors.Is(err, syscall.ENOSPC) {
		t.Fatal(err)
	}
	assertRecoveryBytes(t, i.target, "bootstrap")
}
func TestJournalRejectsSymlinkRecoveryFiles(t *testing.T) {
	for _, name := range []string{"journal.json", "lock", "backup", "directory"} {
		t.Run(name, func(t *testing.T) {
			i, dir := recoveryFixture(t)
			switch name {
			case "directory":
				i.close()
				if err := os.RemoveAll(i.dir); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(dir, i.dir); err != nil {
					t.Fatal(err)
				}
				if other, err := openInstallation(context.Background(), i.target); err == nil {
					other.close()
					t.Fatal("accepted directory symlink")
				}
			case "lock":
				i.close()
				if err := os.Remove(filepath.Join(i.dir, "lock")); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(i.target, filepath.Join(i.dir, "lock")); err != nil {
					t.Fatal(err)
				}
				if other, err := openInstallation(context.Background(), i.target); err == nil {
					other.close()
					t.Fatal("accepted lock symlink")
				}
			case "journal.json":
				if err := os.Symlink(i.target, filepath.Join(i.dir, name)); err != nil {
					t.Fatal(err)
				}
				if _, err := i.read(); err == nil {
					t.Fatal("accepted journal symlink")
				}
			case "backup":
				b := Binary{Digest: strings.Repeat("a", 64)}
				p, _ := i.artifact(b)
				if err := os.Symlink(i.target, p); err != nil {
					t.Fatal(err)
				}
				if _, err := i.verify(b); err == nil {
					t.Fatal("accepted artifact symlink")
				}
			}
			assertRecoveryBytes(t, i.target, "bootstrap")
		})
	}
}
func TestBootstrapHasNoRollbackAndFutureJournalIsFenced(t *testing.T) {
	i, dir := recoveryFixture(t)
	if err := i.replace(stageRecovery(t, dir, "new"), "v1.0.0"); err != nil {
		t.Fatal(err)
	}
	i.close()
	if err := Rollback(context.Background(), i.target, "failed", func(_ string, f func() error) error { return f() }); err == nil {
		t.Fatal("unverified bootstrap recovery")
	}
	j := readRecovery(t, i)
	j.Schema = 2
	if err := i.write(j); err != nil {
		t.Fatal(err)
	}
	if err := CompleteProbation(context.Background(), i.target, "v1.0.0"); err == nil {
		t.Fatal("future journal accepted")
	}
	assertRecoveryBytes(t, i.target, "new")
}

func TestJournalGarbageCollectionPreservesActiveRecoveryTarget(t *testing.T) {
	i, dir := recoveryFixture(t)
	passedRecovery(t, i, dir)
	if err := i.replace(stageRecovery(t, dir, "new"), "v2.0.0"); err != nil {
		t.Fatal(err)
	}
	orphan := Binary{Digest: strings.Repeat("b", 64)}
	p, _ := i.artifact(orphan)
	if err := os.WriteFile(p, []byte("unused"), 0o600); err != nil {
		t.Fatal(err)
	}
	i.close()
	if err := CompleteProbation(context.Background(), i.target, "v2.0.0"); err != nil {
		t.Fatal(err)
	}
	j := readRecovery(t, i)
	if _, err := i.verify(*j.Previous); err != nil {
		t.Fatal(err)
	}
	if _, err := i.verify(j.Candidate); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("orphan not collected: %v", err)
	}
}
func TestDiskFullAtEveryJournalWrite(t *testing.T) {
	for boundary := 1; boundary <= 3; boundary++ {
		t.Run(strconv.Itoa(boundary), func(t *testing.T) {
			i, dir := recoveryFixture(t)
			staged := stageRecovery(t, dir, "new")
			original := journalWrite
			calls := 0
			journalWrite = func(f *os.File, raw []byte) error {
				calls++
				if calls == boundary {
					return syscall.ENOSPC
				}
				return original(f, raw)
			}
			t.Cleanup(func() { journalWrite = original })
			if err := i.replace(staged, "v1.0.0"); !errors.Is(err, syscall.ENOSPC) {
				t.Fatal(err)
			}
			journalWrite = original
			j := readRecovery(t, i)
			if err := i.recover(j); err != nil {
				t.Fatal(err)
			}
			if boundary == 3 {
				assertRecoveryBytes(t, i.target, "new")
				if j.Probation != probationPending {
					t.Fatal(j.Probation)
				}
			} else {
				assertRecoveryBytes(t, i.target, "bootstrap")
			}
		})
	}
}
func TestQuarantineIsDurableAndCannotBePromoted(t *testing.T) {
	i, dir := recoveryFixture(t)
	if err := i.replace(stageRecovery(t, dir, "failed"), "v1.0.0"); err != nil {
		t.Fatal(err)
	}
	i.close()
	if err := Quarantine(context.Background(), i.target, "failed health"); err != nil {
		t.Fatal(err)
	}
	if err := CompleteProbation(context.Background(), i.target, "v1.0.0"); err == nil {
		t.Fatal("promoted quarantined candidate")
	}
	j := readRecovery(t, i)
	if j.Quarantine[j.Candidate.Digest] != "failed health" {
		t.Fatal(j.Quarantine)
	}
	assertRecoveryBytes(t, i.target, "failed")
}

func TestJournalRecoveryRejectsUnexpectedInstalledBytes(t *testing.T) {
	i, dir := recoveryFixture(t)
	if err := i.replace(stageRecovery(t, dir, "new"), "v1.0.0"); err != nil {
		t.Fatal(err)
	}
	j := readRecovery(t, i)
	j.Probation = probationPrepared
	if err := i.write(j); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(i.target, []byte("unexpected"), 0o750); err != nil { //nolint:gosec // an executable fixture
		t.Fatal(err)
	}
	if err := i.recover(j); err == nil {
		t.Fatal("recovered unexpected installed bytes")
	}
	assertRecoveryBytes(t, i.target, "unexpected")
}
func TestProbationPromotionInterruptionIsIdempotent(t *testing.T) {
	i, dir := recoveryFixture(t)
	if err := i.replace(stageRecovery(t, dir, "new"), "v1.0.0"); err != nil {
		t.Fatal(err)
	}
	i.close()
	original := journalRename
	journalRename = func(string, string) error { return syscall.ENOSPC }
	t.Cleanup(func() { journalRename = original })
	if err := CompleteProbation(context.Background(), i.target, "v1.0.0"); !errors.Is(err, syscall.ENOSPC) {
		t.Fatal(err)
	}
	if j := readRecovery(t, i); j.Probation != probationPending {
		t.Fatal(j.Probation)
	}
	journalRename = original
	for range 2 {
		if err := CompleteProbation(context.Background(), i.target, "v1.0.0"); err != nil {
			t.Fatal(err)
		}
	}
	assertRecoveryBytes(t, i.target, "new")
}
