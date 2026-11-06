package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const releasePipelinePhase = "release wait: pipeline"

// mergeProgress is persisted by the child, so detached gates and later lanes
// invocations see the same phase clock. Output refreshes Last, not Since.
type mergeProgress struct {
	Phase          string        `json:"phase"`
	Since          time.Time     `json:"since"`
	Last           time.Time     `json:"last"`
	Timeout        time.Duration `json:"timeout"`
	base           string
	pending        string
	ciTimeout      time.Duration
	releaseTimeout time.Duration
}

func argumentTimeout(argv []string, flag string, fallback time.Duration) time.Duration {
	for i, arg := range argv {
		value, ok := strings.CutPrefix(arg, flag+"=")
		if arg == flag && i+1 < len(argv) {
			value, ok = argv[i+1], true
		}
		if ok {
			if d, err := time.ParseDuration(value); err == nil && d > 0 {
				return d
			}
		}
	}
	return fallback
}

func newMergeProgress(base string, argv []string, now time.Time) *mergeProgress {
	p := &mergeProgress{Phase: "CI", Since: now, Last: now, base: base,
		ciTimeout:      argumentTimeout(argv, timeoutFlag, time.Hour),
		releaseTimeout: argumentTimeout(argv, "--release-timeout", time.Hour)}
	p.Timeout = p.ciTimeout
	p.save()
	return p
}

func (p *mergeProgress) save() {
	raw, err := json.Marshal(p)
	if err != nil {
		return
	}
	root, err := os.OpenRoot(filepath.Dir(p.base))
	if err != nil {
		return
	}
	defer func() { _ = root.Close() }()
	name := filepath.Base(p.base) + ".progress"
	if root.WriteFile(name+".tmp", raw, 0o600) == nil {
		_ = root.Rename(name+".tmp", name)
	}
}

func (p *mergeProgress) Write(b []byte) (int, error) {
	p.observe(string(b), time.Now().UTC())
	return len(b), nil
}

func (p *mergeProgress) observe(output string, now time.Time) {
	p.Last = now
	p.pending += output
	for {
		line, rest, ok := strings.Cut(p.pending, "\n")
		if !ok {
			break
		}
		p.pending = rest
		phase := p.Phase
		lower := strings.ToLower(line)
		switch {
		case strings.HasPrefix(lower, "release: waiting"):
			phase = "release wait: tag"
		case strings.HasPrefix(lower, "merged:"), strings.HasPrefix(lower, "merge queue:"), strings.Contains(lower, "retrying the merge call"), strings.Contains(lower, ": green"):
			phase = gitMerge
		case strings.HasPrefix(phase, "release wait:"):
			switch {
			case strings.Contains(lower, "does not exist yet"), strings.HasPrefix(lower, "no tag on "):
				phase = "release wait: tag"
			case strings.HasPrefix(lower, "tag "), strings.Contains(lower, "pipeline"), strings.HasPrefix(lower, "status "), strings.HasPrefix(lower, "actions run "), strings.HasPrefix(lower, "check run "), strings.Contains(lower, "workflows"):
				phase = releasePipelinePhase
			case strings.Contains(lower, "every artifact is available"):
				phase = releasePipelinePhase
			case strings.HasPrefix(lower, "expecting "), strings.Contains(lower, "registry"), strings.Contains(lower, " available ("), strings.Contains(lower, "asset"), strings.HasPrefix(lower, "no release for "), strings.HasPrefix(lower, "catalog "):
				phase = "release wait: assets"
			}
		}
		if phase != p.Phase {
			p.Phase, p.Since = phase, now
		}
		if strings.HasPrefix(phase, "release wait:") {
			p.Timeout = p.releaseTimeout
		} else {
			p.Timeout = p.ciTimeout
		}
	}
	p.save()
}

func readMergeProgress(base string) *mergeProgress {
	raw, err := os.ReadFile(base + ".progress") //nolint:gosec // private child state
	var p mergeProgress
	if err != nil || json.Unmarshal(raw, &p) != nil || p.Since.IsZero() || p.Last.IsZero() || p.Timeout <= 0 {
		return nil
	}
	return &p
}

// silentMerge waits a full phase timeout since the last byte from devctl.
// Old children without phase records use the log's last write as a fallback.
func silentMerge(base string, started, now time.Time, fallback time.Duration) bool {
	if p := readMergeProgress(base); p != nil {
		return now.Sub(p.Last) > p.Timeout
	}
	if info, err := os.Stat(base + ".log"); err == nil && info.ModTime().After(started) {
		started = info.ModTime()
	}
	return now.Sub(started) > fallback
}
