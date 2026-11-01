package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

// AutoReleaseWorkflow is the Auto-release workflow devctl generates: on a
// push to a branch its on.push filter names, it tags the merge commit.
const AutoReleaseWorkflow = ".github/workflows/zz_generated.auto_release.yaml"

// PluginReleaseWorkflow is the Auto Release of a plugin marketplace: on a
// merged pull request it tags every plugin whose version in Marketplace the
// merge changed, <plugin>/vX.Y.Z (PluginTag).
const PluginReleaseWorkflow = ".github/workflows/auto-release.yaml"

// Marketplace is a plugin marketplace's manifest, its plugins' names and
// versions.
const Marketplace = ".claude-plugin/marketplace.json"

// The release-PR flow devctl generates in place of Auto-release:
// CreateReleasePRWorkflow opens a release pull request (head
// <base>#release#<level>, title "chore(release): vX.Y.Z") on a push of such
// a branch, and CreateReleaseWorkflow, on a push to a branch its on.push
// filter names, tags vX.Y.Z when the pushed commit's subject is that title:
// the merge of the release pull request.
const (
	CreateReleasePRWorkflow = ".github/workflows/zz_generated.create_release_pr.yaml"
	CreateReleaseWorkflow   = ".github/workflows/zz_generated.create_release.yaml"
)

// releaseTitle is the title of a release pull request, the subject
// CreateReleaseWorkflow tags from: chore(release) or the older Release,
// then the version.
var releaseTitle = regexp.MustCompile(`(?i)^(?:chore\(release\):|Release) (v\d+\.\d+\.\d+(?:-[0-9A-Za-z.-]+)?)$`)

// ReleaseVersion is the tag the merge of a release pull request with head
// branch head and title cuts, "" when it is no release pull request.
func ReleaseVersion(head, title string) string {
	if !strings.Contains(head, "release#") {
		return ""
	}
	if m := releaseTitle.FindStringSubmatch(strings.TrimSpace(title)); m != nil {
		return m[1]
	}
	return ""
}

// BaseRelease is how a pull request's base branch releases: Auto when the
// Auto-release workflow on the branch runs on a push to it, so a merge into
// it is tagged; Plugins when the branch has no such workflow and its
// PluginReleaseWorkflow tags the merged plugins (PluginTags); ReleasePR when
// it has neither and releases through the release-PR flow
// (CreateReleaseWorkflow), whose release pull request's merge tags Version;
// otherwise its tags are cut by hand (a maintenance branch, a fork line's
// backport branch).
type BaseRelease struct {
	Base      string
	Auto      bool
	Plugins   bool
	ReleasePR bool
	// Version is the tag the merge of the pull request cuts when it is the
	// release pull request of a ReleasePR branch (ReleaseVersion), "" for
	// any other pull request.
	Version string
}

// PullBase is a pull request's base branch, and the head branch and title
// that make it a release pull request (ReleaseVersion).
type PullBase struct {
	Base  string `json:"base"`
	Head  string `json:"head"`
	Title string `json:"title"`
}

// ReadPullBase reads repo#n's base branch, head branch and title, one REST
// request: the core budget the merge gate checks, not GraphQL's.
func ReadPullBase(ctx context.Context, run GH, repo string, n int) (PullBase, error) {
	out, err := run(ctx, "api", fmt.Sprintf("repos/%s/pulls/%d", repo, n),
		"--jq", "{base: .base.ref, head: .head.ref, title: .title}")
	if err != nil {
		return PullBase{}, err
	}
	var p PullBase
	if err := json.Unmarshal(out, &p); err != nil || p.Base == "" {
		return PullBase{}, fmt.Errorf("gh api repos/%s/pulls/%d: no base branch in its answer", repo, n)
	}
	return p, nil
}

// Of is how the pull request p merges into its base branch, which releases
// as b: the release pull request of a ReleasePR branch cuts its Version.
func (p PullBase) Of(b BaseRelease) BaseRelease {
	b.Version = ""
	if b.ReleasePR {
		b.Version = ReleaseVersion(p.Head, p.Title)
	}
	return b
}

// ReadBaseRelease reads repo#n's base branch (ReadPullBase) and how it
// releases (ReadBranchRelease). An error is GitHub not answering.
func ReadBaseRelease(ctx context.Context, run GH, repo string, n int) (BaseRelease, error) {
	p, err := ReadPullBase(ctx, run, repo, n)
	if err != nil {
		return BaseRelease{}, err
	}
	r, err := ReadBranchRelease(ctx, run, repo, p.Base)
	if err != nil {
		return BaseRelease{}, err
	}
	return p.Of(r), nil
}

// ReadBranchRelease reads how repo's branch base releases: the
// Auto-release workflow on it, one request; on a branch without it the
// PluginReleaseWorkflow, a second; on a branch without either the
// release-PR flow's two workflows, up to two more. A branch without any has
// no auto-release; an error is GitHub not answering. Version is left to the
// pull request (PullBase.Of).
func ReadBranchRelease(ctx context.Context, run GH, repo, base string) (BaseRelease, error) {
	r := BaseRelease{Base: base}
	raw, err := rawFile(ctx, run, repo, AutoReleaseWorkflow, r.Base)
	switch {
	case notFound(err):
	case err != nil:
		return BaseRelease{}, err
	default:
		if r.Auto, err = PushesBranch(raw, r.Base); err != nil {
			return BaseRelease{}, fmt.Errorf("%s on %s: %w", AutoReleaseWorkflow, r.Base, err)
		}
		return r, nil
	}
	raw, err = rawFile(ctx, run, repo, PluginReleaseWorkflow, r.Base)
	switch {
	case notFound(err):
	case err != nil:
		return BaseRelease{}, err
	default:
		if r.Plugins, err = ClosesInto(raw, r.Base); err != nil {
			return BaseRelease{}, fmt.Errorf("%s on %s: %w", PluginReleaseWorkflow, r.Base, err)
		}
		if r.Plugins = r.Plugins && strings.Contains(string(raw), Marketplace); r.Plugins {
			return r, nil
		}
	}
	if r.ReleasePR, err = releasesByPR(ctx, run, repo, r.Base); err != nil {
		return BaseRelease{}, err
	}
	return r, nil
}

// releasesByPR says whether base releases through the release-PR flow: its
// CreateReleaseWorkflow runs on a push to it, and its
// CreateReleasePRWorkflow is there to open the release pull request.
func releasesByPR(ctx context.Context, run GH, repo, base string) (bool, error) {
	raw, err := rawFile(ctx, run, repo, CreateReleaseWorkflow, base)
	switch {
	case notFound(err):
		return false, nil
	case err != nil:
		return false, err
	}
	pushes, err := PushesBranch(raw, base)
	if err != nil {
		return false, fmt.Errorf("%s on %s: %w", CreateReleaseWorkflow, base, err)
	}
	if !pushes {
		return false, nil
	}
	_, err = rawFile(ctx, run, repo, CreateReleasePRWorkflow, base)
	switch {
	case notFound(err):
		return false, nil
	case err != nil:
		return false, err
	}
	return true, nil
}

// rawFile is the file at path on ref, raw; one the repository lacks there
// is GitHub's 404 (notFound).
func rawFile(ctx context.Context, run GH, repo, path, ref string) ([]byte, error) {
	return run(ctx, "api", "-H", "Accept: application/vnd.github.raw",
		fmt.Sprintf("repos/%s/contents/%s?ref=%s", repo, path, url.QueryEscape(ref)))
}

// notFound says whether err is GitHub's 404.
func notFound(err error) bool { return err != nil && strings.Contains(err.Error(), "HTTP 404") }

// ClosesInto says whether a workflow runs when a pull request into branch
// closes: its on.pull_request (or pull_request_target) trigger lists the
// closed type, and its branch filters let branch through.
func ClosesInto(workflow []byte, branch string) (bool, error) {
	var doc struct {
		On any `yaml:"on"`
	}
	if err := yaml.Unmarshal(workflow, &doc); err != nil {
		return false, err
	}
	on, _ := doc.On.(map[string]any)
	for _, event := range []string{"pull_request", "pull_request_target"} {
		f, _ := on[event].(map[string]any)
		types, _ := f["types"].([]any)
		if !slices.Contains(types, any("closed")) {
			continue
		}
		branches, hasBranches := f["branches"].([]any)
		ignore, hasIgnore := f["branches-ignore"].([]any)
		switch {
		case hasBranches:
			return filterMatches(branches, branch)
		case hasIgnore:
			m, err := filterMatches(ignore, branch)
			return !m, err
		}
		return true, nil
	}
	return false, nil
}

// PluginTag is the tag a plugin marketplace's Auto Release cuts for a
// plugin's version.
func PluginTag(plugin, version string) string { return plugin + "/v" + version }

// PluginTags are the tags the PluginReleaseWorkflow cuts for merge commit
// sha: one per plugin whose version in Marketplace differs from its version
// in the commit's first parent (a plugin new in it included), in the
// manifest's order; none when the merge changed no version.
func PluginTags(ctx context.Context, run GH, repo, sha string) ([]string, error) {
	parent, err := run(ctx, "api", fmt.Sprintf("repos/%s/commits/%s", repo, sha), "--jq", ".parents[0].sha")
	if err != nil {
		return nil, err
	}
	now, err := marketplaceAt(ctx, run, repo, sha)
	if err != nil {
		return nil, err
	}
	var was manifest
	if p := strings.TrimSpace(string(parent)); p != "" {
		if was, err = marketplaceAt(ctx, run, repo, p); err != nil {
			return nil, err
		}
	}
	var tags []string
	for _, pl := range now.order {
		if v := now.versions[pl]; v != "" && v != was.versions[pl] {
			tags = append(tags, PluginTag(pl, v))
		}
	}
	return tags, nil
}

// manifest is a Marketplace's plugins in order and their versions.
type manifest struct {
	order    []string
	versions map[string]string
}

// marketplaceAt reads the Marketplace at ref; a ref without one has no
// plugins.
func marketplaceAt(ctx context.Context, run GH, repo, ref string) (manifest, error) {
	raw, err := rawFile(ctx, run, repo, Marketplace, ref)
	switch {
	case notFound(err):
		return manifest{}, nil
	case err != nil:
		return manifest{}, err
	}
	var doc struct {
		Plugins []struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"plugins"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return manifest{}, fmt.Errorf("%s at %s: %w", Marketplace, shortRef(ref), err)
	}
	m := manifest{versions: map[string]string{}}
	for _, p := range doc.Plugins {
		m.order = append(m.order, p.Name)
		m.versions[p.Name] = p.Version
	}
	return m, nil
}

// shortRef is a commit's short form, a branch unchanged.
func shortRef(ref string) string { return ref[:min(len(ref), 9)] }

// TagExists says whether repo has tag.
func TagExists(ctx context.Context, run GH, repo, tag string) (bool, error) {
	_, err := run(ctx, "api", fmt.Sprintf("repos/%s/git/ref/tags/%s", repo, tag))
	switch {
	case notFound(err):
		return false, nil
	case err != nil:
		return false, err
	}
	return true, nil
}

// PushesBranch says whether a workflow runs on a push to branch, by its
// on: trigger and GitHub's branch filters (branches with ! negations, the
// last matching pattern deciding; branches-ignore; a tags filter alone
// leaves branch pushes out).
func PushesBranch(workflow []byte, branch string) (bool, error) {
	var doc struct {
		On any `yaml:"on"`
	}
	if err := yaml.Unmarshal(workflow, &doc); err != nil {
		return false, err
	}
	switch on := doc.On.(type) {
	case string:
		return on == "push", nil
	case []any:
		for _, e := range on {
			if e == "push" {
				return true, nil
			}
		}
		return false, nil
	case map[string]any:
		push, ok := on["push"]
		if !ok {
			return false, nil
		}
		f, _ := push.(map[string]any)
		branches, hasBranches := f["branches"].([]any)
		ignore, hasIgnore := f["branches-ignore"].([]any)
		_, hasTags := f["tags"]
		_, hasTagsIgnore := f["tags-ignore"]
		switch {
		case hasBranches:
			return filterMatches(branches, branch)
		case hasIgnore:
			m, err := filterMatches(ignore, branch)
			return !m, err
		}
		return !hasTags && !hasTagsIgnore, nil
	}
	return false, nil
}

// filterMatches applies a branch filter's patterns in order: a pattern
// includes the branch, a !pattern excludes it again.
func filterMatches(patterns []any, branch string) (bool, error) {
	in := false
	for _, p := range patterns {
		s, ok := p.(string)
		if !ok {
			continue
		}
		neg := strings.HasPrefix(s, "!")
		re, err := filterPattern(strings.TrimPrefix(s, "!"))
		if err != nil {
			return false, err
		}
		if re.MatchString(branch) {
			in = !neg
		}
	}
	return in, nil
}

// filterPattern is a GitHub filter pattern as a regular expression: * any
// characters but /, ** any, ? and + quantify the character before them,
// [...] one character of the set.
func filterPattern(p string) (*regexp.Regexp, error) {
	var b strings.Builder
	b.WriteByte('^')
	for i := 0; i < len(p); i++ {
		switch c := p[i]; c {
		case '*':
			if i+1 < len(p) && p[i+1] == '*' {
				b.WriteString(".*")
				i++
			} else {
				b.WriteString("[^/]*")
			}
		case '?', '+':
			b.WriteByte(c)
		case '[':
			j := strings.IndexByte(p[i:], ']')
			if j < 0 {
				return nil, fmt.Errorf("pattern %q: unclosed [", p)
			}
			b.WriteString(p[i : i+j+1])
			i += j
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	b.WriteByte('$')
	return regexp.Compile(b.String())
}
