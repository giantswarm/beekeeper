//go:build perf && linux

package perfsuite

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// limit is one budget. Known names the open issue that has the measured value
// over the limit today: the row reports it without failing, and fails once the
// value is within the limit, so the marker is removed and the budget enforced
// from then on.
type limit struct {
	Max   float64
	Known string
}

// measured is no budget: the value is reported only.
var measured = limit{Max: -1}

// row is one measured value against its budget.
type row struct {
	Test   string  `json:"test"`
	Case   string  `json:"case"`
	Metric string  `json:"metric"`
	Unit   string  `json:"unit"`
	Value  float64 `json:"value"`
	Limit  float64 `json:"limit"`
	Status string  `json:"status"`
	Known  string  `json:"known,omitempty"`
}

type report struct {
	mu    sync.Mutex
	rows  []row
	notes []string
}

var rep = &report{}

// units of the metrics the suite measures.
var units = map[string]string{
	"wall": "s", "cpu": "s", "read": "MiB", "write": "MiB", "rss": "MiB", "mem": "MiB",
	"children": "processes", "procs": "processes", "hold": "s", "growth": "ratio", "exit": "code",
	"core": "cores", "read/s": "MiB/s",
}

const mib = 1 << 20

// check records value against l and fails t when it is over a budget that
// is not a known regression, or within one that is.
func (r *report) check(t *testing.T, kase, metric string, value float64, l limit) {
	t.Helper()
	status := "ok"
	// A control run (BEEKEEPER_PERF_STRICT=1) holds every budget, known or not.
	if os.Getenv("BEEKEEPER_PERF_STRICT") == "1" {
		l.Known = ""
	}
	switch {
	case l.Max < 0:
		status = "measured"
	case value > l.Max && l.Known != "":
		status = "over, known"
	case value > l.Max:
		status = "OVER"
	case l.Known != "":
		status = "met, remove known"
	}
	r.mu.Lock()
	r.rows = append(r.rows, row{Test: t.Name(), Case: kase, Metric: metric, Unit: units[metric], Value: value, Limit: l.Max, Status: status, Known: l.Known})
	r.mu.Unlock()
	switch status {
	case "OVER":
		t.Errorf("%s %s: %.3f %s over the budget of %.3f %s", kase, metric, value, units[metric], l.Max, units[metric])
	case "over, known":
		t.Logf("%s %s: %.3f %s over the budget of %.3f %s, known: %s", kase, metric, value, units[metric], l.Max, units[metric], l.Known)
	case "met, remove known":
		t.Errorf("%s %s: %.3f %s is within the budget of %.3f %s now: remove the known marker of %s so the budget is enforced", kase, metric, value, units[metric], l.Max, units[metric], l.Known)
	default:
		t.Logf("%s %s: %.3f %s (budget %.3f)", kase, metric, value, units[metric], l.Max)
	}
}

// note keeps a line for the report, beside the table.
func (r *report) note(format string, args ...any) {
	r.mu.Lock()
	r.notes = append(r.notes, fmt.Sprintf(format, args...))
	r.mu.Unlock()
}

// write prints the table to the job output, and keeps it as JSON and
// Markdown under BEEKEEPER_PERF_REPORT, with the commit and version it is for.
func (r *report) write() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.rows) == 0 && len(r.notes) == 0 {
		return nil
	}
	commit := firstEnv("CIRCLE_SHA1", "GITHUB_SHA", "BEEKEEPER_PERF_COMMIT")
	ref := firstEnv("CIRCLE_TAG", "CIRCLE_BRANCH", "GITHUB_REF_NAME")
	var b strings.Builder
	fmt.Fprintf(&b, "# beekeeper performance\n\ncommit: %s\nref: %s\nbinary: %s\nmeasured: %s\n\n", commit, ref, binaryVersion, time.Now().UTC().Format(time.RFC3339))
	b.WriteString("| test | case | metric | value | budget | status |\n|---|---|---|---:|---:|---|\n")
	rows := slices.Clone(r.rows)
	for _, x := range rows {
		budget := "-"
		if x.Limit >= 0 {
			budget = fmt.Sprintf("%.3f", x.Limit)
		}
		status := x.Status
		if x.Known != "" {
			status += " " + x.Known
		}
		fmt.Fprintf(&b, "| %s | %s | %s | %.3f %s | %s | %s |\n", x.Test, x.Case, x.Metric, x.Value, x.Unit, budget, status)
	}
	if len(r.notes) > 0 {
		b.WriteString("\n")
		for _, n := range r.notes {
			b.WriteString("- " + n + "\n")
		}
	}
	fmt.Println(b.String())
	dir := os.Getenv("BEEKEEPER_PERF_REPORT")
	if dir == "" {
		return nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(map[string]any{"commit": commit, "ref": ref, "binary": binaryVersion, "rows": rows, "notes": r.notes}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "report.json"), raw, 0o600); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "report.md"), []byte(b.String()), 0o600)
}

func firstEnv(names ...string) string {
	for _, n := range names {
		if v := os.Getenv(n); v != "" {
			return v
		}
	}
	return "unknown"
}
