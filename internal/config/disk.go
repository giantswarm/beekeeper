package config

import (
	"fmt"
	"path/filepath"
	"strings"
)

// DiskPruneAuto and DiskPruneAsk select the automatic disk step or the
// existing manual commands and doctor trims.
const (
	DiskPruneAuto = "auto"
	DiskPruneAsk  = "ask"
)

// Disk configures automatic disk reclamation. Ask retains the existing
// on-demand commands and doctor trims, without the automatic disk step.
type Disk struct {
	Prune string   `yaml:"prune"`
	Every Duration `yaml:"every"`
	// SccacheMaxGiB caps sccache's local cache: a scheduled run trims it least
	// recently used first to three quarters of the cap; negative disables the cap.
	SccacheMaxGiB int `yaml:"sccacheMaxGiB"`
	// Checkouts locates main checkouts whose harness worktrees are inspected.
	Checkouts []string `yaml:"checkouts"`
	TrashDirs []string `yaml:"trashDirs"`
	// BuildDirs adds regenerable directory names inside worktrees.
	BuildDirs []string `yaml:"buildDirs"`
	// BuildOutputs associates external build output with its owning worktree.
	BuildOutputs []BuildOutput `yaml:"buildOutputs"`
	// Caches are the disk step's cache policies. Beekeeper's own caches are
	// its default entries (DiskCacheGo, DiskCacheSccache, DiskCacheTmp): an
	// entry of the same name changes them, mode off drops them.
	Caches []DiskCache `yaml:"caches"`
	// Holders is the lifecycle of the state home's holders (beekeeper disk
	// holders): it lists them and what is eligible for removal, removing
	// nothing.
	Holders Holders `yaml:"holders"`
}

// Holders configures the listing of the state home's holders: each entry of
// Root (default the parent of stateDir, ~/.local/state), and each entry of
// its AsideDirs one by one. A holder unused for StaleAfter (default 72h), or
// an aside entry kept past AsideRetention (default 168h), whose task is not
// open and which no process uses, is eligible for removal; the watch says
// STATE HOLDERS once the eligible ones reach WarnGiB (default 10; negative,
// never), looking every Every (default 6h).
type Holders struct {
	Root           string   `yaml:"root"`
	StaleAfter     Duration `yaml:"staleAfter"`
	AsideRetention Duration `yaml:"asideRetention"`
	// AsideDirs are the root's directories holding moved-aside material
	// (default aside, moved-aside).
	AsideDirs []string `yaml:"asideDirs"`
	// Keep names the root's entries never eligible (model weights, costly
	// to fetch again).
	Keep    []string `yaml:"keep"`
	WarnGiB int      `yaml:"warnGiB"`
	Every   Duration `yaml:"every"`
}

// Cache modes of the disk step. Entries removes a root's direct children
// whole, files its single files, command runs the tool's own cleaner; go and sccache are the built-in Go build
// cache and sccache trims; off drops an entry.
const (
	DiskCacheEntries = "entries"
	DiskCacheFiles   = "files"
	DiskCacheCommand = "command"
	DiskCacheModeGo  = "go"
	DiskCacheModeSc  = "sccache"
	DiskCacheOff     = "off"
)

// Names of the built-in cache entries.
const (
	DiskCacheGo          = "go-build"
	DiskCacheSccache     = "sccache"
	DiskCacheTmp         = "beekeeper-tmp"
	DiskCacheTranscripts = "transcripts"
)

// DiskCache is one cache policy of the disk step. Path is absolute (or
// ~/...) and may be a glob: each match is a cache root. In entries mode a
// root's direct children are the units, in files mode its files. A unit
// untouched for OlderThan goes; past MaxGiB the least recently written go
// until the root fits; under disk pressure a Regenerable cache keeps only
// what was written within the hour. Nothing written within the hour, nothing
// a process uses and nothing holding a git checkout is ever removed.
type DiskCache struct {
	// Name names the entry in reports (default its path).
	Name        string   `yaml:"name"`
	Path        string   `yaml:"path"`
	Mode        string   `yaml:"mode"`
	OlderThan   Duration `yaml:"olderThan"`
	MaxGiB      float64  `yaml:"maxGiB"`
	Regenerable bool     `yaml:"regenerable"`
	// Command is mode command's cleaner, the tool's own ("uv cache prune"):
	// split into words and run without a shell in the cache root, once the
	// root holds more than MaxGiB and under disk pressure (no MaxGiB: under
	// pressure only), never while a process references the root.
	Command string `yaml:"command"`
}

// MaxBytes is the size budget in bytes; 0 is none.
func (d DiskCache) MaxBytes() int64 { return int64(d.MaxGiB * (1 << 30)) }

// Builtin reports whether name is one of beekeeper's own cache entries.
func (DiskCache) Builtin(name string) bool {
	return name == DiskCacheGo || name == DiskCacheSccache || name == DiskCacheTmp || name == DiskCacheTranscripts || name == "agy-conversations" || name == "agy-brain"
}

// BuildOutput is explicitly disposable output of the task in Worktree.
// It is reclaimed only once that worktree's task ended and no process uses it.
type BuildOutput struct {
	Path     string `yaml:"path"`
	Worktree string `yaml:"worktree"`
}

// Auto reports whether the disk step runs without a person's answer.
func (d Disk) Auto() bool { return d.Prune == "" || d.Prune == DiskPruneAuto }

// homePath is p with a leading ~/ resolved against home.
// validateDiskCaches refuses a cache policy that could reach beyond a cache:
// a relative path or filesystem root, a bad glob, a rule-less custom entry.
func validateDiskCachesEach(caches []DiskCache, report func(error)) {
	names := map[string]bool{}
	for _, e := range caches {
		if e.Name == "" || names[e.Name] {
			report(fmt.Errorf("disk.caches: each entry needs a path or a unique name (%q)", e.Name))
		}
		names[e.Name] = true
		builtin := e.Builtin(e.Name)
		switch e.Mode {
		case "", DiskCacheEntries, DiskCacheFiles, DiskCacheOff:
		case DiskCacheCommand:
			if strings.TrimSpace(e.Command) == "" {
				report(fmt.Errorf("disk.caches %s: mode command needs a command", e.Name))
			}
		case DiskCacheModeGo, DiskCacheModeSc:
			if (e.Mode == DiskCacheModeGo) != (e.Name == DiskCacheGo) || (e.Mode == DiskCacheModeSc) != (e.Name == DiskCacheSccache) {
				report(fmt.Errorf("disk.caches %s: mode %s belongs to the built-in entry of that cache alone", e.Name, e.Mode))
			}
		default:
			report(fmt.Errorf("disk.caches %s: mode %q is none of entries, files, command, off", e.Name, e.Mode))
		}
		if e.Command != "" && e.Mode != DiskCacheCommand {
			report(fmt.Errorf("disk.caches %s: command needs mode command", e.Name))
		}
		if e.OlderThan.Duration < 0 || e.MaxGiB < 0 {
			report(fmt.Errorf("disk.caches %s: olderThan and maxGiB take no negative value", e.Name))
		}
		if e.Path == "" {
			if !builtin {
				report(fmt.Errorf("disk.caches %s: path is required", e.Name))
			}
			continue
		}
		if !filepath.IsAbs(e.Path) || filepath.Dir(e.Path) == e.Path {
			report(fmt.Errorf("disk.caches %s: path must be absolute and not a filesystem root", e.Name))
		}
		if _, err := filepath.Match(e.Path, ""); err != nil {
			report(fmt.Errorf("disk.caches %s: path: %w", e.Name, err))
		}
		if !builtin && e.Mode != DiskCacheOff && e.Mode != DiskCacheCommand && e.OlderThan.Duration == 0 && e.MaxGiB == 0 && !e.Regenerable {
			report(fmt.Errorf("disk.caches %s: set olderThan, maxGiB or regenerable", e.Name))
		}
	}
}
