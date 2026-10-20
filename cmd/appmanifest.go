package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/guard"
)

// appCreateSecret is app create --manifest: the App a local manifest
// declares, created through this run's loopback listener, its code
// converted in this process and its credentials written into the SOPS
// Secret apps.secrets names for installation. owner is the organisation
// (apps.repo's owner when its login is empty) or the personal account the
// App belongs to.
func (a *app) appCreateSecret(cmd *cobra.Command, installation, file string, owner appOwner, wait time.Duration, replace, open bool) error {
	if inSandbox() {
		return refused("app create runs on the host: the agent sandbox reaches neither the person's browser nor the loopback listener")
	}
	target, err := a.appSecret(installation)
	if err != nil {
		return err
	}
	manifest, name, err := readManifest(file)
	if err != nil {
		return usageErr("--manifest: %v", err)
	}
	if owner.login == "" && !owner.user {
		owner.login, _, _ = strings.Cut(a.cfg.Apps.Repo, "/")
	}
	if owner.login == "" {
		return usageErr("--org or --user names the account that owns the App: apps.repo names none")
	}
	instFile, repos, err := localInstallation(file)
	if err != nil {
		return usageErr("%v", err)
	}
	ctx := cmd.Context()
	if known, err := a.appKnown(ctx, guard.AppSlug(name)); err != nil {
		return err
	} else if known != nil {
		return knownRefusal(name, known)
	}
	ownerID, repoIDs, err := a.installTarget(ctx, owner, instFile, repos)
	if err != nil {
		return err
	}
	if open {
		if err := a.holdsBrowser(); err != nil {
			return err
		}
	}
	say := cmd.ErrOrStderr()
	var flow *manifestFlow
	defer func() {
		if flow != nil {
			flow.stop()
		}
	}()
	var created *githubApp
	unrecord := func() {}
	defer func() { unrecord() }()
	err = a.writeGitHubApp(cmd, installation, target, replace, func(ctx context.Context) (*githubApp, error) {
		if flow, err = serveManifest(owner, name, file, manifest); err != nil {
			return nil, err
		}
		if unrecord, err = a.recordRun(name, owner.login, file, flow); err != nil {
			return nil, err
		}
		if _, err := fmt.Fprintf(say, "App %s, declared in %s, its credentials for %s (%s)\n", name, file, installation, target.File); err != nil {
			return nil, err
		}
		code, err := flow.awaitCode(ctx, say, open, wait)
		if err != nil {
			return nil, err
		}
		if _, err := fmt.Fprintln(say, "GitHub redirected with the App's code: converting it and writing the Secret"); err != nil {
			return nil, err
		}
		created, err = convertManifest(ctx, code)
		return created, err
	})
	if created != nil {
		stored := target.File
		if err != nil {
			stored = ""
		}
		a.recordCreated(say, name, owner.login, file, stored, created)
	}
	if flow != nil {
		// the callback holds the browser until the Secret is written
		out := flowOutcome{err: err}
		if err == nil && created != nil {
			out.installURL = installURL(created.Slug, ownerID, repoIDs)
		}
		flow.installs <- out
	}
	return err
}

// localInstallation reads the installation file next to the local
// manifest: its path and the repositories it names, nil for all; no file
// is all of them.
func localInstallation(manifest string) (string, []string, error) {
	file := filepath.Join(filepath.Dir(manifest), github.InstallationFile)
	raw, err := os.ReadFile(file) //nolint:gosec // the installation file beside the manifest the person names
	if errors.Is(err, os.ErrNotExist) {
		return file, nil, nil
	}
	if err != nil {
		return "", nil, err
	}
	repos, err := github.ParseInstallation(raw)
	if err != nil {
		return "", nil, fmt.Errorf("%s: %v", file, err)
	}
	return file, repos, nil
}

// readManifest reads a local App manifest: a JSON object with the App's
// name. Its redirect_url is this run's.
func readManifest(file string) (map[string]any, string, error) {
	raw, err := os.ReadFile(file) //nolint:gosec // the manifest the person names
	if err != nil {
		return nil, "", err
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, "", fmt.Errorf("%s is no JSON object: %v", file, err)
	}
	name, _ := m["name"].(string)
	if name = strings.TrimSpace(name); name == "" {
		return nil, "", fmt.Errorf("%s names no App (\"name\")", file)
	}
	return m, name, nil
}
