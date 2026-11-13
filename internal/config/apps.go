package config

import (
	"strings"
)

// AppName is the placeholder of the App's name in Apps.Manifest, and of
// the purpose in Apps.Template.
const AppName = "{name}"

// The placeholders a manifest template's strings may carry: the
// installation's name and its base domain.
const (
	AppInstallation = "{installation}"
	AppBaseDomain   = "{baseDomain}"
)

// Apps is the repository that declares the organisation's GitHub Apps, one
// manifest each (GitHub's App manifest: name, callback_urls). Unset, `app
// allow` refuses: an App is recorded only once it is declared.
type Apps struct {
	// Repo is the declaring repository (owner/repo), read on its default
	// branch.
	Repo string `yaml:"repo"`
	// Manifest is an App's manifest's path in Repo, AppName standing for
	// the App's name.
	Manifest string `yaml:"manifest"`
	// Template is the path in Repo of the manifest template of a purpose
	// whose Apps exist once per installation, AppName standing for the
	// purpose; unset, Manifest's file with .template.json for .json.
	Template string `yaml:"template"`
	// Secrets are, by installation, the SOPS file of the Kubernetes Secret
	// an App's credentials go into (beekeeper secret github-app).
	Secrets map[string]AppSecret `yaml:"secrets"`
}

// AppSecret is the SOPS file of an installation's App Secret and, for a
// file absent so far, the Secret it starts as.
type AppSecret struct {
	// File is the SOPS file, absolute (~/ allowed), under the creation
	// rules of the .sops.yaml nearest above it.
	File      string `yaml:"file"`
	Name      string `yaml:"name"`
	Namespace string `yaml:"namespace"`
}

// ManifestOf is the path of the manifest of the App name in Repo, "" while
// the declaring repository is unset.
func (a Apps) ManifestOf(name string) string {
	if a.Repo == "" || !strings.Contains(a.Manifest, AppName) {
		return ""
	}
	return strings.ReplaceAll(a.Manifest, AppName, name)
}

// TemplateOf is the path of the manifest template of purpose in Repo, ""
// while the declaring repository or the path is unset.
func (a Apps) TemplateOf(purpose string) string {
	tmpl := a.Template
	if tmpl == "" {
		base, ok := strings.CutSuffix(a.Manifest, ".json")
		if !ok {
			return ""
		}
		tmpl = base + ".template.json"
	}
	if a.Repo == "" || !strings.Contains(tmpl, AppName) {
		return ""
	}
	return strings.ReplaceAll(tmpl, AppName, purpose)
}
