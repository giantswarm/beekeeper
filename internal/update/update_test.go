package update

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/giantswarm/go-selfupdate"

	"github.com/giantswarm/beekeeper/pkg/project"
)

// fakeSource stands in for GitHub: the releases it lists, how it fails and
// what each asset's download returns.
type fakeSource struct {
	releases []selfupdate.SourceRelease
	err      error
	assets   map[int64][]byte
	calls    atomic.Int32
}

func (s *fakeSource) ListReleases(context.Context, selfupdate.Repository) ([]selfupdate.SourceRelease, error) {
	s.calls.Add(1)
	return s.releases, s.err
}

func (s *fakeSource) DownloadReleaseAsset(_ context.Context, _ *selfupdate.Release, id int64) (io.ReadCloser, error) {
	data, ok := s.assets[id]
	if !ok {
		return nil, fmt.Errorf("no asset %d", id)
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

type fakeAsset struct {
	id   int64
	name string
}

func (a fakeAsset) GetID() int64                  { return a.id }
func (a fakeAsset) GetName() string               { return a.name }
func (a fakeAsset) GetSize() int                  { return 3 }
func (a fakeAsset) GetBrowserDownloadURL() string { return "https://example.test/" + a.name }

type fakeRelease struct {
	tag    string
	pre    bool
	assets []selfupdate.SourceAsset
}

func (r fakeRelease) GetID() int64                        { return 1 }
func (r fakeRelease) GetTagName() string                  { return r.tag }
func (r fakeRelease) GetDraft() bool                      { return false }
func (r fakeRelease) GetPrerelease() bool                 { return r.pre }
func (r fakeRelease) GetPublishedAt() time.Time           { return time.Date(2026, 9, 24, 23, 0, 0, 0, time.UTC) }
func (r fakeRelease) GetReleaseNotes() string             { return "notes" }
func (r fakeRelease) GetName() string                     { return r.tag }
func (r fakeRelease) GetURL() string                      { return "https://" + Repository + "/releases/tag/" + r.tag }
func (r fakeRelease) GetAssets() []selfupdate.SourceAsset { return r.assets }

// The running binary and the release the fake GitHub carries.
const (
	running = "v0.2.0"
	latestV = "v0.3.0"
)

// newBinary is the binary a release carries in the tests.
const newBinary = "new"

// The asset IDs of a release: the bundle and the binary.
const (
	bundleID int64 = 1
	binaryID int64 = 2
)

// binaryName is the asset architect publishes for this platform.
func binaryName() string {
	name := "beekeeper-" + runtime.GOOS + "-" + runtime.GOARCH
	if runtime.GOOS == windowsOS {
		name += ".exe"
	}
	return name
}

// release is a release the way architect publishes beekeeper's: a binary per
// platform next to its cosign bundle, the bundle listed first to prove the
// binary is the one picked, and a tag with a pre-release part marked a
// pre-release, as GitHub does.
func release(tag string) fakeRelease {
	return fakeRelease{tag: tag, pre: strings.Contains(tag, "-"), assets: []selfupdate.SourceAsset{
		fakeAsset{bundleID, binaryName() + ".bundle"},
		fakeAsset{binaryID, binaryName()},
	}}
}

// acceptingValidator asks for the same bundle as the cosign validator and
// accepts whatever it is handed, recording it. The signature check itself is
// tested in github.com/giantswarm/selfupdate-cosign; these tests prove that
// self-update wires it in so that nothing unverified reaches the disk.
type acceptingValidator struct {
	asset          string
	binary, bundle []byte
}

func (v *acceptingValidator) GetValidationAssetName(asset string) string { return asset + ".bundle" }
func (v *acceptingValidator) Validate(asset string, binary, bundle []byte) error {
	v.asset, v.binary, v.bundle = asset, binary, bundle
	return nil
}

// setup makes the running version current and GitHub src; the validator
// stays the real one unless a test replaces it.
func setup(t *testing.T, current string, src selfupdate.Source) {
	t.Helper()
	previousEvidence := checkEvidence
	checkEvidence = func(context.Context, selfupdate.Source, *selfupdate.Release, string) error { return nil }
	t.Cleanup(func() { checkEvidence = previousEvidence })
	prevSource, prevTag, prevValidator, prevExe, prevVersion := newSource, newTagSource, newValidator, executable, currentVersion
	newSource = func(bool) (selfupdate.Source, error) { return src, nil }
	newTagSource = func(string) (selfupdate.Source, error) { return src, nil }
	currentVersion = func() string { return current }
	t.Cleanup(func() {
		newSource, newTagSource, newValidator, executable, currentVersion = prevSource, prevTag, prevValidator, prevExe, prevVersion
	})
}

// installed points self-update at a throwaway executable, reached through a
// symbolic link, and returns the file's path and content.
func installed(t *testing.T) (string, []byte) {
	t.Helper()
	content := []byte("the beekeeper installed right now")
	exe := filepath.Join(t.TempDir(), "beekeeper")
	if err := os.WriteFile(exe, content, 0o750); err != nil { //nolint:gosec // an executable
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "beekeeper")
	if err := os.Symlink(exe, link); err != nil {
		t.Fatal(err)
	}
	executable = func() (string, error) { return link, nil }
	return exe, content
}

func assertUnchanged(t *testing.T, exe string, content []byte) {
	t.Helper()
	got, err := os.ReadFile(filepath.Clean(exe))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("the installed binary was replaced: %q", got)
	}
}

func TestCheckReportsANewerRelease(t *testing.T) {
	setup(t, running, &fakeSource{releases: []selfupdate.SourceRelease{release(latestV), release(running)}})
	var out bytes.Buffer
	res, err := Run(context.Background(), &out, true)
	if !errors.Is(err, ErrOutdated) {
		t.Fatalf("Run(check) = %v, want ErrOutdated", err)
	}
	if res.Current != running || res.Latest != latestV || !res.Newer || res.Updated {
		t.Errorf("result %+v", res)
	}
	for _, want := range []string{running, latestV, "releases/tag/" + latestV} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("report lacks %q:\n%s", want, out.String())
		}
	}
}

func TestCheckPassesOnTheLatestRelease(t *testing.T) {
	// A pseudo-version after the latest tag (a build of main) is not behind.
	for _, current := range []string{latestV, latestV + "+dirty", "v0.3.1-0.20260924230000-edd882d0a1b2"} {
		setup(t, current, &fakeSource{releases: []selfupdate.SourceRelease{release(latestV)}})
		var out bytes.Buffer
		res, err := Run(context.Background(), &out, true)
		if err != nil || res.Newer {
			t.Errorf("%s: Run(check) = %+v, %v", current, res, err)
		}
		if !strings.Contains(out.String(), "is the latest release") {
			t.Errorf("%s: report:\n%s", current, out.String())
		}
	}
}

// The newest release candidate the fake GitHub carries.
const newestRC = "v0.4.0-rc.12"

// The release candidates GitHub lists, newest first, after the last release.
func candidates() []selfupdate.SourceRelease {
	return []selfupdate.SourceRelease{
		release(newestRC), release("v0.4.0-rc.11"), release("v0.4.0-rc.10"), release("v0.4.0-rc.9"), release(latestV),
	}
}

func TestCheckReportsANewerPrerelease(t *testing.T) {
	for _, current := range []string{"v0.4.0-rc.9", "v0.4.0-rc.9.0.20260924230000-edd882d0a1b2"} {
		setup(t, current, &fakeSource{releases: candidates()})
		var out bytes.Buffer
		res, err := Run(context.Background(), &out, true)
		if !errors.Is(err, ErrOutdated) {
			t.Errorf("%s: Run(check) = %v, want ErrOutdated\n%s", current, err, out.String())
		}
		if res.Latest != newestRC || !res.Newer || res.Updated {
			t.Errorf("%s: result %+v", current, res)
		}
	}
}

func TestCheckPassesOnTheLatestPrerelease(t *testing.T) {
	setup(t, newestRC, &fakeSource{releases: candidates()})
	var out bytes.Buffer
	res, err := Run(context.Background(), &out, true)
	if err != nil || res.Newer || res.Latest != newestRC {
		t.Errorf("Run(check) = %+v, %v", res, err)
	}
}

// A release, or a build of main after one, is offered only releases.
func TestCheckSkipsPrereleasesOnARelease(t *testing.T) {
	for _, current := range []string{latestV, "v0.3.1-0.20260924230000-edd882d0a1b2"} {
		setup(t, current, &fakeSource{releases: candidates()})
		res, err := Run(context.Background(), io.Discard, true)
		if err != nil || res.Newer || res.Latest != latestV {
			t.Errorf("%s: Run(check) = %+v, %v", current, res, err)
		}
	}
}

// A release after the candidates is newer than each of them.
func TestCheckReportsAReleaseAfterAPrerelease(t *testing.T) {
	setup(t, newestRC, &fakeSource{releases: append([]selfupdate.SourceRelease{release("v0.4.0")}, candidates()...)})
	res, err := Run(context.Background(), io.Discard, true)
	if !errors.Is(err, ErrOutdated) || res.Latest != "v0.4.0" {
		t.Errorf("Run(check) = %+v, %v", res, err)
	}
}

func TestRefusesADevelopmentBuild(t *testing.T) {
	for _, current := range []string{"dev", "e6c760a32b485c574f91cf9b89060dfa168923c9", ""} {
		src := &fakeSource{releases: []selfupdate.SourceRelease{release(latestV)}}
		setup(t, current, src)
		_, err := Run(context.Background(), io.Discard, false)
		if err == nil || !strings.Contains(err.Error(), "development build") || !strings.Contains(err.Error(), project.Source+"/releases") {
			t.Errorf("%q: Run() = %v, want a refusal that says how to install a release", current, err)
		}
		if n := src.calls.Load(); n != 0 {
			t.Errorf("%q: asked GitHub %d times", current, n)
		}
	}
}

func TestReportsAnUnreachableGitHub(t *testing.T) {
	setup(t, running, &fakeSource{err: errors.New("dial tcp: no route to host")})
	_, err := Run(context.Background(), io.Discard, false)
	if err == nil || !strings.Contains(err.Error(), "no route to host") || !strings.Contains(err.Error(), "online") {
		t.Errorf("Run() = %v, want the network error and the hint", err)
	}
}

func TestReplacesTheExecutableOnceTheBundleVerifies(t *testing.T) {
	setup(t, running, &fakeSource{
		releases: []selfupdate.SourceRelease{release(latestV)},
		assets:   map[int64][]byte{binaryID: []byte(newBinary), bundleID: []byte("its bundle")},
	})
	exe, _ := installed(t)
	link, _ := executable()
	v := &acceptingValidator{}
	newValidator = func() selfupdate.Validator { return v }

	var out bytes.Buffer
	res, err := Run(context.Background(), &out, false)
	if err != nil {
		t.Fatalf("Run() = %v\n%s", err, out.String())
	}
	if !res.Updated || res.Path != link {
		t.Errorf("result %+v", res)
	}
	if got, _ := os.ReadFile(filepath.Clean(exe)); string(got) != newBinary {
		t.Errorf("executable holds %q after the update", got)
	}
	if !strings.Contains(out.String(), "Verified the signature and updated") {
		t.Errorf("output:\n%s", out.String())
	}
	if v.asset != binaryName() || string(v.binary) != newBinary || string(v.bundle) != "its bundle" {
		t.Errorf("validated %q (%q) against bundle %q", v.asset, v.binary, v.bundle)
	}
	if runtime.GOOS == windowsOS {
		return
	}
	// The link still names the file, which keeps its mode, and nothing is
	// left beside it except the durable recovery directory.
	if target, err := os.Readlink(link); err != nil || target != exe {
		t.Errorf("link names %q, %v", target, err)
	}
	if info, err := os.Stat(exe); err != nil || info.Mode().Perm() != 0o750 {
		t.Errorf("executable mode %v, %v; want -rwxr-x---", info.Mode(), err)
	}
	if entries, _ := os.ReadDir(filepath.Dir(exe)); len(entries) != 2 {
		t.Errorf("%s holds %d entries, want the binary and recovery directory", filepath.Dir(exe), len(entries))
	}
}

func TestInstallsANewerPrereleaseOnceTheBundleVerifies(t *testing.T) {
	src := &fakeSource{releases: candidates(), assets: map[int64][]byte{binaryID: []byte(newBinary), bundleID: []byte("its bundle")}}
	setup(t, "v0.4.0-rc.9", src)
	exe, _ := installed(t)
	v := &acceptingValidator{}
	newValidator = func() selfupdate.Validator { return v }

	var out bytes.Buffer
	res, err := Run(context.Background(), &out, false)
	if err != nil {
		t.Fatalf("Run() = %v\n%s", err, out.String())
	}
	if !res.Updated || res.Latest != newestRC {
		t.Errorf("result %+v", res)
	}
	if got, _ := os.ReadFile(filepath.Clean(exe)); string(got) != newBinary {
		t.Errorf("executable holds %q after the update", got)
	}
	if v.asset != binaryName() || string(v.bundle) != "its bundle" {
		t.Errorf("validated %q against bundle %q", v.asset, v.bundle)
	}
}

// A running beekeeper watch holds its executable open: writing over it fails
// (ETXTBSY) and swapping through two renames leaves a moment without a binary.
// The update renames the new file over it once, and the running process is
// untouched.
func TestReplacesARunningExecutable(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("text-busy executables are a Linux matter")
	}
	sleep, err := exec.LookPath("sleep")
	if err != nil {
		t.Skip("no sleep binary to run")
	}
	orig, err := os.ReadFile(filepath.Clean(sleep))
	if err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(t.TempDir(), "beekeeper")
	if err := os.WriteFile(exe, orig, 0o755); err != nil { //nolint:gosec // an executable
		t.Fatal(err)
	}
	watch := exec.Command(exe, "60") //nolint:gosec // the test's own copy
	if err := watch.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = watch.Process.Kill(); _ = watch.Wait() })

	setup(t, running, &fakeSource{
		releases: []selfupdate.SourceRelease{release(latestV)},
		assets:   map[int64][]byte{binaryID: []byte("#!/bin/sh\necho new\n"), bundleID: []byte("its bundle")},
	})
	executable = func() (string, error) { return exe, nil }
	newValidator = func() selfupdate.Validator { return &acceptingValidator{} }

	if _, err := Run(context.Background(), io.Discard, false); err != nil {
		t.Fatalf("Run() with the executable running = %v", err)
	}
	if got, _ := os.ReadFile(filepath.Clean(exe)); string(got) != "#!/bin/sh\necho new\n" {
		t.Errorf("executable holds %d bytes, not the release", len(got))
	}
	if err := watch.Process.Signal(syscall.Signal(0)); err != nil {
		t.Errorf("the running copy is gone: %v", err)
	}
}

func TestRefusesAReleaseWithoutASignatureBundle(t *testing.T) {
	src := &fakeSource{
		releases: []selfupdate.SourceRelease{fakeRelease{tag: latestV, assets: []selfupdate.SourceAsset{fakeAsset{binaryID, binaryName()}}}},
		assets:   map[int64][]byte{binaryID: []byte("unsigned")},
	}
	for _, check := range []bool{false, true} {
		setup(t, running, src)
		exe, content := installed(t)
		_, err := Run(context.Background(), io.Discard, check)
		if err == nil || errors.Is(err, ErrOutdated) || !strings.Contains(err.Error(), "no signature bundle") {
			t.Errorf("check=%v: Run() = %v, want the refusal", check, err)
		}
		assertUnchanged(t, exe, content)
	}
}

func TestRefusesADownloadThatDoesNotVerify(t *testing.T) {
	setup(t, running, &fakeSource{
		releases: []selfupdate.SourceRelease{release(latestV)},
		assets:   map[int64][]byte{binaryID: []byte("tampered"), bundleID: []byte("{}")},
	})
	exe, content := installed(t)
	var out bytes.Buffer
	_, err := Run(context.Background(), &out, false)
	if err == nil || !strings.Contains(err.Error(), "failed") || !strings.Contains(err.Error(), "is not a Sigstore bundle") {
		t.Errorf("Run() = %v, want the refusal and why", err)
	}
	if strings.Contains(out.String(), "Verified") {
		t.Errorf("a refused download reported success:\n%s", out.String())
	}
	assertUnchanged(t, exe, content)
}

func TestReportsSourceInitializationFailure(t *testing.T) {
	setup(t, running, &fakeSource{})
	failure := errors.New("source initialization failed")
	newSource = func(bool) (selfupdate.Source, error) { return nil, failure }
	_, err := Run(context.Background(), io.Discard, true)
	if !errors.Is(err, failure) {
		t.Fatalf("Run() error=%v, want initialization failure", err)
	}
}

// installBundle is the bundle InstallVersion's release carries.
const installBundle = "the release's bundle"

func TestInstallVersionBootstrapsJournalWithoutUnverifiedBackup(t *testing.T) {
	src := &fakeSource{
		releases: []selfupdate.SourceRelease{release("v0.4.0"), release(latestV)},
		assets:   map[int64][]byte{binaryID: []byte(newBinary), bundleID: []byte(installBundle)},
	}
	setup(t, running, src)
	exe, _ := installed(t)
	v := &acceptingValidator{}
	newValidator = func() selfupdate.Validator { return v }

	var out bytes.Buffer
	res, err := InstallVersion(context.Background(), &out, latestV)
	if err != nil {
		t.Fatalf("InstallVersion() = %v\n%s", err, out.String())
	}
	if !res.Updated || res.Latest != latestV {
		t.Errorf("result %+v", res)
	}
	if got, _ := os.ReadFile(filepath.Clean(exe)); string(got) != newBinary {
		t.Errorf("executable holds %q after the update", got)
	}
	if string(v.bundle) != installBundle {
		t.Errorf("validated against bundle %q", v.bundle)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(exe), "."+filepath.Base(exe)+".previous")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("backup left after update: %v", err)
	}
}

func TestInstallVersionLeavesTheRunningRelease(t *testing.T) {
	src := &fakeSource{}
	setup(t, latestV, src)
	exe, content := installed(t)
	res, err := InstallVersion(context.Background(), io.Discard, latestV)
	if err != nil || res.Updated {
		t.Fatalf("InstallVersion() = %+v, %v", res, err)
	}
	if src.calls.Load() != 0 {
		t.Errorf("GitHub asked %d times for the running release", src.calls.Load())
	}
	assertUnchanged(t, exe, content)
}

func TestInstallVersionRefusesAMissingOrUnsignedRelease(t *testing.T) {
	for name, src := range map[string]*fakeSource{
		"missing":  {releases: []selfupdate.SourceRelease{release("v0.4.0")}},
		"unsigned": {releases: []selfupdate.SourceRelease{fakeRelease{tag: latestV, assets: []selfupdate.SourceAsset{fakeAsset{binaryID, binaryName()}}}}, assets: map[int64][]byte{binaryID: []byte(newBinary)}},
	} {
		t.Run(name, func(t *testing.T) {
			setup(t, running, src)
			exe, content := installed(t)
			if _, err := InstallVersion(context.Background(), io.Discard, latestV); err == nil {
				t.Fatal("InstallVersion() installed a release it must refuse")
			}
			assertUnchanged(t, exe, content)
		})
	}
}

func TestInstallVersionRefusesNoReleaseVersion(t *testing.T) {
	setup(t, running, &fakeSource{})
	for _, v := range []string{"", "0.3.0", "v0.3", "latest", "v0.3.0; rm"} {
		if _, err := InstallVersion(context.Background(), io.Discard, v); err == nil {
			t.Errorf("InstallVersion(%q) accepted", v)
		}
	}
}

func TestInstallVersionDoesNotTouchBackupPath(t *testing.T) {
	src := &fakeSource{releases: []selfupdate.SourceRelease{release(latestV)}, assets: map[int64][]byte{binaryID: []byte(newBinary), bundleID: []byte(installBundle)}}
	setup(t, running, src)
	exe, _ := installed(t)
	newValidator = func() selfupdate.Validator { return &acceptingValidator{} }
	// A nonempty directory makes backup removal fail on every platform,
	// even when the test runs with elevated permissions.
	backup := filepath.Join(filepath.Dir(exe), "."+filepath.Base(exe)+".previous")
	if err := os.Mkdir(backup, 0o700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(backup, "untouched")
	if err := os.WriteFile(marker, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := InstallVersion(context.Background(), io.Discard, latestV)
	if err != nil || !res.Updated {
		t.Fatalf("InstallVersion() = %+v, %v", res, err)
	}
	if got, err := os.ReadFile(filepath.Clean(exe)); err != nil || string(got) != newBinary {
		t.Fatalf("installed %q, %v", got, err)
	}
	if got, err := os.ReadFile(filepath.Clean(marker)); err != nil || string(got) != "keep" {
		t.Fatalf("backup path touched: %q, %v", got, err)
	}
}

// rootOwned makes the installation's directory unwritable to this account, as
// a system installation's is, with its recovery in this test's state directory.
func rootOwned(t *testing.T, exe string) {
	t.Helper()
	if runtime.GOOS == windowsOS || os.Geteuid() == 0 {
		t.Skip("needs directory permissions that bind this account")
	}
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	dir := filepath.Dir(exe)
	if err := os.Chmod(dir, 0o500); err != nil { //nolint:gosec // a directory of this test
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) }) //nolint:gosec // TempDir cleanup needs it writable
}

// fakeSudo stands in for privilegedInstall on a rootOwned installation: it opens
// the directory only for the copy, and counts its calls.
func fakeSudo(t *testing.T, denied error) *int {
	t.Helper()
	original := privilegedInstall
	t.Cleanup(func() { privilegedInstall = original })
	calls := 0
	privilegedInstall = func(_ context.Context, source, target, mode string) error {
		calls++
		if mode != "750" {
			t.Fatalf("mode %q", mode)
		}
		if denied != nil {
			return denied
		}
		raw, err := os.ReadFile(filepath.Clean(source))
		if err != nil {
			return err
		}
		dir := filepath.Dir(target)
		if err := os.Chmod(dir, 0o700); err != nil { //nolint:gosec // a directory of this test
			return err
		}
		defer func() { _ = os.Chmod(dir, 0o500) }() //nolint:gosec // a directory of this test
		next := target + ".next"
		if err := os.WriteFile(next, raw, 0o750); err != nil { //nolint:gosec // an executable fixture
			return err
		}
		return os.Rename(next, target)
	}
	return &calls
}

func acceptRelease(t *testing.T, version, binary string, running string) {
	t.Helper()
	src := &fakeSource{releases: []selfupdate.SourceRelease{release(version)}, assets: map[int64][]byte{binaryID: []byte(binary), bundleID: []byte(installBundle)}}
	setup(t, running, src)
	newValidator = func() selfupdate.Validator { return &acceptingValidator{} }
}

func installJournal(t *testing.T, exe string) *Journal {
	t.Helper()
	i, err := openInstallation(context.Background(), exe)
	if err != nil {
		t.Fatal(err)
	}
	defer i.close()
	j, err := i.read()
	if err != nil || j == nil {
		t.Fatalf("journal %+v, %v", j, err)
	}
	return j
}

func TestInstallVersionUsesPrivilegeOnlyForVerifiedReplacement(t *testing.T) {
	for _, tc := range []struct {
		name               string
		verifyErr, sudoErr error
	}{
		{name: "success"},
		{name: "invalid signature", verifyErr: errors.New("invalid signature")},
		{name: "sudo unavailable", sudoErr: errors.New("sudo denied")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			acceptRelease(t, latestV, newBinary, running)
			exe, content := installed(t)
			rootOwned(t, exe)
			calls := fakeSudo(t, tc.sudoErr)
			original := installVerified
			t.Cleanup(func() { installVerified = original })
			installVerified = func(ctx context.Context, up *selfupdate.Updater, rel *selfupdate.Release, path string) error {
				if tc.verifyErr != nil {
					return tc.verifyErr
				}
				return original(ctx, up, rel, path)
			}
			res, err := InstallVersion(context.Background(), io.Discard, latestV)
			switch {
			case tc.verifyErr != nil:
				if !errors.Is(err, tc.verifyErr) || *calls != 0 {
					t.Fatalf("verification error %v, privileged calls %d", err, *calls)
				}
			case tc.sudoErr != nil:
				if !errors.Is(err, tc.sudoErr) || *calls != 1 {
					t.Fatalf("sudo error %v, calls %d", err, *calls)
				}
			default:
				if err != nil || !res.Updated || *calls != 1 {
					t.Fatalf("result %+v, %v, privileged calls %d", res, err, *calls)
				}
				if got := installJournal(t, exe); got.Probation != probationPending || got.Candidate.Version != latestV {
					t.Fatalf("journal %+v", got)
				}
				return
			}
			assertUnchanged(t, exe, content)
		})
	}
}

// A writable installation never takes privilege.
func TestInstallVersionWritableInstallationNeverEscalates(t *testing.T) {
	acceptRelease(t, latestV, newBinary, running)
	exe, _ := installed(t)
	calls := fakeSudo(t, nil)
	if _, err := InstallVersion(context.Background(), io.Discard, latestV); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Clean(exe)); err != nil || string(got) != newBinary || *calls != 0 {
		t.Fatalf("installed %q, %v, privileged calls %d", got, err, *calls)
	}
}

func TestPrivilegedInstallReplacesAtomicallyWithoutBackup(t *testing.T) {
	if runtime.GOOS == windowsOS {
		t.Skip("sudo installation is Unix only")
	}
	dir := t.TempDir()
	// Exercise the actual installer script without privileges. The fake sudo
	// accepts only noninteractive invocation and executes its exact argv.
	sudo := filepath.Join(dir, "sudo")
	if err := os.WriteFile(sudo, []byte("#!/bin/sh\n[ \"$1\" = -n ] && [ \"$2\" = -- ] || exit 1\nshift 2\nexec \"$@\"\n"), 0o700); err != nil { //nolint:gosec // executable fixture in a test-owned directory
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	target := filepath.Join(dir, "installed binary")
	source := filepath.Join(dir, "verified binary")
	if err := os.WriteFile(target, []byte("old"), 0o755); err != nil { //nolint:gosec // executable fixture whose mode the install preserves
		t.Fatal(err)
	}
	if err := os.WriteFile(source, []byte(newBinary), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := privilegedInstall(context.Background(), source, target, "750"); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Clean(target)); err != nil || string(got) != newBinary {
		t.Fatalf("installed %q, %v", got, err)
	}
	info, err := os.Stat(target)
	if err != nil || info.Mode().Perm() != 0o750 {
		t.Fatalf("mode %v, %v", info, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Fatalf("installation left temporary files: %v", entries)
	}
}

// The replacement keeps the executable's mode and, beside the update directory,
// leaves nothing: no temporary file and no backup.
func TestInstallVersionReplacesAtomicallyWithoutBackup(t *testing.T) {
	if runtime.GOOS == windowsOS {
		t.Skip("mode bits are Unix only")
	}
	acceptRelease(t, latestV, newBinary, running)
	exe, _ := installed(t)
	before, err := os.Stat(exe)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := InstallVersion(context.Background(), io.Discard, latestV); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Clean(exe)); err != nil || string(got) != newBinary {
		t.Fatalf("installed %q, %v", got, err)
	}
	after, err := os.Stat(exe)
	if err != nil || after.Mode().Perm() != before.Mode().Perm() {
		t.Fatalf("mode %v, want %v, %v", after, before.Mode().Perm(), err)
	}
	entries, err := os.ReadDir(filepath.Dir(exe))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != filepath.Base(exe) && e.Name() != filepath.Base(exe)+".update" {
			t.Fatalf("installation left %s behind", e.Name())
		}
	}
}

const thirdV, thirdBinary = "v0.9.0", "the third binary"

// The second update needs nobody: run by the installed candidate through
// InstallVersion alone, as an execution host runs it, it promotes the candidate
// and installs the next release. A root-owned installation does the same.
func TestSecondUpdateCompletesProbation(t *testing.T) {
	for _, owned := range []bool{false, true} {
		t.Run(map[bool]string{false: "writable", true: "root-owned"}[owned], func(t *testing.T) {
			acceptRelease(t, latestV, newBinary, running)
			exe, _ := installed(t)
			if owned {
				rootOwned(t, exe)
				fakeSudo(t, nil)
			}
			if _, err := InstallVersion(context.Background(), io.Discard, latestV); err != nil {
				t.Fatal(err)
			}
			first := installJournal(t, exe)
			acceptRelease(t, thirdV, thirdBinary, latestV)
			res, err := InstallVersion(context.Background(), io.Discard, thirdV)
			if err != nil || !res.Updated {
				t.Fatalf("second update = %+v, %v", res, err)
			}
			if got, err := os.ReadFile(filepath.Clean(exe)); err != nil || string(got) != thirdBinary {
				t.Fatalf("installed %q, %v", got, err)
			}
			j := installJournal(t, exe)
			if j.Probation != probationPending || j.Candidate.Version != thirdV || j.Previous == nil || *j.Previous != first.Candidate {
				t.Fatalf("journal %+v", j)
			}
		})
	}
}

// A process of another version proves nothing about the installed candidate.
func TestUpdateFromAnotherVersionLeavesProbationPending(t *testing.T) {
	acceptRelease(t, latestV, newBinary, running)
	exe, _ := installed(t)
	if _, err := InstallVersion(context.Background(), io.Discard, latestV); err != nil {
		t.Fatal(err)
	}
	acceptRelease(t, thirdV, thirdBinary, running)
	if _, err := InstallVersion(context.Background(), io.Discard, thirdV); err == nil {
		t.Fatal("promoted a candidate this process is not")
	}
	assertUnchanged(t, exe, []byte(newBinary))
	if j := installJournal(t, exe); j.Probation != probationPending {
		t.Fatalf("journal %+v", j)
	}
}

// Health repeats the local checks on the installed candidate. A failure never
// promotes it: the update moves past it and keeps the last known good.
func TestUpdateHealthGatesPromotion(t *testing.T) {
	for _, failed := range []bool{false, true} {
		t.Run(map[bool]string{false: "healthy", true: "unhealthy"}[failed], func(t *testing.T) {
			acceptRelease(t, latestV, newBinary, running)
			exe, _ := installed(t)
			resolved, err := filepath.EvalSymlinks(exe)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := InstallVersion(context.Background(), io.Discard, latestV); err != nil {
				t.Fatal(err)
			}
			first := installJournal(t, exe)
			checked := ""
			ctx := WithOptions(context.Background(), Options{Health: func(_ context.Context, path, version string) error {
				checked = path + " " + version
				if failed {
					return errors.New("no answer")
				}
				return nil
			}})
			acceptRelease(t, thirdV, thirdBinary, latestV)
			if _, err := InstallVersion(ctx, io.Discard, thirdV); err != nil {
				t.Fatal(err)
			}
			if checked != resolved+" "+latestV {
				t.Fatalf("checked %q", checked)
			}
			if got, err := os.ReadFile(filepath.Clean(exe)); err != nil || string(got) != thirdBinary {
				t.Fatalf("installed %q, %v", got, err)
			}
			j := installJournal(t, exe)
			if failed {
				if j.Previous != nil || !strings.Contains(j.Quarantine[first.Candidate.Digest], "no answer") {
					t.Fatalf("promoted a failed candidate: %+v", j)
				}
				return
			}
			if j.Previous == nil || *j.Previous != first.Candidate || len(j.Quarantine) != 0 {
				t.Fatalf("journal %+v", j)
			}
		})
	}
}

// A rollback of a root-owned installation restores through the same single
// privileged step, from recovery bytes this account holds.
func TestRootOwnedRollbackRestoresThroughPrivilege(t *testing.T) {
	acceptRelease(t, latestV, newBinary, running)
	exe, _ := installed(t)
	rootOwned(t, exe)
	calls := fakeSudo(t, nil)
	if _, err := InstallVersion(context.Background(), io.Discard, latestV); err != nil {
		t.Fatal(err)
	}
	acceptRelease(t, thirdV, thirdBinary, latestV)
	if _, err := InstallVersion(context.Background(), io.Discard, thirdV); err != nil {
		t.Fatal(err)
	}
	guard := func(_, _ string, restore func() error) error { return restore() }
	if err := Rollback(context.Background(), exe, "failed health", guard); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Clean(exe)); err != nil || string(got) != newBinary || *calls != 3 {
		t.Fatalf("restored %q, %v, privileged calls %d", got, err, *calls)
	}
}

// A journal keeps its place when the directory's permissions change, or its
// recovery target would be lost.
func TestRecoveryDirKeepsAnExistingJournal(t *testing.T) {
	acceptRelease(t, latestV, newBinary, running)
	exe, _ := installed(t)
	rootOwned(t, exe)
	fakeSudo(t, nil)
	if _, err := InstallVersion(context.Background(), io.Discard, latestV); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Dir(exe), 0o700); err != nil { //nolint:gosec // a directory of this test
		t.Fatal(err)
	}
	if j := installJournal(t, exe); j.Candidate.Version != latestV {
		t.Fatalf("journal %+v", j)
	}
}

func TestCanaryRefusesBadBinaryAndSkipsRelease(t *testing.T) {
	setup(t, running, &fakeSource{
		releases: []selfupdate.SourceRelease{release(latestV)},
		assets:   map[int64][]byte{binaryID: []byte(newBinary), bundleID: []byte("its bundle")},
	})
	exe, content := installed(t)
	newValidator = func() selfupdate.Validator { return &acceptingValidator{} }

	skipped := map[string]bool{}
	var canariedPath, canariedVersion, canariedBytes string
	ctx := WithOptions(context.Background(), Options{
		Canary: func(_ context.Context, path, version string) error {
			canariedPath, canariedVersion = path, version
			b, err := os.ReadFile(filepath.Clean(path))
			if err != nil {
				return err
			}
			canariedBytes = string(b)
			// The running executable is still the old one while the canary runs.
			assertUnchanged(t, exe, content)
			skipped[version] = true
			return errors.New("canary budget --gate failed")
		},
		Skip: func(tag string) bool { return skipped[tag] },
	})

	res, err := Run(ctx, io.Discard, false)
	if err == nil || !strings.Contains(err.Error(), "canary budget --gate failed") || res.Updated {
		t.Fatalf("Run() = %+v, %v; want canary failure without updating", res, err)
	}
	if canariedPath == "" || canariedVersion != latestV || canariedBytes != newBinary {
		t.Fatalf("canary saw path=%q version=%q bytes=%q", canariedPath, canariedVersion, canariedBytes)
	}
	assertUnchanged(t, exe, content)

	// Once skipped, --check and a second update both skip that release.
	var out bytes.Buffer
	res, err = Run(ctx, &out, true)
	if err != nil || res.Newer {
		t.Fatalf("Run(check) after skip = %+v, %v", res, err)
	}
	if !strings.Contains(out.String(), "is the latest release") {
		t.Errorf("check output after skip:\n%s", out.String())
	}
}

func TestCanaryCannotModifyTheVerifiedCandidate(t *testing.T) {
	src := &fakeSource{releases: []selfupdate.SourceRelease{release(latestV)}, assets: map[int64][]byte{binaryID: []byte(newBinary), bundleID: []byte(installBundle)}}
	setup(t, running, src)
	exe, content := installed(t)
	newValidator = func() selfupdate.Validator { return &acceptingValidator{} }
	ctx := WithOptions(context.Background(), Options{Canary: func(_ context.Context, path, _ string) error {
		return os.WriteFile(path, []byte("modified after verification"), 0o700) //nolint:gosec // an executable fixture
	}})
	if _, err := InstallVersion(ctx, io.Discard, latestV); err == nil {
		t.Fatal("installed modified candidate")
	}
	assertUnchanged(t, exe, content)
}

func TestRunExactReleaseCandidate(t *testing.T) {
	const tag = "v0.3.0-rc.3"
	for _, check := range []bool{true, false} {
		t.Run(fmt.Sprint(check), func(t *testing.T) {
			src := &fakeSource{releases: []selfupdate.SourceRelease{release(tag)}, assets: map[int64][]byte{binaryID: []byte(newBinary), bundleID: []byte(installBundle)}}
			setup(t, running, src)
			exe, original := installed(t)
			validator := &acceptingValidator{}
			newValidator = func() selfupdate.Validator { return validator }
			ctx := WithOptions(context.Background(), Options{
				Version: tag,
				Skip: func(string) bool {
					t.Fatal("an explicit version must bypass earlier skip records")
					return true
				},
			})
			res, err := Run(ctx, io.Discard, check)
			if res.Latest != tag || !res.Newer {
				t.Fatalf("result = %+v", res)
			}
			if check {
				if !errors.Is(err, ErrOutdated) || res.Updated || validator.asset != "" {
					t.Fatalf("check = %+v, %v", res, err)
				}
				assertUnchanged(t, exe, original)
			} else {
				if err != nil || !res.Updated || string(validator.bundle) != installBundle {
					t.Fatalf("install = %+v, %v", res, err)
				}
				got, err := os.ReadFile(exe) //nolint:gosec // a path of this test
				if err != nil || string(got) != newBinary {
					t.Fatalf("installed = %q, %v", got, err)
				}
			}
		})
	}
}

func TestRunExactReleaseRefusalsKeepBinary(t *testing.T) {
	const tag = "v0.3.0-rc.3"
	unsigned := release(tag)
	unsigned.assets = []selfupdate.SourceAsset{fakeAsset{binaryID, binaryName()}}
	for name, src := range map[string]*fakeSource{
		"missing":  {},
		"unsigned": {releases: []selfupdate.SourceRelease{unsigned}},
		"tampered": {releases: []selfupdate.SourceRelease{release(tag)}, assets: map[int64][]byte{binaryID: []byte("tampered"), bundleID: []byte("{}")}},
	} {
		t.Run(name, func(t *testing.T) {
			setup(t, running, src)
			exe, original := installed(t)
			_, err := Run(WithOptions(context.Background(), Options{Version: tag}), io.Discard, false)
			if err == nil || strings.Contains(err.Error(), "\n") {
				t.Fatalf("expected one-line refusal, got %v", err)
			}
			assertUnchanged(t, exe, original)
		})
	}
}

// A host follows a coordinator rollback without promoting the failed candidate.
// The ordinary updater still waits for the recovery controller, and neither a
// state incompatibility nor changed recovery bytes can be bypassed by the host.
func TestHostFollowsCoordinatorRollbackDuringWatchedProbation(t *testing.T) {
	const rollbackScenario = "rollback"
	for _, scenario := range []string{rollbackScenario, "ordinary update", "incompatible state", "changed download", "changed recovery", "bad bundle", "other release"} {
		t.Run(scenario, func(t *testing.T) {
			acceptRelease(t, latestV, newBinary, running)
			exe, _ := installed(t)
			if _, err := InstallVersion(context.Background(), io.Discard, latestV); err != nil {
				t.Fatal(err)
			}
			if err := CompleteProbation(context.Background(), exe, latestV); err != nil {
				t.Fatal(err)
			}
			acceptRelease(t, thirdV, thirdBinary, latestV)
			ctx := WithOptions(context.Background(), Options{Recovery: func(context.Context, Binary, string, string) error { return nil }})
			if _, err := InstallVersion(ctx, io.Discard, thirdV); err != nil {
				t.Fatal(err)
			}
			before := installJournal(t, exe)
			if !before.Watched || !before.Pending() {
				t.Fatalf("not in watched probation: %+v", before)
			}
			want, download := latestV, newBinary
			if scenario == "changed download" {
				download = "different signed bytes for the same tag"
			}
			if scenario == "other release" {
				want = running
			}
			acceptRelease(t, want, download, thirdV)
			if scenario == "bad bundle" {
				newValidator = func() selfupdate.Validator { return &hostRollbackRejectingValidator{} }
			}
			if scenario == "changed recovery" {
				if err := os.WriteFile(filepath.Join(exe+".update", before.Previous.Digest), []byte("tampered"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			guarded := false
			opts := Options{HostRollback: func(previous, failed string, restore func() error) error {
				guarded = true
				if previous != latestV || failed != thirdV {
					t.Fatalf("guard versions %s, %s", previous, failed)
				}
				if scenario == "incompatible state" {
					return errors.New("incompatible live state")
				}
				return restore()
			}}
			if scenario == "ordinary update" {
				opts.HostRollback = nil
			}
			ctx = WithOptions(context.Background(), opts)
			res, err := InstallVersion(ctx, io.Discard, want)
			if scenario != rollbackScenario {
				if err == nil || res.Updated {
					t.Fatalf("unsafe replacement accepted: %+v, %v", res, err)
				}
				assertRecoveryBytes(t, exe, thirdBinary)
				if j := installJournal(t, exe); j.Probation == probationPassed || j.RolledBack() {
					t.Fatalf("candidate promoted or restored: %+v", j)
				}
				return
			}
			if err != nil || !res.Updated || !guarded {
				t.Fatalf("coordinator rollback: %+v, guarded %v, %v", res, guarded, err)
			}
			assertRecoveryBytes(t, exe, newBinary)
			j := installJournal(t, exe)
			if !j.RolledBack() || j.Previous.Version != latestV || j.Quarantine[before.Candidate.Digest] == "" {
				t.Fatalf("rollback lost recovery or quarantine: %+v", j)
			}
			// An interrupted caller still on the candidate may repeat the request.
			if _, err := InstallVersion(ctx, io.Discard, want); err != nil {
				t.Fatal(err)
			}
			assertRecoveryBytes(t, exe, newBinary)
		})
	}
}

// hostRollbackRejectingValidator proves a failed signature check precedes recovery.
type hostRollbackRejectingValidator struct{ acceptingValidator }

func (*hostRollbackRejectingValidator) Validate(string, []byte, []byte) error {
	return errors.New("invalid signature bundle")
}
