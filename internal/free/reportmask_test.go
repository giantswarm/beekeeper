package free

import (
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/proc"
)

const reportChildScript = "processChild.js"

const reportTokenFlag = "--token"

const reportNode = "node"

func TestReportsMaskProcessArguments(t *testing.T) {
	for _, summary := range []bool{false, true} {
		name := "report"
		if summary {
			name = "summary"
		}
		t.Run(name, func(t *testing.T) {
			r, out := newRun(t, Options{Summary: summary, HeavyMiB: 1, Only: []string{Orphans}, Orphan: 30 * time.Minute})
			r.Table = proc.NewTable(
				&proc.Process{PID: 10, PPID: 2, Comm: litWorker, RSSKiB: 4096, Start: now.Add(-time.Hour),
					Args: []string{"worker", reportTokenFlag, "fixture-token", "PASSWORD=fixture-password", "https://user:fixture-url@host.invalid/path"}},
				&proc.Process{PID: 20, PPID: 1, Comm: reportNode, RSSKiB: 4096, Start: now.Add(-time.Hour),
					Args: []string{reportNode, reportChildScript, reportTokenFlag, "fixture-orphan"}},
			)
			r.anonKiB = func(int) int { return 2048 }
			r.Do()
			for _, value := range []string{"fixture-token", "fixture-password", "fixture-url", "fixture-orphan"} {
				if strings.Contains(out.String(), value) {
					t.Errorf("process argument %q reached %s output", value, name)
				}
			}
			if !strings.Contains(out.String(), reportTokenFlag) || !strings.Contains(out.String(), "(masked)") {
				t.Errorf("masked output should retain flag names and identify omitted values:\n%s", out)
			}
		})
	}
}

func TestReportsKeepMaskedMarkerAfterTruncation(t *testing.T) {
	for _, summary := range []bool{false, true} {
		r, out := newRun(t, Options{Summary: summary, HeavyMiB: 1, Only: []string{Orphans}, Orphan: 30 * time.Minute})
		r.Table = proc.NewTable(&proc.Process{
			PID: 20, PPID: 1, Comm: reportNode, RSSKiB: 4096, Start: now.Add(-time.Hour),
			Args: []string{reportNode, reportChildScript, strings.Repeat("界", 140), reportTokenFlag, "fixture-secret"},
		})
		r.anonKiB = func(int) int { return 2048 }
		r.Do()
		seen := 0
		for _, line := range strings.Split(out.String(), "\n") {
			if !strings.Contains(line, reportChildScript) {
				continue
			}
			seen++
			if !strings.Contains(line, " (masked)") || strings.Contains(line, "fixture-secret") {
				t.Errorf("summary=%v: truncated process line lost masking: %s", summary, line)
			}
		}
		want := 2 // heavy report and orphan action
		if summary {
			want = 1 // heavy TSV row; actions are suppressed
		}
		if seen != want {
			t.Errorf("summary=%v: got %d process lines, want %d", summary, seen, want)
		}
	}
}
