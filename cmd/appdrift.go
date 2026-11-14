package cmd

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/guard"
	"github.com/giantswarm/beekeeper/internal/state"
)

// appDrifts reports only verified differences; an unavailable declaration is
// not evidence of drift. Records and grants are never changed.
func (a *app) appDrifts(ctx context.Context, records []state.App) map[string]string {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out := map[string]string{}
	for _, r := range records {
		if r.ClientID == "" || r.Google || r.GitLab != "" || r.OAuth != "" || r.Slack != "" {
			continue
		}
		file := a.cfg.Apps.ManifestOf(r.Name)
		if file == "" {
			continue
		}
		current, err := github.ReadAppManifest(ctx, appsGH, a.cfg.Apps.Repo, file)
		if err != nil || !strings.EqualFold(current.Name, r.Name) || current.Source == r.Declared {
			continue
		}
		pinned, err := github.ReadPinnedAppManifest(ctx, appsGH, r.Declared)
		if err != nil {
			continue
		}
		oldURLs, newURLs := slices.Clone(pinned.CallbackURLs), slices.Clone(current.CallbackURLs)
		slices.Sort(oldURLs)
		slices.Sort(newURLs)
		if slices.Equal(slices.Compact(oldURLs), slices.Compact(newURLs)) {
			continue
		}
		out[r.Declared] = fmt.Sprintf("App %s consent manifest drift: recorded %s; declared %s; the person can re-record consent with beekeeper app allow", r.Name, r.Declared, current.Source)
	}
	return out
}

func (a *app) consentApps(ctx context.Context, st *state.State, running func(state.AppRun) bool) []guard.App {
	apps := guardAppsWithRuns(st, running)
	drift := a.appDrifts(ctx, st.Apps)
	for i := range apps {
		apps[i].Drift = drift[apps[i].Declared]
	}
	return apps
}

func driftLines(drift map[string]string) []string {
	var lines []string
	for _, line := range drift {
		lines = append(lines, line)
	}
	slices.Sort(lines)
	return lines
}
