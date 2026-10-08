package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// AutoReleaseWorkflow is the Auto-release workflow devctl generates: on a
// push to a branch its on.push filter names, it tags the merge commit.
const AutoReleaseWorkflow = ".github/workflows/zz_generated.auto_release.yaml"

// BaseRelease is how a pull request's base branch releases: Auto when the
// Auto-release workflow on the branch runs on a push to it, so a merge into
// it is tagged; otherwise its tags are cut by hand (a maintenance branch, a
// fork line's backport branch).
type BaseRelease struct {
	Base string
	Auto bool
}

// ReadBaseRelease reads repo#n's base branch and the Auto-release workflow
// on it, two requests. A branch without the workflow has no auto-release; an
// error is GitHub not answering either.
func ReadBaseRelease(ctx context.Context, run GH, repo string, n int) (BaseRelease, error) {
	out, err := run(ctx, "pr", "view", strconv.Itoa(n), "--repo", repo, "--json", "baseRefName")
	if err != nil {
		return BaseRelease{}, err
	}
	var p struct {
		BaseRefName string `json:"baseRefName"`
	}
	if err := json.Unmarshal(out, &p); err != nil || p.BaseRefName == "" {
		return BaseRelease{}, fmt.Errorf("gh pr view %d --repo %s: no base branch in its answer", n, repo)
	}
	r := BaseRelease{Base: p.BaseRefName}
	raw, err := run(ctx, "api", "-H", "Accept: application/vnd.github.raw",
		fmt.Sprintf("repos/%s/contents/%s?ref=%s", repo, AutoReleaseWorkflow, url.QueryEscape(r.Base)))
	switch {
	case err != nil && strings.Contains(err.Error(), "HTTP 404"):
		return r, nil
	case err != nil:
		return BaseRelease{}, err
	}
	if r.Auto, err = PushesBranch(raw, r.Base); err != nil {
		return BaseRelease{}, fmt.Errorf("%s on %s: %w", AutoReleaseWorkflow, r.Base, err)
	}
	return r, nil
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
