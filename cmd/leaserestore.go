package cmd

import (
	"path/filepath"
	"strings"

	"github.com/giantswarm/beekeeper/internal/proc"
)

// A platform command can outlive its worker's turn. Keep the lease until
// it exits, including on forced and automatic releases. Missing --lab is
// ambiguous, so it conservatively protects every lab.
func labPlatformPID(table *proc.Table, resource, cluster string) int {
	for _, p := range table.ByPID {
		if len(p.Args) < 2 || filepath.Base(p.Args[0]) != "agentlab" {
			continue
		}
		platform := false
		lab := ""
		for i, arg := range p.Args[1:] {
			if arg == "platform" {
				platform = true
			}
			if strings.HasPrefix(arg, "--lab=") {
				lab = strings.TrimPrefix(arg, "--lab=")
			}
			if arg == "--lab" && i+2 < len(p.Args) {
				lab = p.Args[i+2]
			}
		}
		if platform && (lab == "" || lab == resource || lab == cluster) {
			return p.PID
		}
	}
	return 0
}

func (a *app) labReleaseReady(resource string) error {
	cluster := a.cfg.LabCluster(resource)
	if cluster == "" {
		return nil
	}
	table, err := plat.Machine.Processes()
	if err != nil {
		return refused("%s not released: cannot check running lab restore processes: %v", resource, err)
	}
	if pid := labPlatformPID(table, resource, cluster); pid != 0 {
		return refused("%s not released: agentlab platform process %d is still running; release after the restore exits", resource, pid)
	}
	return nil
}
