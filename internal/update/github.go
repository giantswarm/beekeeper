package update

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/Masterminds/semver/v3"
	"github.com/giantswarm/go-selfupdate"
	"github.com/google/go-github/v93/github"

	deskgithub "github.com/giantswarm/beekeeper/internal/github"
)

// githubToken uses the shared gh login resolver; tests replace it without credentials.
var githubToken = deskgithub.Token

// Bound candidate reads when tags have no published, signed binary. A normal
// lookup uses two small requests, independent of the release history's size.
const maxReleaseCandidates = 8
const emptyRefAttempts = 3

type githubSource struct {
	selfupdate.Source // retain the upstream asset downloader
	api               *github.Client
	prereleases       bool
	current           string
	commit            string
}

type releaseCandidate struct {
	tag     string
	version *semver.Version
	sha     string
}

type baseKey struct {
	major, minor, patch uint64
}

type rcRef struct {
	version *semver.Version
	sha     string
}

type candidateSet struct {
	candidates   []releaseCandidate
	newestRC     map[baseKey]rcRef
	latestRC     rcRef
	latestStable rcRef
	stableSHAs   map[string]bool
	tagSHA       map[string]string
	olderTagSHAs map[string]bool
}

func newGitHubSource(prereleases bool) (selfupdate.Source, error) {
	var opts []github.ClientOptionsFunc
	token := os.Getenv("GH_TOKEN")
	if token == "" {
		token = os.Getenv("GITHUB_TOKEN")
	}
	if token == "" {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		token, _ = githubToken(ctx)
		cancel()
	}
	if token != "" {
		opts = append(opts, github.WithAuthToken(token))
	}
	source, err := selfupdate.NewGitHubSource(selfupdate.GitHubConfig{APIToken: token})
	if err != nil {
		return nil, err
	}
	client, err := github.NewClient(opts...)
	if err != nil {
		return nil, err
	}
	return &githubSource{
		Source:      source,
		api:         client,
		prereleases: prereleases,
		current:     currentVersion(),
		commit:      currentCommit(),
	}, nil
}

func (s *githubSource) ListReleases(ctx context.Context, repository selfupdate.Repository) ([]selfupdate.SourceRelease, error) {
	owner, repo, err := repository.GetSlug()
	if err != nil {
		return nil, err
	}
	// Release lists include every historical body and asset and stop at 1,000
	// anonymous results. Matching refs carry just tag metadata without that cap.
	var refs []*github.Reference
	for attempt := 0; attempt < emptyRefAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var response *github.Response
		refs, response, err = s.api.Git.ListMatchingRefs(ctx, owner, repo, "tags/v")
		if err != nil {
			if response != nil && response.StatusCode == http.StatusNotFound {
				return nil, nil
			}
			return nil, err
		}
		if len(refs) != 0 {
			break
		}
	}
	set := s.collectCandidates(refs)
	runningSHA := s.runningSHA(set.tagSHA)
	staleSHAs := make(map[string]bool)
	for i, candidate := range set.candidates {
		if i == maxReleaseCandidates {
			return nil, fmt.Errorf("no signed binary found in the newest %d eligible release tags; refusing an incomplete lookup", maxReleaseCandidates)
		}
		stale, err := s.isStaleCandidate(ctx, owner, repo, candidate, set, runningSHA, staleSHAs)
		if err != nil {
			return nil, err
		}
		if stale {
			if candidate.sha != "" {
				staleSHAs[candidate.sha] = true
			}
			continue
		}
		release, response, err := s.api.Repositories.GetReleaseByTag(ctx, owner, repo, candidate.tag)
		if response != nil && response.StatusCode == http.StatusNotFound {
			continue
		} // tag without a release
		if err != nil {
			return nil, err
		}
		if release.GetDraft() || (!s.prereleases && release.GetPrerelease()) {
			continue
		}
		if release.GetTagName() != candidate.tag {
			return nil, fmt.Errorf("release tag %q does not match requested tag %q", release.GetTagName(), candidate.tag)
		}
		name := "beekeeper-" + runtime.GOOS + "-" + runtime.GOARCH
		if runtime.GOOS == "windows" {
			name += ".exe"
		}
		binary, bundle := false, false
		for _, asset := range release.Assets {
			binary = binary || asset.GetName() == name
			bundle = bundle || asset.GetName() == name+".bundle"
		}
		if binary && bundle {
			return []selfupdate.SourceRelease{selfupdate.NewGitHubRelease(release)}, nil
		}
	}
	return nil, nil
}

func (s *githubSource) collectCandidates(refs []*github.Reference) candidateSet {
	newestRC := make(map[baseKey]rcRef)
	rcSHAs := make(map[baseKey]map[string]*semver.Version)
	allRCSHAs := make(map[string]*semver.Version)
	var latestRC rcRef
	tagSHA := make(map[string]string, len(refs))
	olderTagSHAs := make(map[string]bool)
	currentVer, _ := semver.NewVersion(s.current)
	raw := make([]releaseCandidate, 0, len(refs))
	for _, ref := range refs {
		tag := strings.TrimPrefix(ref.GetRef(), "refs/tags/")
		version, err := semver.NewVersion(tag)
		if err != nil {
			continue
		}
		sha := ref.GetObject().GetSHA()
		if sha != "" {
			tagSHA[tag] = sha
			if currentVer != nil && version.LessThan(currentVer) {
				olderTagSHAs[sha] = true
			}
		}
		if version.Prerelease() != "" && sha != "" {
			bk := baseKey{version.Major(), version.Minor(), version.Patch()}
			if rcSHAs[bk] == nil {
				rcSHAs[bk] = make(map[string]*semver.Version)
			}
			if prev := rcSHAs[bk][sha]; prev == nil || version.GreaterThan(prev) {
				rcSHAs[bk][sha] = version
			}
			if prev := allRCSHAs[sha]; prev == nil || version.GreaterThan(prev) {
				allRCSHAs[sha] = version
			}
			if cur := newestRC[bk]; cur.version == nil || version.GreaterThan(cur.version) {
				newestRC[bk] = rcRef{version: version, sha: sha}
			}
			if latestRC.version == nil || version.GreaterThan(latestRC.version) {
				latestRC = rcRef{version: version, sha: sha}
			}
		}
		if !s.prereleases && version.Prerelease() != "" {
			continue
		}
		raw = append(raw, releaseCandidate{tag: tag, version: version, sha: sha})
	}
	stableSHAs := make(map[string]bool)
	promotedBase := make(map[baseKey]bool)
	var latestStable rcRef
	validRaw := make([]releaseCandidate, 0, len(raw))
	for _, c := range raw {
		if c.version.Prerelease() == "" && c.sha != "" {
			bk := baseKey{c.version.Major(), c.version.Minor(), c.version.Patch()}
			if rc, ok := newestRC[bk]; ok && rc.sha != "" && !sameCommit(c.sha, rc.sha) {
				if rcVer := rcSHAs[bk][c.sha]; rcVer != nil && rcVer.LessThan(rc.version) {
					continue
				}
			}
			if latestRC.version != nil && latestRC.sha != "" && !sameCommit(c.sha, latestRC.sha) && c.version.LessThan(latestRC.version) {
				if rcVer := allRCSHAs[c.sha]; rcVer != nil && rcVer.LessThan(latestRC.version) {
					continue
				}
			}
			stableSHAs[c.sha] = true
			if rc, ok := newestRC[bk]; !ok || sameCommit(c.sha, rc.sha) {
				promotedBase[bk] = true
			}
			if latestStable.version == nil || c.version.GreaterThan(latestStable.version) {
				latestStable = rcRef{version: c.version, sha: c.sha}
			}
		}
		validRaw = append(validRaw, c)
	}
	candidates := make([]releaseCandidate, 0, len(validRaw))
	for _, c := range validRaw {
		if c.version.Prerelease() != "" && c.sha != "" && c.tag != s.current {
			bk := baseKey{c.version.Major(), c.version.Minor(), c.version.Patch()}
			if stableSHAs[c.sha] || promotedBase[bk] {
				continue
			}
		}
		candidates = append(candidates, c)
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].version.Equal(candidates[j].version) {
			return candidates[i].tag < candidates[j].tag
		}
		return candidates[i].version.GreaterThan(candidates[j].version)
	})
	return candidateSet{
		candidates:   candidates,
		newestRC:     newestRC,
		latestRC:     latestRC,
		latestStable: latestStable,
		stableSHAs:   stableSHAs,
		tagSHA:       tagSHA,
		olderTagSHAs: olderTagSHAs,
	}
}

func (s *githubSource) isStaleCandidate(ctx context.Context, owner, repo string, c releaseCandidate, set candidateSet, runningSHA string, staleSHAs map[string]bool) (bool, error) {
	if c.sha == "" {
		return false, nil
	}
	if c.version.Prerelease() == "" {
		bk := baseKey{c.version.Major(), c.version.Minor(), c.version.Patch()}
		if rc, ok := set.newestRC[bk]; ok && rc.sha != "" && !sameCommit(c.sha, rc.sha) {
			behind, err := s.isBehind(ctx, owner, repo, rc.sha, c.sha)
			if err != nil || behind {
				return behind, err
			}
		}
		if set.latestRC.sha != "" && !set.stableSHAs[set.latestRC.sha] && !sameCommit(c.sha, set.latestRC.sha) {
			behind, err := s.isBehind(ctx, owner, repo, set.latestRC.sha, c.sha)
			if err != nil || behind {
				return behind, err
			}
		}
	} else if set.latestStable.sha != "" && !staleSHAs[set.latestStable.sha] && !set.olderTagSHAs[set.latestStable.sha] && !sameCommit(c.sha, set.latestStable.sha) && (runningSHA == "" || !sameCommit(c.sha, runningSHA)) {
		behind, err := s.isBehind(ctx, owner, repo, set.latestStable.sha, c.sha)
		if err != nil || behind {
			return behind, err
		}
	}
	if runningSHA != "" && !sameCommit(c.sha, runningSHA) {
		if set.olderTagSHAs[c.sha] {
			return true, nil
		}
		return s.isBehind(ctx, owner, repo, runningSHA, c.sha)
	}
	return false, nil
}

func (s *githubSource) runningSHA(tagSHA map[string]string) string {
	if s.commit != "" && s.commit != "unknown" {
		return s.commit
	}
	return tagSHA[s.current]
}

func (s *githubSource) isBehind(ctx context.Context, owner, repo, base, head string) (bool, error) {
	if base == "" || head == "" || sameCommit(base, head) {
		return false, nil
	}
	comp, resp, err := s.api.Repositories.CompareCommits(ctx, owner, repo, base, head, nil)
	if resp != nil && resp.StatusCode == http.StatusNotFound {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return comp.GetStatus() == "behind", nil
}

func sameCommit(a, b string) bool {
	return a == b || strings.HasPrefix(a, b) || strings.HasPrefix(b, a)
}
