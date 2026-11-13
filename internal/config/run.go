package config

import (
	"cmp"
	"regexp"
	"strconv"

	"github.com/giantswarm/beekeeper/internal/remotebuild"
)

// Units configures the transient user services beekeeper starts (a gated
// merge's child, a wake's turn, a reopen).
type Units struct {
	// Quiet counts every end of such a unit as success, any exit status
	// and a kill by SIGKILL, SIGTERM or SIGABRT included: the run's own
	// files still record the real exit. It is for an isolated harness
	// state, whose stub tools and deliberate kills would otherwise fail
	// units in the person's user manager; the desk's own configuration
	// leaves it off, so a real tooling fault fails its unit.
	Quiet bool `yaml:"quiet"`
}

// DefaultMemcapMax is the fraction of RAM a capped command may use.
const DefaultMemcapMax = 0.14

// MemcapMax is a capped command's MemoryMax on a machine of ramMiB, a
// systemd size: memcap.max, else DefaultMemcapMax of the RAM.
func (c *Config) MemcapMax(ramMiB int) string {
	if c.Memcap.Max != "" || ramMiB <= 0 {
		return cmp.Or(c.Memcap.Max, "infinity")
	}
	return strconv.Itoa(int(DefaultMemcapMax*float64(ramMiB))) + "M"
}

// DefaultMemcapCPUQuota is the fraction of the cores every capped run
// together may use.
const DefaultMemcapCPUQuota = 0.5

// DefaultMemcapCPUWeight is memcap.slice's CPUWeight: half a desktop
// slice's 100, a third of the cores while the desktop wants them too.
const DefaultMemcapCPUWeight = 50

// MemcapCPUQuota is memcap.slice's CPUQuota on a machine of cores, a
// systemd quota (100% a core): memcap.cpuQuota, else DefaultMemcapCPUQuota
// of the cores; "" with the cores unknown: no quota.
func (c *Config) MemcapCPUQuota(cores int) string {
	if c.Memcap.CPUQuota != "" || cores <= 0 {
		return c.Memcap.CPUQuota
	}
	return strconv.Itoa(int(DefaultMemcapCPUQuota*float64(cores)*100)) + "%"
}

// RunConfig chooses where build commands execute.
type RunConfig struct {
	Remote remotebuild.Config `yaml:"remote"`
	// Tail sizes the tail of a failed run's output beekeeper keeps in its
	// record and prints once, so the failing test's name survives the
	// truncation: a systemd size ("256K", "1M"; default
	// DefaultRunTail, capped at one MiB), or "off" to keep nothing.
	Tail string `yaml:"tail"`
}

// DefaultRunTail is how much of a failed run's output beekeeper keeps
// by default: enough for a test suite's failure block and its panic.
const DefaultRunTail = "256K"

// runTailRe is a run.tail size: a number with an optional systemd suffix.
var runTailRe = regexp.MustCompile(`^(\d+)([KkMmGgTt]?)$`)

// Memcap locates the build slots of the memcap wrapper and caps the
// commands `beekeeper run` runs.
type Memcap struct {
	SlotDir string `yaml:"slotDir"`
	Slots   int    `yaml:"slots"`
	// Max is a command's MemoryMax, a systemd size ("12G"; default: a
	// fraction of RAM, DefaultMemcapMax).
	Max string `yaml:"max"`
	// CPUQuota is memcap.slice's CPUQuota, the cores every capped run
	// together may use, a systemd quota ("1200%" is twelve cores; default:
	// a fraction of the cores, DefaultMemcapCPUQuota). The slots share it
	// by equal weight.
	CPUQuota string `yaml:"cpuQuota"`
	// CPUWeight is memcap.slice's CPUWeight against the desktop's slices
	// (100 each): the runs' share of the cores while the desktop wants
	// them too (default DefaultMemcapCPUWeight).
	CPUWeight int `yaml:"cpuWeight"`
	// HoldBound is how long a run may hold a build slot before watch names
	// it and its holder (default 30m): a build longer than that is a
	// benchmark or a lab proof, which runs with --class and takes no slot.
	HoldBound Duration `yaml:"holdBound"`
}
