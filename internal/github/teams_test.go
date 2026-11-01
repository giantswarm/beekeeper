package github

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"maps"
	"strings"
	"testing"
)

// contents is the contents API's answer for a file holding text: base64
// wrapped at 60 characters, as GitHub sends it.
func contents(text string) string {
	b64 := base64.StdEncoding.EncodeToString([]byte(text))
	var lines []string
	for len(b64) > 60 {
		lines = append(lines, b64[:60])
		b64 = b64[60:]
	}
	out, _ := json.Marshal(map[string]string{"encoding": "base64", "content": strings.Join(append(lines, b64), "\n")})
	return string(out)
}

func TestTeamsReadsEveryTeamFile(t *testing.T) {
	files := map[string]string{
		"repos/org/github/contents/repositories": `[
			{"name": "team-a.yaml", "path": "repositories/team-a.yaml", "type": "file"},
			{"name": "team-b.yaml", "path": "repositories/team-b.yaml", "type": "file"},
			{"name": "override", "path": "repositories/override", "type": "dir"},
			{"name": "README.md", "path": "repositories/README.md", "type": "file"}]`,
		"repos/org/github/contents/repositories/team-a.yaml": contents("# yaml-language-server: $schema=x\n- name: Beekeeper\n  gen: {flavours: [cli]}\n- name: muster\n"),
		"repos/org/github/contents/repositories/team-b.yaml": contents("- name: shared-configs\n"),
	}
	var acting []string
	gh := func(ctx context.Context, args ...string) ([]byte, error) {
		acting = append(acting, Repo(ctx))
		out, ok := files[args[1]]
		if !ok {
			t.Fatalf("read %v", args)
		}
		return []byte(out), nil
	}
	got, err := Teams(context.Background(), gh, "org/github", "repositories")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"org/beekeeper": "team-a", "org/muster": "team-a", "org/shared-configs": "team-b"}
	if !maps.Equal(got, want) {
		t.Errorf("teams = %v, want %v", got, want)
	}
	for _, r := range acting {
		if r != "org/github" {
			t.Errorf("a read acted on %q", r)
		}
	}
}

func TestTeamsFailsOnAnyUnreadableFile(t *testing.T) {
	gh := func(_ context.Context, args ...string) ([]byte, error) {
		if strings.HasSuffix(args[1], "team-b.yaml") {
			return nil, errors.New("HTTP 502")
		}
		if strings.HasSuffix(args[1], "team-a.yaml") {
			return []byte(contents("- name: beekeeper\n")), nil
		}
		return []byte(`[{"name": "team-a.yaml", "path": "r/team-a.yaml", "type": "file"}, {"name": "team-b.yaml", "path": "r/team-b.yaml", "type": "file"}]`), nil
	}
	if got, err := Teams(context.Background(), gh, "org/github", "r"); err == nil {
		t.Errorf("a partial read answered %v", got)
	}
}
