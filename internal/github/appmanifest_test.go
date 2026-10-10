package github

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// The declaring repository of the tests, the commit its default branch is
// at, and the App it declares.
const (
	declaringRepo   = "example/github"
	declaringCommit = "0123456789abcdef0123"
	declaredApp     = "example-lab-skills"
	manifestPath    = "apps/" + declaredApp + "/manifest.json"
	installPath     = "apps/" + declaredApp + "/" + InstallationFile
	declaredSource  = declaringRepo + "@0123456789ab:" + manifestPath
	skillsManifest  = `{"name": "` + declaredApp + `", "url": "https://example.org", "public": false, "default_permissions": {"contents": "read", "metadata": "read"}}`
)

// declaring is a gh that answers the default branch's commit and the files
// of files there, any other path 404.
func declaring(files map[string]string) GH {
	return func(_ context.Context, args ...string) ([]byte, error) {
		call := strings.Join(args, " ")
		if strings.Contains(call, "repos/"+declaringRepo+"/commits/HEAD") {
			return []byte(declaringCommit + "\n"), nil
		}
		for path, body := range files {
			if strings.Contains(call, "repos/"+declaringRepo+"/contents/"+path+"?ref="+declaringCommit) {
				return []byte(body), nil
			}
		}
		return nil, errors.New("gh: Not Found (HTTP 404)")
	}
}

// A declaration is the manifest document and the installation file next to
// it at the same commit: a list of names sorted and each once, "all" or an
// absent file as all repositories; a manifest without a name, one that is
// no object, and an installation file of another shape are refused, and so
// is a manifest the repository lacks, naming the path.
func TestReadAppDeclaration(t *testing.T) {
	ctx := context.Background()
	for name, tc := range map[string]struct {
		files   map[string]string
		repos   []string
		install string
		want    string
	}{
		"a list":      {map[string]string{manifestPath: skillsManifest, installPath: `{"repositories": ["workspace-manager", "agentlab", "agentlab"]}`}, []string{"agentlab", "workspace-manager"}, declaringRepo + "@0123456789ab:" + installPath, ""},
		"all":         {map[string]string{manifestPath: skillsManifest, installPath: `{"repositories": "all"}`}, nil, declaringRepo + "@0123456789ab:" + installPath, ""},
		"absent":      {map[string]string{manifestPath: skillsManifest}, nil, "", ""},
		"no object":   {map[string]string{manifestPath: `[]`}, nil, "", "the manifest is no JSON object"},
		"no name":     {map[string]string{manifestPath: `{"url": "https://example.org"}`}, nil, "", "declares no App name"},
		"missing":     {map[string]string{}, nil, "", declaringRepo + " declares no App at " + manifestPath},
		"other shape": {map[string]string{manifestPath: skillsManifest, installPath: `{"repos": ["a"]}`}, nil, "", `the installation file is {"repositories": "all" | ["<name>", …]}`},
		"not all":     {map[string]string{manifestPath: skillsManifest, installPath: `{"repositories": "some"}`}, nil, "", `"repositories" is "all" or a list of names, not "some"`},
		"empty list":  {map[string]string{manifestPath: skillsManifest, installPath: `{"repositories": []}`}, nil, "", `lists no repository`},
		"a full name": {map[string]string{manifestPath: skillsManifest, installPath: `{"repositories": ["example/agentlab"]}`}, nil, "", `by its name alone, not "example/agentlab"`},
	} {
		d, err := ReadAppDeclaration(ctx, declaring(tc.files), declaringRepo, manifestPath)
		if tc.want != "" {
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("%s: err = %v, want %q", name, err, tc.want)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if d.Name != declaredApp || d.Source != declaredSource || d.Installation != tc.install || strings.Join(d.Repositories, " ") != strings.Join(tc.repos, " ") ||
			(d.Repositories == nil) != (tc.repos == nil) || d.Manifest["url"] != "https://example.org" {
			t.Errorf("%s: declaration = %+v", name, d)
		}
	}
}

// The consent record's reader still answers the name, the callback hosts
// and the source, and refuses a manifest without callbacks or one the
// repository lacks.
func TestReadAppManifest(t *testing.T) {
	ctx := context.Background()
	files := map[string]string{manifestPath: `{"name": "` + declaredApp + `", "callback_urls": ["https://Lab.127.0.0.1.nip.io:8443/cb", "https://lab.127.0.0.1.nip.io:8443/other"]}`,
		"apps/skills/manifest.json": skillsManifest}
	m, err := ReadAppManifest(ctx, declaring(files), declaringRepo, manifestPath)
	if err != nil || m.Name != declaredApp || strings.Join(m.Callbacks, " ") != "lab.127.0.0.1.nip.io:8443" || m.Source != declaredSource {
		t.Errorf("ReadAppManifest = %+v, %v", m, err)
	}
	if _, err := ReadAppManifest(ctx, declaring(files), declaringRepo, "apps/skills/manifest.json"); err == nil || !strings.Contains(err.Error(), "declares no App name and callback URL") {
		t.Errorf("a manifest without callbacks: %v", err)
	}
	if _, err := ReadAppManifest(ctx, declaring(files), declaringRepo, "apps/none/manifest.json"); err == nil || err.Error() != declaringRepo+" declares no App at apps/none/manifest.json" {
		t.Errorf("a missing manifest: %v", err)
	}
}
