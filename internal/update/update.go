// Package update replaces the running beekeeper with the latest signed
// release.
package update

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/Masterminds/semver/v3"
	"github.com/giantswarm/go-selfupdate"
	selfupdatecosign "github.com/giantswarm/selfupdate-cosign"
	"github.com/google/go-github/v93/github"
	"golang.org/x/mod/module"

	"github.com/giantswarm/beekeeper/pkg/project"
)

// Repository is the GitHub repository whose releases carry the binaries:
// beekeeper-<os>-<arch> (beekeeper-windows-<arch>.exe), each next to its
// cosign Sigstore bundle (<asset>.bundle), which the download is verified
// against before anything is written.
const Repository = "giantswarm/beekeeper"

// lookupTimeout bounds finding the latest release; the download itself runs
// until done or interrupted.
const lookupTimeout = 15 * time.Second

// ErrOutdated is what a check returns when a newer release exists.
var ErrOutdated = errors.New("a newer beekeeper release is available")

// Seams the tests replace: where releases come from (public GitHub,
// authenticated with the environment or gh login when available; empty tag responses are retried),
// what a download must verify against
// (the shared cosign validator: a CircleCI build of Repository, against the
// Sigstore public-good trust root), the file the update replaces and the
// running version.
var (
	newSource      = newGitHubSource
	newTagSource   = newGitHubTagSource
	newValidator   = func() selfupdate.Validator { return selfupdatecosign.New(Repository) }
	executable     = selfupdate.ExecutablePath
	currentVersion = project.Version
	currentCommit  = project.GitSHA
)

// Result is what a check or an update found and did.
type Result struct {
	Current   string    `json:"current"`
	Latest    string    `json:"latest"`
	Published time.Time `json:"published"`
	URL       string    `json:"url"`
	Newer     bool      `json:"newer"`
	// Path is the file the update replaced, symbolic links not resolved.
	Path    string `json:"path,omitempty"`
	Updated bool   `json:"updated"`
}

// Run looks up the latest release and, unless checkOnly, installs its binary
// for this OS and architecture over the running executable once its bundle
// verifies. A running pre-release looks among the pre-releases too, a
// running release only among releases. Progress goes to w. With checkOnly the error is ErrOutdated when
// a newer release exists.
//
// The binary is renamed over the executable in one step, symbolic links
// resolved: a beekeeper watch that runs keeps running the old binary, every
// process started afterwards runs the new one, and several updates may run
// at once. A development build, a release without a bundle for this
// platform's binary and a download that does not verify are refused, and the
// executable stays as it is.
func Run(ctx context.Context, w io.Writer, checkOnly bool) (Result, error) {
	res := Result{Current: currentVersion()}
	if _, err := semver.NewVersion(res.Current); err != nil {
		return res, fmt.Errorf("cannot self-update a development build (version %s): install a release from %s/releases or with go install %s@latest", res.Current, project.Source, project.Module)
	}

	source, err := newSource(prerelease(res.Current))
	if err != nil {
		return res, fmt.Errorf("creating release source: %w", err)
	}
	up, err := selfupdate.NewUpdater(selfupdate.Config{Source: source, Validator: newValidator(), Prerelease: prerelease(res.Current)})
	if err != nil {
		return res, err
	}
	lookup, cancel := context.WithTimeout(ctx, lookupTimeout)
	rel, found, err := up.DetectLatest(lookup, selfupdate.ParseSlug(Repository))
	cancel()
	switch {
	case errors.Is(err, selfupdate.ErrValidationAssetNotFound):
		// Nothing has been downloaded.
		return res, fmt.Errorf("the latest release of %s has no signature bundle for this platform's binary; refusing to install it: %w", Repository, err)
	case err != nil:
		var limited *github.RateLimitError
		if errors.As(err, &limited) {
			return res, fmt.Errorf("GitHub release lookup rate limit exceeded; resets at %s: %w", limited.Rate.Reset.UTC().Format(time.RFC3339), err)
		}
		return res, fmt.Errorf("looking up the latest release of %s (is this machine online?): %w", Repository, err)
	case !found:
		return res, fmt.Errorf("GitHub answered no release of %s that carries a binary for %s/%s", Repository, runtime.GOOS, runtime.GOARCH)
	}
	res.Latest, res.Published, res.URL = "v"+rel.Version(), rel.PublishedAt, rel.URL
	res.Newer = newer(res.Latest, res.Current)
	if !res.Newer {
		_, _ = fmt.Fprintf(w, "beekeeper %s is the latest release.\n", res.Current)
		return res, nil
	}
	_, _ = fmt.Fprintf(w, "beekeeper %s is out (published %s), this is %s:\n  %s\n", res.Latest, res.Published.Format(time.RFC3339), res.Current, res.URL)
	if checkOnly {
		return res, ErrOutdated
	}

	if res.Path, err = executable(); err != nil {
		return res, fmt.Errorf("locating the running executable: %w", err)
	}
	if err := selfupdatecosign.Install(ctx, up, rel, res.Path); err != nil {
		return res, fmt.Errorf("updating %s failed, it is unchanged: %w", res.Path, err)
	}
	res.Updated = true
	_, _ = fmt.Fprintf(w, "Verified the signature and updated %s to %s.\n", res.Path, res.Latest)
	return res, nil
}

// newer reports whether latest is a higher version than current, which is
// what pkg/project reports: a tag ("v0.3.0"), a tag with local edits
// ("v0.3.0+dirty"), or a Go pseudo-version between tags
// ("v0.3.1-0.20260924210000-edd882d0a1b2", after v0.3.0).
func newer(latest, current string) bool {
	l, err := semver.NewVersion(latest)
	if err != nil {
		return false
	}
	c, err := semver.NewVersion(current)
	if err != nil {
		return false
	}
	return l.GreaterThan(c)
}

// prerelease reports whether current is a pre-release ("v0.3.0-rc.2"), or a
// Go pseudo-version after one ("v0.3.0-rc.2.0.20260924210000-edd882d0a1b2");
// a pseudo-version after a release ("v0.3.1-0.20260924210000-edd882d0a1b2")
// is not.
func prerelease(current string) bool {
	if module.IsPseudoVersion(current) {
		base, err := module.PseudoVersionBase(current)
		if err != nil {
			return false
		}
		current = base
	}
	v, err := semver.NewVersion(current)
	return err == nil && v.Prerelease() != ""
}

// InstallVersion installs exactly the release version (a tag, "v0.3.0")
// over the running executable once its bundle verifies, as Run installs the
// latest: an execution host follows its coordinator's release, never a newer
// one. The replaced binary stays next to the executable (Previous) until
// Rollback puts it back or the next install replaces it. The running
// version is left as it is.
func InstallVersion(ctx context.Context, w io.Writer, version string) (Result, error) {
	res := Result{Current: currentVersion(), Latest: version}
	want, err := semver.StrictNewVersion(strings.TrimPrefix(version, "v"))
	if err != nil || !strings.HasPrefix(version, "v") {
		return res, fmt.Errorf("%q is no release version (v<major>.<minor>.<patch>)", version)
	}
	if _, err := semver.NewVersion(res.Current); err != nil {
		return res, fmt.Errorf("cannot update a development build (version %s) to a release: install %s by hand", res.Current, version)
	}
	if version == res.Current {
		return res, nil
	}
	source, err := newTagSource(version)
	if err != nil {
		return res, fmt.Errorf("creating release source: %w", err)
	}
	up, err := selfupdate.NewUpdater(selfupdate.Config{Source: source, Validator: newValidator(), Prerelease: want.Prerelease() != ""})
	if err != nil {
		return res, err
	}
	lookup, cancel := context.WithTimeout(ctx, lookupTimeout)
	rel, found, err := up.DetectVersion(lookup, selfupdate.ParseSlug(Repository), version)
	cancel()
	switch {
	case errors.Is(err, selfupdate.ErrValidationAssetNotFound):
		return res, fmt.Errorf("release %s of %s has no signature bundle for this platform's binary; refusing to install it: %w", version, Repository, err)
	case err != nil:
		return res, fmt.Errorf("looking up release %s of %s: %w", version, Repository, err)
	case !found || "v"+rel.Version() != version:
		return res, fmt.Errorf("GitHub answered no release %s of %s that carries a binary for %s/%s", version, Repository, runtime.GOOS, runtime.GOARCH)
	}
	res.Published, res.URL, res.Newer = rel.PublishedAt, rel.URL, newer(version, res.Current)
	if res.Path, err = executable(); err != nil {
		return res, fmt.Errorf("locating the running executable: %w", err)
	}
	target, err := filepath.EvalSymlinks(res.Path)
	if err != nil {
		return res, err
	}
	if err := keepPrevious(target); err != nil {
		return res, fmt.Errorf("keeping %s for a rollback: %w", target, err)
	}
	if err := selfupdatecosign.Install(ctx, up, rel, res.Path); err != nil {
		return res, fmt.Errorf("updating %s failed, it is unchanged: %w", res.Path, err)
	}
	res.Updated = true
	_, _ = fmt.Fprintf(w, "Verified the signature and updated %s from %s to %s.\n", res.Path, res.Current, version)
	return res, nil
}

// Previous is where InstallVersion keeps the binary it replaced at the
// resolved executable path target: hidden beside it, so the directory's
// bare beekeeper stays the one installed.
func Previous(target string) string {
	return filepath.Join(filepath.Dir(target), "."+filepath.Base(target)+".previous")
}

// keepPrevious links the binary at target to Previous(target), replacing an
// older one: the install renames over target, and the link keeps the
// replaced file, mode included, without a copy.
func keepPrevious(target string) error {
	prev := Previous(target)
	if err := os.Remove(prev); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return os.Link(target, prev)
}

// Rollback puts the binary InstallVersion replaced back over the executable
// at path in one rename.
func Rollback(path string) error {
	target, err := filepath.EvalSymlinks(path)
	if err != nil {
		return err
	}
	return os.Rename(Previous(target), target)
}
