package guard

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// labConfigRefusal protects the directory recorded when a lab was claimed.
// Like the other shell guards this is best effort, not a filesystem lock:
// arbitrary programs and unresolved shell expressions can write other paths.
func (h Hook) labConfigRefusal(ev event) string {
	if h.Labs == nil {
		return ""
	}
	cwd := ev.CWD
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	var targets []string
	switch ev.ToolName {
	case writeTool, editTool, multiEditTool, notebookTool:
		p, _ := ev.ToolInput[filePathKey].(string)
		if p == "" {
			p, _ = ev.ToolInput[notebookPathKey].(string)
		}
		if p != "" {
			targets = append(targets, absIn(cwd, p))
		}
	case bashTool:
		cmd, _ := ev.ToolInput[commandKey].(string)
		targets = walkConfigTargets(cmd, cwd, 0, false, true)
	}
	if len(targets) == 0 {
		return ""
	}
	for _, l := range h.Labs() {
		if l.LabConfig == nil || l.LabConfig.Path == "" {
			continue
		}
		if heldBy(l, ev.Session) {
			continue
		}
		dir := canonicalPath(filepath.Dir(l.LabConfig.Path))
		for _, target := range targets {
			matches := false
			if name, ok := strings.CutPrefix(target, labSelector); ok {
				cluster, found := LabCluster(dir)
				matches = found && cluster == name
			} else {
				p := canonicalPath(target)
				matches = p == dir || strings.HasPrefix(p, dir+string(filepath.Separator))
			}
			if matches {
				who := l.Name
				if who == "" {
					who = l.Holder
				}
				return fmt.Sprintf("Refused: lab %s configuration is leased by %s; only the holder may write its configuration directory. Ask the holder to make the change or release the lease.", l.Env, who)
			}
		}
	}
	return ""
}

const labSelector = "agentlab-lab:"

// configWrites adds common direct file mutations and agentlab configuration
// commands to the redirections and tee targets the shared walker collects.
func (w *targetWalk) configWrites(st *deleteState, dir, name string, raw, words []string) {
	switch name {
	case "agentlab":
		configure := false
		selector := ""
		for i, word := range words {
			if word == litHelp || word == "-h" {
				return
			}
			if word == litConfig && i+1 < len(words) && (words[i+1] == "show" || words[i+1] == "get" || words[i+1] == "list") {
				return
			}
			if word == "configure" || word == "config" {
				configure = true
			}
			if strings.HasPrefix(word, "--lab=") {
				selector = strings.TrimPrefix(word, "--lab=")
			}
			if word == "--lab" && i+1 < len(words) {
				selector = words[i+1]
			}
		}
		if !configure {
			return
		}
		if selector != "" {
			w.out = append(w.out, labSelector+selector)
		} else if dir != "" {
			w.out = append(w.out, filepath.Join(dir, "agentlab.yaml"))
		}
	case "cp", "mv", litInstall:
		if len(raw) > 0 {
			w.add(st, dir, raw[len(raw)-1])
		}
		if name == "mv" {
			for _, r := range raw {
				if !strings.HasPrefix(r, "-") {
					w.add(st, dir, r)
				}
			}
		}
	case "rm", "unlink", transcriptTouch, "truncate":
		for _, r := range raw {
			if !strings.HasPrefix(r, "-") {
				w.add(st, dir, r)
			}
		}
	case litSed:
		for _, word := range words {
			if word == "-i" || strings.HasPrefix(word, "-i") || strings.HasPrefix(word, "--in-place") {
				for _, r := range raw {
					if !strings.HasPrefix(r, "-") {
						w.add(st, dir, r)
					}
				}
				break
			}
		}
	}
}
