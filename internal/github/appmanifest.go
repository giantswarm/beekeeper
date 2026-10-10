package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path"
	"slices"
	"strings"
)

// AppManifest is a GitHub App's declaration in the organisation's
// repository: its name and the hosts of its callback URLs, read from the
// manifest on the default branch.
type AppManifest struct {
	// Name is the App's name.
	Name string
	// Callbacks are the hosts (with a port where the URL has one) of the
	// manifest's callback_urls, lower-case, each once.
	Callbacks []string
	// Source is where it was read: <repo>@<commit>:<path>.
	Source string
}

// ReadAppManifest reads the App manifest at path in repo's default branch:
// its name, its callback hosts and the commit it was read at. A manifest
// that is missing, unreadable or names no callback URL is an error: the App
// is not declared.
func ReadAppManifest(ctx context.Context, run GH, repo, path string) (AppManifest, error) {
	sha, err := defaultBranch(ctx, run, repo)
	if err != nil {
		return AppManifest{}, err
	}
	raw, err := readFile(ctx, run, repo, path, sha)
	if err != nil {
		return AppManifest{}, err
	}
	var m struct {
		Name         string   `json:"name"`
		CallbackURLs []string `json:"callback_urls"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return AppManifest{}, fmt.Errorf("%s:%s: %w", repo, path, err)
	}
	a := AppManifest{Name: m.Name, Source: source(repo, sha, path)}
	for _, c := range m.CallbackURLs {
		u, err := url.Parse(c)
		if err != nil || u.Host == "" {
			return AppManifest{}, fmt.Errorf("%s: callback URL %q has no host", a.Source, c)
		}
		if h := strings.ToLower(u.Host); !slices.Contains(a.Callbacks, h) {
			a.Callbacks = append(a.Callbacks, h)
		}
	}
	if a.Name == "" || len(a.Callbacks) == 0 {
		return AppManifest{}, fmt.Errorf("%s declares no App name and callback URL", a.Source)
	}
	return a, nil
}

// InstallationFile is the file next to an App's manifest that declares its
// installation: {"repositories": "all" | ["<name>", …]}.
const InstallationFile = "installation.json"

// AppDeclaration is a GitHub App's declaration as its creation reads it:
// the manifest document GitHub's manifest flow takes and the installation
// it declares.
type AppDeclaration struct {
	// Name is the App's name.
	Name string
	// Manifest is the manifest document as declared, a JSON object.
	Manifest map[string]any
	// Repositories are the installation's repositories by name; nil is all
	// of the organisation's, declared or by the file's absence.
	Repositories []string
	// Source is where the manifest was read, <repo>@<commit>:<path>, and
	// Installation where the installation file was, "" when absent.
	Source, Installation string
}

// ReadAppDeclaration reads the App manifest at path in repo's default
// branch, the document itself, and the installation file next to it at the
// same commit. A manifest that is missing, unreadable or names no App is an
// error: the App is not declared; so is an installation file of another
// shape than {"repositories": "all" | ["<name>", …]}.
func ReadAppDeclaration(ctx context.Context, run GH, repo, file string) (AppDeclaration, error) {
	sha, err := defaultBranch(ctx, run, repo)
	if err != nil {
		return AppDeclaration{}, err
	}
	raw, err := readFile(ctx, run, repo, file, sha)
	if err != nil {
		return AppDeclaration{}, err
	}
	d := AppDeclaration{Source: source(repo, sha, file)}
	if err := json.Unmarshal(raw, &d.Manifest); err != nil || d.Manifest == nil {
		return AppDeclaration{}, fmt.Errorf("%s: the manifest is no JSON object", d.Source)
	}
	if d.Name, _ = d.Manifest["name"].(string); d.Name == "" {
		return AppDeclaration{}, fmt.Errorf("%s declares no App name", d.Source)
	}
	inst := path.Join(path.Dir(file), InstallationFile)
	raw, err = readFile(ctx, run, repo, inst, sha)
	if err != nil {
		var absent *absentError
		if errors.As(err, &absent) {
			return d, nil
		}
		return AppDeclaration{}, err
	}
	d.Installation = source(repo, sha, inst)
	if d.Repositories, err = parseInstallation(raw); err != nil {
		return AppDeclaration{}, fmt.Errorf("%s: %w", d.Installation, err)
	}
	return d, nil
}

// parseInstallation reads an installation file: the repositories by name,
// nil for "all".
func parseInstallation(raw []byte) ([]string, error) {
	var f struct {
		Repositories json.RawMessage `json:"repositories"`
	}
	if err := json.Unmarshal(raw, &f); err != nil || len(f.Repositories) == 0 {
		return nil, errors.New(`the installation file is {"repositories": "all" | ["<name>", …]}`)
	}
	var all string
	if json.Unmarshal(f.Repositories, &all) == nil {
		if all != "all" {
			return nil, fmt.Errorf(`"repositories" is "all" or a list of names, not %q`, all)
		}
		return nil, nil
	}
	var names []string
	if err := json.Unmarshal(f.Repositories, &names); err != nil {
		return nil, errors.New(`"repositories" is "all" or a list of names`)
	}
	for _, n := range names {
		if n == "" || strings.ContainsAny(n, "/ \t\n") {
			return nil, fmt.Errorf(`"repositories" names a repository by its name alone, not %q`, n)
		}
	}
	if len(names) == 0 {
		return nil, errors.New(`"repositories" lists no repository: "all", or the names`)
	}
	return slices.Compact(slices.Sorted(slices.Values(names))), nil
}

// absentError is a file the repository lacks at the commit: it declares no
// App there.
type absentError struct{ repo, file string }

func (e *absentError) Error() string { return e.repo + " declares no App at " + e.file }

// defaultBranch is the commit the default branch of repo is at.
func defaultBranch(ctx context.Context, run GH, repo string) (string, error) {
	out, err := run(ctx, "api", "repos/"+repo+"/commits/HEAD", "--jq", ".sha")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// readFile reads the file at file in repo at the commit sha; one the
// repository lacks there is errAbsent, wrapped in what the path declares.
func readFile(ctx context.Context, run GH, repo, file, sha string) ([]byte, error) {
	raw, err := run(ctx, "api", "-H", "Accept: application/vnd.github.raw", fmt.Sprintf("repos/%s/contents/%s?ref=%s", repo, file, sha))
	if err != nil {
		if strings.Contains(err.Error(), "HTTP 404") {
			return nil, &absentError{repo: repo, file: file}
		}
		return nil, err
	}
	return raw, nil
}

// source is where a file was read: <repo>@<commit>:<path>.
func source(repo, sha, file string) string { return fmt.Sprintf("%s@%.12s:%s", repo, sha, file) }
