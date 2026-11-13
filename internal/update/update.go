// Package update replaces the running beekeeper with the latest signed
// release.
package update

import (
	"context"
	"errors"
	"fmt"
	"io"
	"runtime"
	"strings"
	"time"

	"github.com/Masterminds/semver/v3"
	"github.com/giantswarm/go-selfupdate"
	selfupdatecosign "github.com/giantswarm/selfupdate-cosign"
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

// ErrHealthUnproven leaves probation pending when live health evidence is
// unavailable. It neither promotes nor quarantines the installed candidate.
var ErrHealthUnproven = errors.New("candidate health is unproven")

// Seams the tests replace: where releases come from (GitHub, authenticated
// with the environment's token or the gh login, never anonymously; empty tag
// responses are retried),
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

// Canary checks a verified candidate binary at path for version before it
// replaces the installed executable. Returning an error keeps the current
// executable in place.
type Canary func(ctx context.Context, path, version string) error

// Health checks the executable at path, the installed candidate of version.
// Return ErrHealthUnproven (possibly wrapped) for transient or unreadable evidence.
type Health func(ctx context.Context, path, version string) error

// Options configures a Run call through its context.
type Options struct {
	// Version selects an exact release tag, including pre-releases.
	Version string
	Canary  Canary
	// Health checks the installed candidate before an update promotes it. Nil,
	// that the candidate runs the update is its evidence.
	Health Health
	Skip   func(tag string) bool
	// Recovery launches the controller that supervises the candidate about to
	// replace target, from the verified recovery binary at artifact. It runs
	// before the replacement: a candidate that never starts cannot prevent it.
	// Nil or an error leaves the candidate unwatched, promoted by its next update.
	Recovery Recovery
	// HostRollback authorizes an execution host to follow its coordinator back
	// to the verified previous release during probation. The guard checks live
	// state compatibility and fences candidate writers under the state lock.
	// Ordinary self-updates leave this nil and wait for the controller verdict.
	HostRollback func(previous, failed string, restore func() error) error
}

// Recovery launches a recovery controller; see Options.Recovery.
type Recovery func(ctx context.Context, previous Binary, artifact, target string) error

type optionsKey struct{}

// WithOptions attaches opts to ctx for Run.
func WithOptions(ctx context.Context, opts Options) context.Context {
	return context.WithValue(ctx, optionsKey{}, opts)
}

// OptionsFrom returns the Options attached to ctx.
func OptionsFrom(ctx context.Context) Options {
	opts, _ := ctx.Value(optionsKey{}).(Options)
	return opts
}

func optionsFrom(ctx context.Context) Options {
	return OptionsFrom(ctx)
}

type skipSource struct {
	selfupdate.Source
	skip    func(string) bool
	skipped bool
}

func (s *skipSource) ListReleases(ctx context.Context, repository selfupdate.Repository) ([]selfupdate.SourceRelease, error) {
	releases, err := s.Source.ListReleases(ctx, repository)
	if g, ok := s.Source.(*githubSource); ok && g.skipped {
		s.skipped = true
	}
	if err != nil || s.skip == nil {
		return releases, err
	}
	kept := make([]selfupdate.SourceRelease, 0, len(releases))
	for _, r := range releases {
		tag := r.GetTagName()
		if s.skip(tag) || s.skip("v"+strings.TrimPrefix(tag, "v")) {
			s.skipped = true
			continue
		}
		kept = append(kept, r)
	}
	return kept, nil
}

// Run looks up the latest release and, unless checkOnly, installs its binary
// for this OS and architecture over the running executable once its bundle
// verifies. A running pre-release looks among the pre-releases too, a
// running release only among releases. Progress goes to w. With checkOnly the error is ErrOutdated when
// a newer release exists.
//
// The binary is renamed over the executable in one step, symbolic links
// resolved: a beekeeper watch that runs keeps running the old binary, every
// process started afterwards runs the new one. Concurrent updates serialize
// through a durable journal lock. A development build, a release without a bundle for this
// platform's binary and a download that does not verify are refused, and the
// executable stays as it is.
func Run(ctx context.Context, w io.Writer, checkOnly bool) (Result, error) {
	opts := optionsFrom(ctx)
	if opts.Version != "" {
		return selectVersion(ctx, w, opts.Version, checkOnly, func() (selfupdate.Source, error) { return newTagSource(opts.Version) })
	}
	res := Result{Current: currentVersion()}
	if _, err := semver.NewVersion(res.Current); err != nil {
		return res, fmt.Errorf("cannot self-update a development build (version %s): install a release from %s/releases", res.Current, project.Source)
	}

	source, err := newSource(prerelease(res.Current))
	if err != nil {
		return res, fmt.Errorf("creating release source: %w", err)
	}
	filtered := &skipSource{Source: source, skip: opts.Skip}
	up, err := selfupdate.NewUpdater(selfupdate.Config{Source: filtered, Validator: newValidator(), Prerelease: prerelease(res.Current)})
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
		if limit := rateLimit(err); limit != "" {
			return res, fmt.Errorf("looking up the latest release of %s: %s: %w", Repository, limit, err)
		}
		if errors.Is(err, errIncompleteLookup) {
			return res, fmt.Errorf("looking up the latest release of %s: %w", Repository, err)
		}
		return res, fmt.Errorf("looking up the latest release of %s (is this machine online?): %w", Repository, err)
	case !found && filtered.skipped:
		res.Latest = res.Current
		_, _ = fmt.Fprintf(w, "beekeeper %s is the latest release.\n", res.Current)
		return res, nil
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
	if err := installRelease(ctx, up, rel, res.Path, source); err != nil {
		return res, installFailed(res.Path, err)
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
// one. The verified download is staged beside the destination and renamed
// over it atomically, retaining a verified recovery target once probation has
// succeeded. First installation has no trusted recovery target.
func InstallVersion(ctx context.Context, w io.Writer, version string) (Result, error) {
	return installVersion(ctx, w, version, func() (selfupdate.Source, error) { return newTagSource(version) })
}

// releaseSemver is release version (a tag, "v0.3.0") parsed, or why it is
// none.
func releaseSemver(version string) (*semver.Version, error) {
	want, err := semver.StrictNewVersion(strings.TrimPrefix(version, "v"))
	if err != nil || !strings.HasPrefix(version, "v") {
		return nil, fmt.Errorf("%q is no release version (v<major>.<minor>.<patch>)", version)
	}
	return want, nil
}

// installVersion installs release version, which newSource lists, as
// InstallVersion says.
func installVersion(ctx context.Context, w io.Writer, version string, newSource func() (selfupdate.Source, error)) (Result, error) {
	return selectVersion(ctx, w, version, false, newSource)
}

// selectVersion checks or installs an exact tag through the verified installer.
func selectVersion(ctx context.Context, w io.Writer, version string, checkOnly bool, newSource func() (selfupdate.Source, error)) (Result, error) {
	res := Result{Current: currentVersion(), Latest: version}
	want, err := releaseSemver(version)
	if err != nil {
		return res, err
	}
	if _, err := semver.NewVersion(res.Current); err != nil {
		return res, fmt.Errorf("cannot update a development build (version %s) to a release: install %s by hand", res.Current, version)
	}
	if version == res.Current {
		_, _ = fmt.Fprintf(w, "beekeeper %s is already installed.\n", version)
		return res, nil
	}
	source, err := newSource()
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
		if limit := rateLimit(err); limit != "" {
			return res, fmt.Errorf("looking up release %s of %s: %s: %w", version, Repository, limit, err)
		}
		return res, fmt.Errorf("looking up release %s of %s: %w", version, Repository, err)
	case !found || "v"+rel.Version() != version:
		return res, fmt.Errorf("GitHub answered no release %s of %s that carries a binary for %s/%s", version, Repository, runtime.GOOS, runtime.GOARCH)
	}
	res.Published, res.URL, res.Newer = rel.PublishedAt, rel.URL, newer(version, res.Current)
	if checkOnly {
		_, _ = fmt.Fprintf(w, "beekeeper %s is available, this is %s.\n", version, res.Current)
		if res.Newer {
			return res, ErrOutdated
		}
		return res, nil
	}
	if res.Path, err = executable(); err != nil {
		return res, fmt.Errorf("locating the running executable: %w", err)
	}
	if err := installRelease(ctx, up, rel, res.Path, source); err != nil {
		return res, installFailed(res.Path, err)
	}
	res.Updated = true
	_, _ = fmt.Fprintf(w, "Verified the signature and updated %s from %s to %s.\n", res.Path, res.Current, version)
	return res, nil
}
