package config

import (
	"regexp"
	"strings"
)

// Fatal configures additional probes for critical machine components.
type Fatal struct {
	// Checks are fatal health checks, never arbitrary worker reports. Exit 0
	// is healthy, 1 is failed, 2 is deliberately disabled, others unknown.
	Checks []FatalCheck `yaml:"checks"`
	// Repositories are repositories whose default branch CI is critical.
	Repositories []string `yaml:"repositories"`
	// Checkouts maps repair repositories to source checkouts; otherwise the
	// source is projects.dir/<repository-name>. Repairs use managed worktrees.
	Checkouts map[string]string `yaml:"checkouts"`
}

type FatalCheck struct {
	Name  string   `yaml:"name"`
	Probe []string `yaml:"probe"`
	// Harness excludes the broken harness from repair selection.
	Harness string `yaml:"harness"`
	// Repository owns this check's repair (default giantswarm/beekeeper).
	Repository string `yaml:"repository"`
}

// Doctor configures the known faults the doctor probes and remedies.
type Doctor struct {
	Faults []Fault `yaml:"faults"`
	// GoCacheMaxGiB caps the Go build cache every session's builds share:
	// over it the watch trims the least recently used entries down to
	// three quarters of it, while no go build runs or once the cache is
	// past it by a quarter. Default 20; negative: off.
	GoCacheMaxGiB int `yaml:"goCacheMaxGiB"`
	// GoCacheEvery is how often the cache's size is read (default 1h);
	// a trim a build held back is tried again every five minutes.
	GoCacheEvery Duration `yaml:"goCacheEvery"`
	// FreeEvery is how often the watch frees what dead sessions and dead go
	// commands leave in the tmpfs /tmp (free --apply's automatic sections;
	// default 1h); negative: never.
	FreeEvery Duration `yaml:"freeEvery"`
	// DockerImagesMaxGiB caps the machine's Docker images: over it the
	// watch removes the unused images created longest ago down to three
	// quarters of it, never one a container or a kind lab's node uses, and
	// never while an image import or build runs or a lab lease is fresh.
	// Default 40; negative: off.
	DockerImagesMaxGiB int `yaml:"dockerImagesMaxGiB"`
	// DockerBuildCacheMaxGiB caps Docker's build cache likewise, pruned
	// by Docker's own least-recently-used order. Default 20; negative: off.
	DockerBuildCacheMaxGiB int `yaml:"dockerBuildCacheMaxGiB"`
	// DockerEvery is how often the watch reads Docker's sizes (default 1h);
	// a prune an import held back is tried again at the next sample.
	DockerEvery Duration `yaml:"dockerEvery"`
	// WorktreeRoots are the folders (~/ allowed) under which the watch
	// looks for the linked git worktrees of finished workers (default
	// ~/.local/state/beekeeper/worktrees).
	WorktreeRoots []string `yaml:"worktreeRoots"`
	// WorktreesEvery is how often the watch removes the worktrees whose
	// pull request merged or closed, and the build output of those that
	// still hold uncommitted or unpushed work and of any other untouched
	// for six hours (default 6h); negative: never.
	WorktreesEvery Duration `yaml:"worktreesEvery"`
}

// DockerImagesMax is the Docker images' cap in bytes; 0 is off.
func (d Doctor) DockerImagesMax() int64 { return gib(d.DockerImagesMaxGiB) }

// DockerBuildCacheMax is the Docker build cache's cap in bytes; 0 is off.
func (d Doctor) DockerBuildCacheMax() int64 { return gib(d.DockerBuildCacheMaxGiB) }

// gib is n GiB in bytes, 0 for a negative n.
func gib(n int) int64 { return int64(max(n, 0)) << 30 }

// GoCacheMax is the Go build cache's cap in bytes; 0 is off.
func (d Doctor) GoCacheMax() int64 {
	if d.GoCacheMaxGiB < 0 {
		return 0
	}
	return int64(d.GoCacheMaxGiB) << 30
}

// Fault is a known fault with a known remedy.
type Fault struct {
	Name string `yaml:"name"`
	// Probe is a shell command that exits 0 while the fault is absent.
	Probe string `yaml:"probe"`
	// Remedy is the shell command that fixes the fault.
	Remedy string `yaml:"remedy"`
	// Unattended lets the watch run the remedy by itself; otherwise a
	// sighting is one note for guide.person.
	Unattended bool `yaml:"unattended"`
}

// ownerReposEach reports entries of field that are not owner/repo.
func validFatalRepository(repo string) bool {
	if !regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`).MatchString(repo) {
		return false
	}
	for _, part := range strings.Split(repo, "/") {
		if part == "." || part == ".." {
			return false
		}
	}
	return true
}
