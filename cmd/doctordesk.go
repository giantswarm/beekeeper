package cmd

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/giantswarm/beekeeper/internal/brieflint"
)

// These are warning signatures, not a proof that arbitrary prose is free of
// duplicated workflow. Keep source lines out of diagnostics: desks are private.
var deskWorkflow = regexp.MustCompile(`(?i)\b(devctl\s+pr\s+(merge|wait)|gh\s+pr\s+merge|beekeeper\s+agents\s+(register|park|idle)|beekeeper\s+gate\s+(merge|submit))\b|\b(workers?\s+(must|should)\s+(push|park|report)|loop\s+owns\s+(CI|every merge)|no\s+force[ -]push)\b`)

func (a *app) doctorDeskWorkflow() []string {
	return checkDeskWorkflow(homePath(a.cfg.Guide.Desk), []string{
		homePath(a.cfg.Supervisor.WorkerBrief), homePath(a.cfg.Supervisor.ReviewBrief),
	})
}

func checkDeskWorkflow(desk string, briefs []string) []string {
	var lines []string
	seen := make(map[string]bool)
	check := func(name string) {
		name = filepath.Clean(name)
		if seen[name] {
			return
		}
		seen[name] = true
		// Resolve each configured or discovered file within its parent directory.
		// Rooted reads also prevent a replacement symlink from escaping it.
		root, err := os.OpenRoot(filepath.Dir(name))
		if err != nil {
			lines = append(lines, fmt.Sprintf("desk workflow: cannot read %s: %v", name, err))
			return
		}
		defer func() { _ = root.Close() }()
		base := filepath.Base(name)
		info, err := root.Lstat(base)
		if err == nil && info.Mode()&os.ModeSymlink != 0 {
			return // Installed skill symlinks are references, not desk copies.
		}
		if err == nil && !info.Mode().IsRegular() {
			lines = append(lines, fmt.Sprintf("desk workflow: cannot read %s: not a regular file", name))
			return
		}
		var raw []byte
		if err == nil {
			raw, err = root.ReadFile(base)
		}
		if err != nil {
			lines = append(lines, fmt.Sprintf("desk workflow: cannot read %s: %v", name, err))
			return
		}
		for i, line := range strings.Split(brieflint.WithoutData(string(raw)), "\n") {
			if deskWorkflow.MatchString(line) {
				lines = append(lines, fmt.Sprintf("desk workflow: %s:%d may restate beekeeper's merge or worker workflow; remove the duplicate, keep desk facts and dated orders, and file missing behavior in giantswarm/beekeeper (docs/desk-boundary.md)", name, i+1))
			}
		}
	}
	for _, name := range briefs {
		if name != "" {
			check(name)
		}
	}
	if desk == "" {
		return lines
	}
	err := filepath.WalkDir(desk, func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			lines = append(lines, fmt.Sprintf("desk workflow: cannot inspect %s: %v", name, err))
			if entry != nil && entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.IsDir() {
			if entry.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.EqualFold(filepath.Ext(name), ".md") || entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		rel, err := filepath.Rel(desk, name)
		if err != nil {
			return err
		}
		for _, part := range strings.Split(filepath.Dir(rel), string(filepath.Separator)) {
			if part == "briefs" || part == "skills" {
				check(name)
				break
			}
		}
		return nil
	})
	if err != nil {
		lines = append(lines, fmt.Sprintf("desk workflow: cannot inspect %s: %v", desk, err))
	}
	return lines
}
