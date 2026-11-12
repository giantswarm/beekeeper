package update

import (
	"context"
	"errors"
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

// errIncompleteLookup is a lookup that read maxReleaseCandidates tags
// without a signed binary: GitHub answered, so it is no question of being
// online.
var errIncompleteLookup = errors.New("refusing an incomplete lookup")

type githubSource struct {
	selfupdate.Source // retain the upstream asset downloader
	api               *github.Client
	prereleases       bool
	current           string
	commit            string
	skipped           bool
}

type releaseCandidate struct {
	tag        string
	version    *semver.Version
	sha        string
	promotedBy []string
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

// authToken is the token the release lookup sends: GH_TOKEN, GITHUB_TOKEN
// or the gh login's, as every other GitHub call of beekeeper; errNoToken
// without one.
func authToken() (string, error) {
	token := os.Getenv("GH_TOKEN")
	if token == "" {
		token = os.Getenv("GITHUB_TOKEN")
	}
	if token == "" {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		token, _ = githubToken(ctx)
		cancel()
	}
	if token == "" {
		return "", errNoToken
	}
	return token, nil
}

func newGitHubSource(prereleases bool) (selfupdate.Source, error) {
	token, err := authToken()
	if err != nil {
		return nil, err
	}
	cfg := selfupdate.GitHubConfig{APIToken: token}
	opts := []github.ClientOptionsFunc{github.WithAuthToken(token)}
	// Only a scenario build names another API than GitHub's (apiBase).
	if base := apiBase(); base != "" {
		cfg.EnterpriseBaseURL = base
		opts = append(opts, github.WithEnterpriseURLs(base, base))
	}
	source, err := selfupdate.NewGitHubSource(cfg)
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

// tagSource lists exactly one release, the tag's, signed binary or not: the
// validator refuses one without its bundle.
type tagSource struct {
	selfupdate.Source // retain the upstream asset downloader
	api               *github.Client
	tag               string
}

// newGitHubTagSource is the source of release tag alone, authenticated as
// newGitHubSource is.
func newGitHubTagSource(tag string) (selfupdate.Source, error) {
	s, err := newGitHubSource(true)
	if err != nil {
		return nil, err
	}
	g := s.(*githubSource)
	return &tagSource{Source: g.Source, api: g.api, tag: tag}, nil
}

func (s *tagSource) ListReleases(ctx context.Context, repository selfupdate.Repository) ([]selfupdate.SourceRelease, error) {
	owner, repo, err := repository.GetSlug()
	if err != nil {
		return nil, err
	}
	if err := deskgithub.AdmitRESTCall(ctx, "update.tagSource.ListReleases"); err != nil {
		return nil, err
	}
	release, response, err := s.api.Repositories.GetReleaseByTag(ctx, owner, repo, s.tag)
	if response != nil && response.StatusCode == http.StatusNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if release.GetDraft() || release.GetTagName() != s.tag {
		return nil, nil
	}
	return []selfupdate.SourceRelease{selfupdate.NewGitHubRelease(release)}, nil
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
		if err := deskgithub.AdmitRESTCall(ctx, "update.githubSource.ListReleases.refs"); err != nil {
			return nil, err
		}
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
	// Why each candidate was passed over, for an incomplete lookup's error.
	var skipped []string
	skip := optionsFrom(ctx).Skip
	checked := 0
	pending := make(map[string]bool, len(set.candidates))
	for _, candidate := range set.candidates {
		pending[candidate.tag] = true
	}
	for len(set.candidates) != 0 {
		// Try a stable promotion before its candidates, but retain them as
		// fallbacks until the promotion has a usable published release.
		index := 0
		for i, candidate := range set.candidates {
			blocked := false
			for _, tag := range candidate.promotedBy {
				blocked = blocked || pending[tag]
			}
			if !blocked {
				index = i
				break
			}
		}
		candidate := set.candidates[index]
		set.candidates = append(set.candidates[:index], set.candidates[index+1:]...)
		delete(pending, candidate.tag)
		if skip != nil && (skip(candidate.tag) || skip("v"+strings.TrimPrefix(candidate.tag, "v"))) {
			s.skipped = true
			continue
		}
		if checked == maxReleaseCandidates {
			return nil, fmt.Errorf("no signed binary found in the newest %d eligible release tags (%s); %w", maxReleaseCandidates, strings.Join(skipped, "; "), errIncompleteLookup)
		}
		checked++
		stale, err := s.isStaleCandidate(ctx, owner, repo, candidate, set, runningSHA, staleSHAs)
		if err != nil {
			return nil, err
		}
		if stale != "" {
			if candidate.sha != "" {
				staleSHAs[candidate.sha] = true
			}
			skipped = append(skipped, candidate.tag+": "+stale)
			continue
		}
		if err := deskgithub.AdmitRESTCall(ctx, "update.githubSource.ListReleases.release"); err != nil {
			return nil, err
		}
		release, response, err := s.api.Repositories.GetReleaseByTag(ctx, owner, repo, candidate.tag)
		if response != nil && response.StatusCode == http.StatusNotFound {
			skipped = append(skipped, candidate.tag+": no release")
			continue
		} // tag without a release
		if err != nil {
			return nil, err
		}
		if release.GetDraft() {
			skipped = append(skipped, candidate.tag+": a draft release")
			continue
		}
		if !s.prereleases && release.GetPrerelease() {
			skipped = append(skipped, candidate.tag+": a pre-release")
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
		missing := name
		if binary {
			missing += ".bundle"
		}
		// Published metadata precedes the asset upload job. Keep looking for
		// the newest usable release rather than blocking every update on it.
		skipped = append(skipped, candidate.tag+": no "+missing)
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
	stableTags := make(map[string][]string)
	promotedBase := make(map[baseKey][]string)
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
			// A running release never follows a pre-release: a later base's
			// release candidate leaves the releases before it eligible.
			if s.prereleases && latestRC.version != nil && latestRC.sha != "" && !sameCommit(c.sha, latestRC.sha) && c.version.LessThan(latestRC.version) {
				if rcVer := allRCSHAs[c.sha]; rcVer != nil && rcVer.LessThan(latestRC.version) {
					continue
				}
			}
			stableSHAs[c.sha] = true
			stableTags[c.sha] = append(stableTags[c.sha], c.tag)
			if rc, ok := newestRC[bk]; !ok || sameCommit(c.sha, rc.sha) {
				promotedBase[bk] = append(promotedBase[bk], c.tag)
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
			c.promotedBy = append(c.promotedBy, stableTags[c.sha]...)
			c.promotedBy = append(c.promotedBy, promotedBase[bk]...)
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

// isStaleCandidate says why candidate c is behind a commit the lookup must
// not go back from, "" when it is not.
func (s *githubSource) isStaleCandidate(ctx context.Context, owner, repo string, c releaseCandidate, set candidateSet, runningSHA string, staleSHAs map[string]bool) (string, error) {
	if c.sha == "" {
		return "", nil
	}
	if c.version.Prerelease() == "" {
		bk := baseKey{c.version.Major(), c.version.Minor(), c.version.Patch()}
		if rc, ok := set.newestRC[bk]; ok && rc.sha != "" && !sameCommit(c.sha, rc.sha) {
			if why, err := s.behindWhy(ctx, owner, repo, rc, c.sha); why != "" || err != nil {
				return why, err
			}
		}
		// Only a running pre-release follows the newest release candidate:
		// on a release, the newest release is the latest whatever
		// pre-releases a later base has.
		if s.prereleases && set.latestRC.sha != "" && !set.stableSHAs[set.latestRC.sha] && !sameCommit(c.sha, set.latestRC.sha) {
			if why, err := s.behindWhy(ctx, owner, repo, set.latestRC, c.sha); why != "" || err != nil {
				return why, err
			}
		}
	} else if set.latestStable.sha != "" && !staleSHAs[set.latestStable.sha] && !set.olderTagSHAs[set.latestStable.sha] && !sameCommit(c.sha, set.latestStable.sha) && (runningSHA == "" || !sameCommit(c.sha, runningSHA)) {
		if why, err := s.behindWhy(ctx, owner, repo, set.latestStable, c.sha); why != "" || err != nil {
			return why, err
		}
	}
	if runningSHA != "" && !sameCommit(c.sha, runningSHA) {
		if set.olderTagSHAs[c.sha] {
			return "at the commit of a release older than this one", nil
		}
		return s.behindWhy(ctx, owner, repo, rcRef{sha: runningSHA}, c.sha)
	}
	return "", nil
}

// behindWhy says that head is behind ref's commit, "" when it is not.
func (s *githubSource) behindWhy(ctx context.Context, owner, repo string, ref rcRef, head string) (string, error) {
	behind, err := s.isBehind(ctx, owner, repo, ref.sha, head)
	if err != nil || !behind {
		return "", err
	}
	if ref.version == nil {
		return "behind the running commit " + shortSHA(ref.sha), nil
	}
	return "behind " + ref.version.Original() + " at " + shortSHA(ref.sha), nil
}

// shortSHA is a commit's abbreviation for a message.
func shortSHA(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
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
	if err := deskgithub.AdmitRESTCall(ctx, "update.githubSource.compare"); err != nil {
		return false, err
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
