package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/giantswarm/beekeeper/internal/claude"
)

// securityReviewEnv names the model of the review the security-guidance
// plugin runs, as an Agent SDK session (claude.SDKEntrypoint), on each Stop,
// commit and push of a Claude Code session.
const securityReviewEnv = "SECURITY_REVIEW_MODEL"

// withReviewModel gives a headless Claude turn on model its security review
// on the same model, so a worker's --model (or its task kind's) sets what its
// reviews cost. A settings env outranks the environment the CLI inherits and
// flag settings outrank the person's, so the model travels in the turn's
// --settings: merged into a copy of the profile's settings file when flags
// name one (Claude Code reads a single --settings), else as inline JSON. No
// model leaves flags as they are and the person's setting applies.
func withReviewModel(model string, flags []string) ([]string, error) {
	model = strings.TrimSpace(model)
	if model == "" {
		return flags, nil
	}
	i := slices.Index(flags, settingsFlag)
	if i < 0 || i+1 >= len(flags) {
		raw, err := json.Marshal(map[string]any{"env": map[string]string{securityReviewEnv: model}})
		if err != nil {
			return nil, err
		}
		return append(slices.Clone(flags), settingsFlag, string(raw)), nil
	}
	path := flags[i+1]
	raw, err := os.ReadFile(filepath.Clean(path)) //nolint:gosec // the settings file profileFlags wrote in beekeeper's state folder
	if err != nil {
		return nil, fmt.Errorf("profile settings: %w", err)
	}
	s := map[string]any{}
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("profile settings %s: %w", path, err)
	}
	env, _ := s["env"].(map[string]any)
	if env == nil {
		env = map[string]any{}
	}
	env[securityReviewEnv] = model
	s["env"] = env
	if raw, err = json.MarshalIndent(s, "", "  "); err != nil {
		return nil, err
	}
	// one file per profile and model, rewritten whole: workers of one
	// profile on different models never share it
	out := strings.TrimSuffix(path, ".json") + "." + unsafeKey.ReplaceAllString(model, "_") + ".json"
	// Each writer owns its temporary file, so a concurrent start or wake
	// cannot truncate the published file or rename another writer's file.
	tmp, err := os.CreateTemp(filepath.Dir(out), filepath.Base(out)+".*.tmp")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.Remove(tmp.Name()) }() //nolint:gosec // private temporary file returned by CreateTemp
	if _, err := tmp.Write(append(raw, '\n')); err != nil {
		_ = tmp.Close()
		return nil, err
	}
	if err := tmp.Close(); err != nil {
		return nil, err
	}
	if err := os.Rename(tmp.Name(), out); err != nil { //nolint:gosec // beside the profile's own file in beekeeper's state folder
		return nil, err
	}
	flags = slices.Clone(flags)
	flags[i+1] = out
	return flags, nil
}

// attachSDKChildren gives each local Claude worker of views the sdk-py
// sessions run in its transcript's folder since it registered: the security
// reviews its hooks started there. Workers sharing a folder share none: a
// child goes to the worker registered last before its first request.
func (a *app) attachSDKChildren(views []agentView) {
	byDir := map[string][]int{}
	for i, v := range views {
		if v.Execution != nil || (v.Harness != "" && v.Harness != claudeComm) {
			continue
		}
		if p := claude.Transcript(a.cfg, v.Session, ""); p != "" {
			byDir[filepath.Dir(p)] = append(byDir[filepath.Dir(p)], i)
		}
	}
	for dir, idx := range byDir {
		since := views[idx[0]].Registered
		for _, i := range idx[1:] {
			since = minRegistered(since, views[i].Registered)
		}
		for _, c := range claude.SDKChildren(dir, since, a.cfg.Metrics) {
			var owner *agentView
			for _, i := range idx {
				v := &views[i]
				if !v.Registered.After(c.Started) && (owner == nil || v.Registered.After(owner.Registered)) {
					owner = v
				}
			}
			if owner != nil {
				owner.SDKChildren = append(owner.SDKChildren, c)
			}
		}
	}
}

func minRegistered(a, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}
	return a
}

// sdkChildrenCell is a worker's sdk-py children in a roster row: how many,
// what they cost and on which models; "-" for none.
func sdkChildrenCell(cs []claude.SDKChild) string {
	if len(cs) == 0 {
		return "-"
	}
	var usd float64
	short := ""
	var models []string
	for _, c := range cs {
		usd += c.USD
		if c.Unpriced {
			short = "+"
		}
		for _, m := range c.Models {
			if !slices.Contains(models, m) {
				models = append(models, m)
			}
		}
	}
	slices.Sort(models)
	return fmt.Sprintf("%d $%.2f%s %s", len(cs), usd, short, strings.Join(models, ","))
}
