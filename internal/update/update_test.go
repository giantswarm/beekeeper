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

	"github.com/creativeprojects/go-selfupdate"
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
	assets []selfupdate.SourceAsset
}

func (r fakeRelease) GetID() int64                        { return 1 }
func (r fakeRelease) GetTagName() string                  { return r.tag }
func (r fakeRelease) GetDraft() bool                      { return false }
func (r fakeRelease) GetPrerelease() bool                 { return false }
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

// The asset IDs of a release: the bundle and the binary.
const (
	bundleID int64 = 1
	binaryID int64 = 2
)

// binaryName is the asset architect publishes for this platform.
func binaryName() string {
	name := "beekeeper-" + runtime.GOOS + "-" + runtime.GOARCH
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	return name
}

// release is a release the way architect publishes beekeeper's: a binary per
// platform next to its cosign bundle, the bundle listed first to prove the
// binary is the one picked.
func release(tag string) fakeRelease {
	return fakeRelease{tag: tag, assets: []selfupdate.SourceAsset{
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
func setup(t *testing.T, current string, src *fakeSource) {
	t.Helper()
	prevSource, prevValidator, prevExe, prevVersion := newSource, newValidator, executable, currentVersion
	newSource = func() selfupdate.Source { return src }
	currentVersion = func() string { return current }
	t.Cleanup(func() {
		newSource, newValidator, executable, currentVersion = prevSource, prevValidator, prevExe, prevVersion
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

func TestRefusesADevelopmentBuild(t *testing.T) {
	for _, current := range []string{"dev", "e6c760a32b485c574f91cf9b89060dfa168923c9", ""} {
		src := &fakeSource{releases: []selfupdate.SourceRelease{release(latestV)}}
		setup(t, current, src)
		_, err := Run(context.Background(), io.Discard, false)
		if err == nil || !strings.Contains(err.Error(), "development build") || !strings.Contains(err.Error(), "go install github.com/giantswarm/beekeeper@latest") {
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
		assets:   map[int64][]byte{binaryID: []byte("new"), bundleID: []byte("its bundle")},
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
	if got, _ := os.ReadFile(filepath.Clean(exe)); string(got) != "new" {
		t.Errorf("executable holds %q after the update", got)
	}
	if !strings.Contains(out.String(), "Verified the signature and updated") {
		t.Errorf("output:\n%s", out.String())
	}
	if v.asset != binaryName() || string(v.binary) != "new" || string(v.bundle) != "its bundle" {
		t.Errorf("validated %q (%q) against bundle %q", v.asset, v.binary, v.bundle)
	}
	if runtime.GOOS == "windows" {
		return
	}
	// The link still names the file, which keeps its mode, and nothing is
	// left beside it.
	if target, err := os.Readlink(link); err != nil || target != exe {
		t.Errorf("link names %q, %v", target, err)
	}
	if info, err := os.Stat(exe); err != nil || info.Mode().Perm() != 0o750 {
		t.Errorf("executable mode %v, %v; want -rwxr-x---", info.Mode(), err)
	}
	if entries, _ := os.ReadDir(filepath.Dir(exe)); len(entries) != 1 {
		t.Errorf("%s holds %d entries, want only the binary", filepath.Dir(exe), len(entries))
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
	if err == nil || !strings.Contains(err.Error(), "is unchanged") || !strings.Contains(err.Error(), "is not a Sigstore bundle") {
		t.Errorf("Run() = %v, want the refusal and why", err)
	}
	if strings.Contains(out.String(), "Verified") {
		t.Errorf("a refused download reported success:\n%s", out.String())
	}
	assertUnchanged(t, exe, content)
}
