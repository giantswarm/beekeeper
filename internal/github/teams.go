package github

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// Teams reads the team files in dir of repo (owner/repo), where an
// organisation declares which team owns which of its repositories: every
// <team>.yaml directly in dir lists that team's repositories of repo's
// owner as `- name: <repo>` entries. It answers owner/name, lower-cased, to
// its team. A file it cannot read or parse fails the whole read: a partial
// answer would make a team's repositories nobody's.
func Teams(ctx context.Context, gh GH, repo, dir string) (map[string]string, error) {
	ctx = ActingOn(ctx, repo)
	out, err := gh(ctx, "api", fmt.Sprintf("repos/%s/contents/%s", repo, dir))
	if err != nil {
		return nil, err
	}
	var entries []struct {
		Name string `json:"name"`
		Path string `json:"path"`
		Type string `json:"type"`
	}
	if err := json.Unmarshal(out, &entries); err != nil {
		return nil, fmt.Errorf("the listing of %s/%s: %w", repo, dir, err)
	}
	owner, _, _ := strings.Cut(repo, "/")
	teams := map[string]string{}
	for _, e := range entries {
		team, ok := strings.CutSuffix(e.Name, ".yaml")
		if e.Type != "file" || !ok {
			continue
		}
		// The JSON answer, whose content is base64: RunGH asks a read again
		// whose body is no complete JSON, which a raw YAML body never is.
		out, err := gh(ctx, "api", fmt.Sprintf("repos/%s/contents/%s", repo, e.Path))
		if err != nil {
			return nil, err
		}
		var file struct {
			Content  string `json:"content"`
			Encoding string `json:"encoding"`
		}
		if err := json.Unmarshal(out, &file); err != nil || file.Encoding != "base64" {
			return nil, fmt.Errorf("the team file %s of %s: no base64 content (%v)", e.Path, repo, err)
		}
		raw, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(file.Content, "\n", ""))
		if err != nil {
			return nil, fmt.Errorf("the team file %s of %s: %w", e.Path, repo, err)
		}
		var declared []struct {
			Name string `yaml:"name"`
		}
		if err := yaml.Unmarshal(raw, &declared); err != nil {
			return nil, fmt.Errorf("the team file %s of %s: %w", e.Path, repo, err)
		}
		for _, d := range declared {
			if d.Name != "" {
				teams[strings.ToLower(owner+"/"+d.Name)] = team
			}
		}
	}
	if len(teams) == 0 {
		return nil, fmt.Errorf("no team file in %s/%s declares a repository", repo, dir)
	}
	return teams, nil
}
