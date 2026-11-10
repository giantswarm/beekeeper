package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/Masterminds/semver/v3"

	"github.com/giantswarm/beekeeper/internal/merge"
	"github.com/giantswarm/beekeeper/internal/state"
)

var releaseTarget = regexp.MustCompile(`^([\w.-]+/[\w.-]+)@([^\s@]+)$`)

// parseReleaseTarget retains the tag spelling: v1.2.3 and 1.2.3 can name
// different tags. Only explicit versions are accepted, never ranges.
func parseReleaseTarget(on string) (repo, version string, ok bool) {
	m := releaseTarget.FindStringSubmatch(on)
	if m == nil {
		return "", "", false
	}
	if _, err := semver.StrictNewVersion(strings.TrimPrefix(m[2], "v")); err != nil {
		return "", "", false
	}
	return m[1], m[2], true
}

// Each watch poll makes a bounded availability check using the same artifact
// and CI checks as devctl release wait. A timeout leaves the park pending.
var readParkRelease = func(ctx context.Context, repo, version string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	c := exec.CommandContext(ctx, merge.Tool, "release", "wait", repo, version, "--timeout", "5s") //nolint:gosec // validated repository and version, no shell
	c.Env = devctlCommandEnv("")
	raw, err := c.Output()
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 2 {
		// devctl's normal timeout means the release is still pending.
		return raw, nil
	}
	return raw, err
}

func (w *watcher) settledReleaseParks(ctx context.Context, st *state.State) []settledPark {
	if boardBudgetLow(st.Budget, w.cfg.GitHub.Floor, w.now) {
		return nil
	}
	checked := map[string]bool{}
	available := map[string]bool{}
	var out []settledPark
	for _, ag := range parkedAgents(st) {
		p := ag.Park
		if p == nil || !p.Resumable.IsZero() {
			continue
		}
		repo, version, ok := parseReleaseTarget(p.On)
		if !ok {
			continue
		}
		if !checked[p.On] {
			checked[p.On] = true
			read := func() error {
				raw, err := readParkRelease(ctx, repo, version)
				if err != nil {
					return err
				}
				var doc struct {
					Verdict  string `json:"verdict"`
					ExitCode int    `json:"exitCode"`
				}
				if err := json.Unmarshal(raw, &doc); err != nil {
					return err
				}
				available[p.On] = doc.Verdict == verdictAvailable && doc.ExitCode == 0
				return nil
			}
			var err error
			if w.loopOwned {
				err = w.loopGitHub(ctx, "park-release", read)
			} else {
				err = read()
			}
			key := "park-release-" + p.On
			if err != nil {
				if !errors.Is(err, errLoopLater) {
					w.fail(key, "cannot check release %s: %v", p.On, err)
				}
			} else {
				w.clear(key)
			}
		}
		if available[p.On] {
			out = append(out, settledPark{ag.Party, p.On, fmt.Sprintf("release %s is available", p.On)})
		}
	}
	return out
}
