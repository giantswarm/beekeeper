// Package update replaces the running beekeeper with the latest signed
// release.
package update

import (
	"context"
	"errors"
	"fmt"
	"io"
	"runtime"
	"time"

	"github.com/Masterminds/semver/v3"
	"github.com/creativeprojects/go-selfupdate"
	selfupdatecosign "github.com/giantswarm/selfupdate-cosign"

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

// Seams the tests replace: where releases come from (nil is public GitHub,
// anonymous unless GITHUB_TOKEN is set), what a download must verify against
// (the shared cosign validator: a CircleCI build of Repository, against the
// Sigstore public-good trust root), the file the update replaces and the
// running version.
var (
	newSource      = func() selfupdate.Source { return nil }
	newValidator   = func() selfupdate.Validator { return selfupdatecosign.New(Repository) }
	executable     = selfupdate.ExecutablePath
	currentVersion = project.Version
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
// verifies. Progress goes to w. With checkOnly the error is ErrOutdated when
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

	up, err := selfupdate.NewUpdater(selfupdate.Config{Source: newSource(), Validator: newValidator()})
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
		return res, fmt.Errorf("looking up the latest release of %s (is this machine online?): %w", Repository, err)
	case !found:
		return res, fmt.Errorf("no release of %s carries a binary for %s/%s", Repository, runtime.GOOS, runtime.GOARCH)
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
