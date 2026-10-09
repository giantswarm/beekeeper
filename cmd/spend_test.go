package cmd

import (
	"bytes"
	"os"
	"path/filepath"
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
	cfg := &config.Config{}
	cfg.Claude.ProjectsDir = dir
	cfg.Metrics.Models = map[string]config.Model{"m": {Input: 3, Output: 15}}
	var out bytes.Buffer
	a := &app{cfg: cfg, now: time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC), out: &out}
	c := a.spendCmd()
	c.SetArgs([]string{"--since", "00:00", "--tz", "UTC"})
	if err := c.Execute(); err != nil {
		t.Fatal(err)
	}
	want := `2026-10-08T00:00:00Z to 2026-10-08T12:00:00Z: $4.50, 2 requests in 1 transcripts of 1 sessions, 1101k tokens
  m: $4.50, 1 requests, 1000k in, 0k cache write, 0k cache read, 100k out
  x: unpriced, 1 requests, 1k in, 0k cache write, 0k cache read, 0k out
unpriced, left out of the total (metrics.models): x
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
}
