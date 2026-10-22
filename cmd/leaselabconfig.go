package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/giantswarm/beekeeper/internal/lease"
	"github.com/giantswarm/beekeeper/internal/state"
)

// The states of a held lab's configuration (lease status).
const (
	labConfigClaimed  = "claim-time" // agentlab.yaml is as the claim found it
	labConfigChanged  = "changed"    // it differs in labConfigView.Keys
	labConfigUnknown  = "unknown"    // the claim recorded none, or it cannot be read
	labConfigFileName = "agentlab.yaml"
)

// labConfigView is what lease status says of a lab's configuration.
type labConfigView struct {
	State string   `json:"state"`
	Keys  []string `json:"keys,omitempty"`
	Why   string   `json:"why,omitempty"`
}

// listLabDirs maps the labs agentlab knows (their clusterName) to their
// directories, from agentlab list.
func (a *app) listLabDirs() (map[string]string, error) {
	if a.labDirs != nil {
		return a.labDirs()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	raw, err := exec.CommandContext(ctx, agentlabCmd, "list", "-o", "json").Output()
	if err != nil {
		return nil, fmt.Errorf("agentlab list: %w", err)
	}
	var ls []struct {
		Name string `json:"name"`
		Dir  string `json:"dir"`
	}
	if err := json.Unmarshal(raw, &ls); err != nil {
		return nil, fmt.Errorf("agentlab list: %w", err)
	}
	dirs := map[string]string{}
	for _, l := range ls {
		dirs[l.Name] = l.Dir
	}
	return dirs, nil
}

// readLabConfig reads the agentlab.yaml of the lab res stands for.
func (a *app) readLabConfig(res string) (*lease.LabConfig, []byte, error) {
	cl := a.cfg.LabCluster(res)
	dirs, err := a.listLabDirs()
	if err != nil {
		return nil, nil, err
	}
	dir, ok := dirs[cl]
	if !ok {
		return nil, nil, fmt.Errorf("agentlab lists no lab %s", cl)
	}
	c, raw, err := lease.ReadLabConfig(filepath.Join(dir, labConfigFileName))
	if err != nil {
		return nil, nil, err
	}
	return &c, raw, nil
}

// labDrift names the keys in which the lab's agentlab.yaml differs from the
// one the claim of cur kept.
func labDrift(dir lease.Dir, cur lease.Holder) ([]string, error) {
	was, err := dir.ClaimedLabConfig(cur.Env)
	if err != nil {
		return nil, err
	}
	_, now, err := lease.ReadLabConfig(cur.LabConfig.Path)
	if err != nil {
		return nil, err
	}
	return lease.LabDiff(was, now)
}

// labConfigOf is the state of the configuration of the lab cur holds.
func labConfigOf(dir lease.Dir, cur lease.Holder) labConfigView {
	if cur.LabConfig == nil {
		return labConfigView{State: labConfigUnknown, Why: "its claim recorded none"}
	}
	keys, err := labDrift(dir, cur)
	switch {
	case err != nil:
		return labConfigView{State: labConfigUnknown, Why: err.Error()}
	case len(keys) > 0:
		return labConfigView{State: labConfigChanged, Keys: keys}
	}
	return labConfigView{State: labConfigClaimed}
}

// Text is the status line of the configuration.
func (v labConfigView) Text() string {
	switch v.State {
	case labConfigClaimed:
		return "agentlab.yaml is at its claim-time state"
	case labConfigChanged:
		return "agentlab.yaml changed since the claim in " + strings.Join(v.Keys, ", ")
	}
	return "agentlab.yaml unknown: " + v.Why
}

// labRelease settles the configuration of the lab cur holds before me
// releases it: unchanged, restored (restore), declared (keep) or, released
// with force, left for the next claim as not declared. It refuses a change
// neither restored nor declared. It returns what the release line adds.
func (a *app) labRelease(dir lease.Dir, cur lease.Holder, me state.Party, restore bool, keep string, force bool) (string, error) {
	if cur.LabConfig == nil {
		return "", nil
	}
	keys, err := labDrift(dir, cur)
	if err != nil {
		return fmt.Sprintf("; agentlab.yaml not compared with its claim-time state (%v)", err), nil
	}
	if len(keys) == 0 {
		return "; agentlab.yaml is at its claim-time state", nil
	}
	changed := strings.Join(keys, ", ")
	switch {
	case restore:
		aside, err := dir.RestoreLabConfig(cur.Env, cur.LabConfig.Path)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("; restored agentlab.yaml to its claim-time state (%s; the released one is %s): the cluster runs what it was installed with until `agentlab platform --lab %s`",
			changed, aside, a.cfg.LabCluster(cur.Env)), nil
	case keep != "" || force:
		c := lease.LabChange{By: me.Name, At: a.now.UTC().Format(time.RFC3339), Keys: keys, Why: keep}
		if c.By == "" {
			c.By = cur.Holder
		}
		if err := dir.LeaveLabChange(cur.Env, c); err != nil {
			return "", err
		}
		if keep == "" {
			return fmt.Sprintf("; agentlab.yaml left changed in %s, not declared: the next claim is told", changed), nil
		}
		return fmt.Sprintf("; agentlab.yaml left changed in %s, declared: the next claim is told", changed), nil
	}
	return "", refused("%s not released: its agentlab.yaml (%s) differs from its claim-time state in %s; put it back with `beekeeper lease release %s --restore`, or declare the change for the next holder with `--keep-config \"<why>\"`",
		cur.Env, cur.LabConfig.Path, changed, cur.Env)
}
