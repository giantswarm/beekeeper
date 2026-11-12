//go:build scenario

package scenario

import (
	"fmt"
	"path"
	"runtime"
	"slices"

	"github.com/Masterminds/semver/v3"

	"github.com/giantswarm/beekeeper/internal/scenario/fakegithub"
	"github.com/giantswarm/beekeeper/internal/update"
)

// Releases are beekeeper's own releases in a scenario, for self-update.
// Each is a release build of the candidate: the same source, built with
// the scenario tag and stamped with its version as a release is
// (-ldflags -X …/pkg/project.version). So an update between two of them
// replaces one real binary with another, whose bytes and versions differ.
type Releases struct {
	// Installed is the release the desk runs: /work/bin/beekeeper is its
	// build. Empty, the desk runs the candidate as the suite built it,
	// whose version is the checkout's.
	Installed string `yaml:"installed"`
	// Published are the releases of giantswarm/beekeeper on the scenario's
	// GitHub, each with this platform's binary and its bundle, tagged on
	// the repository's main. A version with a pre-release part is a
	// pre-release there.
	Published []string `yaml:"published"`
}

// ReleaseBuild names one build of the candidate. Its fields are what
// distinguishes the build from the candidate's own, so a further kind of
// build (another source, further tags) is a further field.
type ReleaseBuild struct {
	// Version is the version the build is stamped with ("v1.2.0").
	Version string
	// Mutation names a defect of Mutations the build carries; empty for
	// the source as it is.
	Mutation string
}

// Name names the build in the sandbox and in a run's release: its
// version, with "+<mutation>" for a mutated build.
func (b ReleaseBuild) Name() string {
	if b.Mutation == "" {
		return b.Version
	}
	return b.Version + "+" + b.Mutation
}

// releasesDir holds the release builds inside the sandbox:
// <releasesDir>/<version>/<asset> and <asset>.bundle beside it.
const releasesDir = "/work/releases"

// ReleaseAsset is the name of this platform's binary among a release's
// assets, as the release pipeline names it.
func ReleaseAsset() string {
	name := "beekeeper-" + runtime.GOOS + "-" + runtime.GOARCH
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	return name
}

// releaseBinary is the release build of that name (ReleaseBuild.Name)
// inside the sandbox.
func releaseBinary(name string) string {
	return path.Join(releasesDir, name, ReleaseAsset())
}

// builds are the release builds the scenario needs, each once.
func (r Releases) builds() []ReleaseBuild {
	var out []ReleaseBuild
	for _, v := range append([]string{r.Installed}, r.Published...) {
		if b := (ReleaseBuild{Version: v}); v != "" && !slices.Contains(out, b) {
			out = append(out, b)
		}
	}
	return out
}

// builds are every build the scenario needs, each once: given.releases'
// and given.install's.
func (g *Given) builds() []ReleaseBuild {
	out := g.Releases.builds()
	for _, b := range g.Install.builds() {
		if !slices.Contains(out, b) {
			out = append(out, b)
		}
	}
	return out
}

// knownBuild says whether name (ReleaseBuild.Name) is one of the
// scenario's builds.
func (g *Given) knownBuild(name string) bool {
	return slices.ContainsFunc(g.builds(), func(b ReleaseBuild) bool { return b.Name() == name })
}

// validate is what the loader refuses of given.releases.
func (r Releases) validate() []string {
	var out []string
	seen := map[string]bool{}
	for _, v := range append([]string{r.Installed}, r.Published...) {
		if v == "" {
			continue
		}
		// The updater and the recovery fence order releases by their
		// version: a tag, without build metadata.
		parsed, err := semver.StrictNewVersion(v[min(1, len(v)):])
		if err != nil || v[0] != 'v' || parsed.Metadata() != "" {
			out = append(out, fmt.Sprintf("%q is no release version (v<major>.<minor>.<patch>[-<pre-release>])", v))
		}
	}
	for _, v := range r.Published {
		if seen[v] {
			out = append(out, fmt.Sprintf("published names %s twice", v))
		}
		seen[v] = true
	}
	if slices.Contains(r.Published, "") {
		out = append(out, "published names an empty version")
	}
	return out
}

// publish puts the published releases on the scenario's GitHub, which a
// scenario that names none gets for them: the repository the updater
// asks, a release per version with the binary and its bundle as assets.
// The assets are files of the sandbox, read at each download.
func (g *Given) publishReleases() {
	if len(g.Releases.Published) == 0 {
		return
	}
	if g.GitHub == nil {
		g.GitHub = &fakegithub.Given{}
	}
	i := slices.IndexFunc(g.GitHub.Repositories, func(r fakegithub.GivenRepo) bool { return r.Name == update.Repository })
	if i < 0 {
		g.GitHub.Repositories = append(g.GitHub.Repositories, fakegithub.GivenRepo{Name: update.Repository})
		i = len(g.GitHub.Repositories) - 1
	}
	repo := &g.GitHub.Repositories[i]
	for _, v := range g.Releases.Published {
		pre := false
		if parsed, err := semver.NewVersion(v); err == nil {
			pre = parsed.Prerelease() != ""
		}
		binary := releaseBinary(v)
		repo.Releases = append(repo.Releases, fakegithub.GivenRelease{
			Tag: v, Prerelease: pre,
			Assets: []fakegithub.GivenAsset{
				{Name: ReleaseAsset(), File: binary},
				{Name: ReleaseAsset() + ".bundle", File: binary + ".bundle"},
			},
		})
	}
}
