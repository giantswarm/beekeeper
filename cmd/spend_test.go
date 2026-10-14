package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/config"
)

func TestSpendCmd(t *testing.T) {
	dir := t.TempDir()
	proj := filepath.Join(dir, "p")
	if err := os.MkdirAll(proj, 0o700); err != nil {
		t.Fatal(err)
	}
	lines := `{"type":"assistant","timestamp":"2026-10-07T23:00:00Z","message":{"id":"r0","model":"m","usage":{"input_tokens":1000000}}}
{"type":"assistant","timestamp":"2026-10-08T09:00:00Z","message":{"id":"r1","model":"m","usage":{"input_tokens":1000000,"output_tokens":100000}}}
{"type":"assistant","timestamp":"2026-10-08T10:00:00Z","message":{"id":"r2","model":"x","usage":{"input_tokens":1000}}}
`
	if err := os.WriteFile(filepath.Join(proj, "s.jsonl"), []byte(lines), 0o600); err != nil {
		t.Fatal(err)
	}
	// An omp session: p/m takes m's price, q/n has none.
	ompDir := filepath.Join(t.TempDir(), "-work")
	if err := os.MkdirAll(ompDir, 0o700); err != nil {
		t.Fatal(err)
	}
	ompLines := `{"type":"session","id":"o","timestamp":"2026-10-08T08:00:00Z","cwd":"/work"}
{"type":"message","id":"e1","timestamp":"2026-10-08T08:00:01Z","message":{"role":"assistant","provider":"p","model":"m","responseId":"o1","usage":{"input":1000000,"output":10000}}}
{"type":"message","id":"e2","timestamp":"2026-10-08T08:00:02Z","message":{"role":"assistant","provider":"q","model":"n","responseId":"o2","usage":{"input":2000}}}
`
	if err := os.WriteFile(filepath.Join(ompDir, "o.jsonl"), []byte(ompLines), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{}
	cfg.Claude.ProjectsDir = dir
	cfg.Omp.SessionsDir = filepath.Dir(ompDir)
	cfg.Metrics.Models = map[string]config.Model{"m": {Input: 3, Output: 15}}
	var out bytes.Buffer
	a := &app{cfg: cfg, now: time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC), out: &out}
	c := a.spendCmd()
	c.SetArgs([]string{"--since", "00:00", "--tz", "UTC"})
	if err := c.Execute(); err != nil {
		t.Fatal(err)
	}
	want := `2026-10-08T00:00:00Z to 2026-10-08T12:00:00Z: $7.65, 4 requests in 2 transcripts of 2 sessions, 2113k tokens
  m: $4.50, 1 requests, 1000k in, 0k cache write, 0k cache read, 100k out
  p/m: $3.15, 1 requests, 1000k in, 0k cache write, 0k cache read, 10k out
  q/n: unpriced, 1 requests, 2k in, 0k cache write, 0k cache read, 0k out
  x: unpriced, 1 requests, 1k in, 0k cache write, 0k cache read, 0k out
unpriced, left out of the total (metrics.models): q/n, x
`
	if out.String() != want {
		t.Errorf("got\n%s\nwant\n%s", out.String(), want)
	}
	c = a.spendCmd()
	c.SetArgs([]string{"--since", "13:00", "--until", "12:00", "--tz", "UTC"})
	c.SilenceErrors, c.SilenceUsage = true, true
	if err := c.Execute(); err != nil {
		t.Fatal(err) // 13:00 is yesterday's, before today's 12:00
	}
	// Bare dates are their 00:00: the window is the whole of October 8.
	out.Reset()
	a.now = time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	c = a.spendCmd()
	c.SetArgs([]string{"--since", "2026-10-08", "--until", "2026-10-09", "--tz", "UTC"})
	if err := c.Execute(); err != nil {
		t.Fatal(err)
	}
	if want := strings.Replace(want, "2026-10-08T12:00:00Z", "2026-10-09T00:00:00Z", 1); out.String() != want {
		t.Errorf("--since 2026-10-08 --until 2026-10-09: got\n%s\nwant\n%s", out.String(), want)
	}
}
