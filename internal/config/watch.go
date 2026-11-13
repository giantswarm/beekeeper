package config

import (
	"math"
	"time"
)

// Ollama is the host's ollama server, whose loaded models live in the
// iGPU's GTT.
type Ollama struct {
	// URL is its API (http://localhost:11434); unset, the machine runs none
	// and nothing watches or guards one.
	URL string `yaml:"url"`
	// Unit is its systemd unit, whose journal names the client that
	// loaded a model.
	Unit string `yaml:"unit"`
	// BudgetGiB is what a model-server claim may load without --gib;
	// MaxBudgetGiB the most a claim may ask for.
	BudgetGiB    int `yaml:"budgetGiB"`
	MaxBudgetGiB int `yaml:"maxBudgetGiB"`
	// NameOnly keeps the watch from unloading a model no lease covers: it
	// only names it.
	NameOnly bool `yaml:"nameOnly"`
	// LabTests are the agentlab subcommands that run turns on the host's
	// models; the hook refuses them outside the model-server lease.
	LabTests []string `yaml:"labTests"`
}

// Lemonade is the host's Lemonade Server, whose loaded models live in the
// iGPU's memory like ollama's.
type Lemonade struct {
	// URL is its API (http://localhost:13305); unset, the machine runs
	// none and nothing watches or guards one.
	URL string `yaml:"url"`
}

// Watch holds the thresholds of `beekeeper watch` (MiB unless noted). A
// memory or disk threshold left unset is a fraction of what the machine has
// (the Default* fractions): its absolute value is read with the method of
// the same name.
type Watch struct {
	// ThroughputAfter bounds how long an eligible PR may wait for a merge (default 2h).
	ThroughputAfter Duration `yaml:"throughputAfter"`
	// BatchInterval paces routine stdout notices. Negative disables batching;
	// --once always prints immediately. Action lines never wait for a batch.
	BatchInterval Duration `yaml:"batchInterval"`
	Interval      Duration `yaml:"interval"`
	StuckAfter    Duration `yaml:"stuckAfter"`
	// Repeat paces how often a lasting condition is handed to the
	// notifier (which sends it again after notify.repeat); the watch
	// prints it once when it starts and once when it ends.
	Repeat      Duration `yaml:"repeat"`
	BudgetEvery Duration `yaml:"budgetEvery"`
	AvailMinMiB int      `yaml:"availMinMiB"`
	SwapMaxMiB  int      `yaml:"swapMaxMiB"`
	// SwapWindow is how far back swap's growth rate looks (10m, at least
	// 5m): a burst older than the window has left the rate, which the SWAP
	// and OOMD IMMINENT checks and capacity's swap guard read.
	SwapWindow Duration `yaml:"swapWindow"`
	// OOMDHeadroomMinMiB and OOMDWithin decide when systemd-oomd's swap
	// kill is imminent (OOMD IMMINENT), only while oomd watches a cgroup
	// for swap and disk swap grows as MemAvailable falls: less swap growth
	// left before its SwapUsedLimit than OOMDHeadroomMinMiB, or the trigger
	// reached within OOMDWithin at the window's growth rate.
	OOMDHeadroomMinMiB int      `yaml:"oomdHeadroomMinMiB"`
	OOMDWithin         Duration `yaml:"oomdWithin"`
	ScopeAnonMaxMiB    int      `yaml:"scopeAnonMaxMiB"`
	// GTTMaxMiB is the iGPU GTT (system RAM the GPU driver pins, which no
	// cgroup counts) above which the watch prints IGPU GTT and names it
	// with the host ollama's models as the cause of LOW RAM and OOMD
	// IMMINENT.
	GTTMaxMiB int `yaml:"gttMaxMiB"`
	// LoadMax is the 1-minute load average over which the watch says HIGH
	// LOAD; unset (0), it is LoadPerCoreMax × the machine's cores
	// (LoadLimit).
	LoadMax        float64 `yaml:"loadMax"`
	LoadPerCoreMax float64 `yaml:"loadPerCoreMax"`
	PSIMax         float64 `yaml:"psiMax"`
	// CPUPSIMax is the CPU pressure ("some avg10" of /proc/pressure/cpu,
	// in percent) over which the watch says CPU PRESSURE once two samples
	// in a row read it, and over which, like over LoadLimit, the machine is
	// strained: the installation reads (upgrades, alerts, lane settling)
	// then run four times less often and at nice 10 (READS SLOWED).
	CPUPSIMax float64 `yaml:"cpuPSIMax"`
	// ForkRateMax is how far the fork rate (processes started per second,
	// from /proc/stat) must exceed the machine's usual one for the watch to
	// say PROCESS STORM, once two samples in a row read it; StackMax the number of copies of one command line
	// (arguments included), each running over a minute, over which it says
	// STACKED. A negative value turns the line off.
	ForkRateMax float64 `yaml:"forkRateMax"`
	StackMax    int     `yaml:"stackMax"`
	// ToolProcsMax is how many processes of the CLIs named in Tools may run
	// machine-wide before the watch says LOAD with the commands and the
	// sessions that run them: thirty sessions' kubectl, helm and tsh load
	// the CPU while no memory line fires. A negative value turns it off.
	ToolProcsMax int      `yaml:"toolProcsMax"`
	Tools        []string `yaml:"tools"`
	TmpMaxMiB    int      `yaml:"tmpMaxMiB"`
	DiskMinMiB   int      `yaml:"diskMinMiB"`
	// DiskCriticalMiB is the free space on / under which the watch says
	// DISK NEARLY FULL; DiskFillWithin how soon / would run full at the
	// rate its free space fell over the last minutes for the watch to say
	// DISK FILLING with the commands and sessions that wrote.
	DiskCriticalMiB int      `yaml:"diskCriticalMiB"`
	DiskFillWithin  Duration `yaml:"diskFillWithin"`
	// QuietSessions are globs (* matches any run) of the names of
	// short-lived sessions whose start, end and restart are no wake-up:
	// the watch logs them (watch.quiet) instead of printing them. Setting
	// it replaces the default, beekeeper's own tests ("test: *").
	QuietSessions []string `yaml:"quietSessions"`
	// Notify says whether the standby watch beekeeper install writes sends
	// the events that need a person to the desktop (watch --notify
	// --standby); off, the default, it runs as watch --standby.
	Notify bool `yaml:"notify"`
}

// The fractions a memory or disk threshold defaults to: of RAM (available
// memory, the desktop scope, the iGPU GTT), of swap (its use and oomd's
// headroom) and of the filesystem (/tmp's use, /'s free space).
const (
	DefaultAvailMin        = 0.12
	DefaultSwapMax         = 0.6
	DefaultOOMDHeadroomMin = 0.06
	DefaultScopeAnonMax    = 0.32
	DefaultGTTMax          = 0.28
	DefaultTmpMax          = 0.45
	DefaultDiskMin         = 0.05
	DefaultDiskCritical    = 0.01
)

// MinSwapSpan is the shortest span a swap growth rate is measured over, and
// the least Watch.SwapWindow may be: a shorter window would never rate one.
const MinSwapSpan = 5 * time.Minute

// AvailMin is the LOW RAM threshold on a machine of ramMiB.
func (w Watch) AvailMin(ramMiB int) int { return atLeast(w.AvailMinMiB, DefaultAvailMin, ramMiB) }

// SwapMax is the SWAP threshold on a machine of swapMiB.
func (w Watch) SwapMax(swapMiB int) int { return atMost(w.SwapMaxMiB, DefaultSwapMax, swapMiB) }

// OOMDHeadroomMin is the OOMD IMMINENT headroom on a machine of swapMiB.
func (w Watch) OOMDHeadroomMin(swapMiB int) int {
	return atLeast(w.OOMDHeadroomMinMiB, DefaultOOMDHeadroomMin, swapMiB)
}

// ScopeAnonMax is the DESKTOP SCOPE threshold on a machine of ramMiB.
func (w Watch) ScopeAnonMax(ramMiB int) int {
	return atMost(w.ScopeAnonMaxMiB, DefaultScopeAnonMax, ramMiB)
}

// GTTMax is the IGPU GTT threshold on a machine of ramMiB.
func (w Watch) GTTMax(ramMiB int) int { return atMost(w.GTTMaxMiB, DefaultGTTMax, ramMiB) }

// TmpMax is the TMPFS threshold on a /tmp of tmpMiB.
func (w Watch) TmpMax(tmpMiB int) int { return atMost(w.TmpMaxMiB, DefaultTmpMax, tmpMiB) }

// DiskMin is the LOW DISK threshold. Explicit positive values retain at least
// a 2 GiB reserve (or 5% on smaller disks). Negative disables it.
func (w Watch) DiskMin(diskMiB int) int {
	if w.DiskMinMiB < 0 {
		return 0
	}
	return max(min(2048, int(DefaultDiskMin*float64(diskMiB))), atLeast(w.DiskMinMiB, DefaultDiskMin, diskMiB))
}

// DiskCritical is the DISK NEARLY FULL threshold on a / of diskMiB.
func (w Watch) DiskCritical(diskMiB int) int {
	return atLeast(w.DiskCriticalMiB, DefaultDiskCritical, diskMiB)
}

// atLeast is a lower threshold: the configured one, else the fraction of
// total (0 when total is unknown, which never fires).
func atLeast(set int, fraction float64, total int) int {
	if set > 0 {
		return set
	}
	return int(fraction * float64(total))
}

// atMost is an upper threshold: the configured one, else the fraction of
// total (unbounded when total is unknown, which never fires).
func atMost(set int, fraction float64, total int) int {
	if set > 0 {
		return set
	}
	if total <= 0 {
		return math.MaxInt
	}
	return int(fraction * float64(total))
}

// KindClusters is the most kind clusters a machine of ramMiB runs:
// maxKindClusters, else one per 40 GiB, at least one.
func (c *Config) KindClusters(ramMiB int) int {
	if c.MaxKindClusters > 0 {
		return c.MaxKindClusters
	}
	return max(1, ramMiB/(40<<10))
}

// LoadLimit is the HIGH LOAD threshold on a machine of cores: LoadMax when
// set, LoadPerCoreMax × cores otherwise.
func (w Watch) LoadLimit(cores int) float64 {
	if w.LoadMax > 0 {
		return w.LoadMax
	}
	return w.LoadPerCoreMax * float64(cores)
}
