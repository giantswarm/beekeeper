package claude

import (
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/config"
)

// spendPrices are hand-picked prices per million tokens: m costs $1 in, $3
// per 1h cache write, $2 per 5m cache write, $0.10 per cache read, $10 out,
// six times that in fast mode.
var spendPrices = config.Metrics{Models: map[string]config.Model{
	"m": {Input: 1, Output: 10, CacheWrite5m: 2, CacheWrite1h: 3, CacheRead: 0.1, Fast: 6},
}}

// spendFixture writes the transcript set: a session with its subagent, a
// fork of it that copies one response, and a transcript last written
// before the window.
func spendFixture(t *testing.T, dir string, from time.Time) {
	t.Helper()
	files := map[string][]string{
		"projA/s1.jsonl": {
			// Before the window: $1.
			`{"type":"assistant","timestamp":"2026-10-08T07:00:00Z","message":{"id":"a0","model":"m","usage":{"input_tokens":1000000}}}`,
			// Streamed over two lines, the last with the final usage:
			// 100k in $0.10, 1M 1h write $3, 10M read $1, 50k out $0.50.
			`{"type":"assistant","timestamp":"2026-10-08T09:00:00Z","message":{"id":"a1","model":"m","usage":{"input_tokens":100000,"output_tokens":5}}}`,
			`{"type":"user","timestamp":"2026-10-08T09:00:01Z","message":{"content":"\"usage\" in a person's words"}}`,
			`{"type":"assistant","timestamp":"2026-10-08T09:00:02Z","message":{"id":"a1","model":"m","usage":{"input_tokens":100000,"cache_creation_input_tokens":1000000,"cache_read_input_tokens":10000000,"output_tokens":50000,"cache_creation":{"ephemeral_5m_input_tokens":0,"ephemeral_1h_input_tokens":1000000}}}}`,
			// Fast: 1M in, $1 times 6.
			`{"type":"assistant","timestamp":"2026-10-08T10:00:00Z","message":{"id":"a2","model":"m","usage":{"input_tokens":1000000,"speed":"fast"}}}`,
			// No price.
			`{"type":"assistant","timestamp":"2026-10-08T10:30:00Z","message":{"id":"u1","model":"x-unpriced","usage":{"input_tokens":500000}}}`,
			// A synthetic message: no request.
			`{"type":"assistant","timestamp":"2026-10-08T10:40:00Z","message":{"id":"s0","model":"<synthetic>","usage":{"input_tokens":0}}}`,
		},
		// A dated model id takes its family's price; a write without the
		// TTL split is a 5m one: 1M at $2.
		"projA/s1/subagents/agent-1.jsonl": {
			`{"type":"assistant","isSidechain":true,"timestamp":"2026-10-08T11:00:00Z","message":{"id":"b1","model":"m-20251001","usage":{"cache_creation_input_tokens":1000000}}}`,
		},
		// A fork carries a1 again, which counts once; c2 2M in $2; c1 is
		// after the window.
		"projB/s2.jsonl": {
			`{"type":"assistant","timestamp":"2026-10-08T09:00:00Z","message":{"id":"a1","model":"m","usage":{"input_tokens":100000,"cache_creation_input_tokens":1000000,"cache_read_input_tokens":10000000,"output_tokens":50000,"cache_creation":{"ephemeral_5m_input_tokens":0,"ephemeral_1h_input_tokens":1000000}}}}`,
			`{"type":"assistant","timestamp":"2026-10-08T11:30:00Z","message":{"id":"c2","model":"m","usage":{"input_tokens":2000000}}}`,
			`{"type":"assistant","timestamp":"2026-10-08T12:30:00Z","message":{"id":"c1","model":"m","usage":{"input_tokens":2000000}}}`,
		},
		"projC/old.jsonl": {
			`{"type":"assistant","timestamp":"2026-10-08T09:30:00Z","message":{"id":"o1","model":"m","usage":{"input_tokens":9000000}}}`,
		},
		"projC/notes.txt": {`{"type":"assistant","message":{"id":"n1","model":"m","usage":{"input_tokens":9000000}}}`},
	}
	for name, lines := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	old := from.Add(-time.Hour)
	if err := os.Chtimes(filepath.Join(dir, "projC/old.jsonl"), old, old); err != nil {
		t.Fatal(err)
	}
}

func TestReadSpendPricesTheWindowOnce(t *testing.T) {
	dir := t.TempDir()
	from := time.Date(2026, 10, 8, 8, 0, 0, 0, time.UTC)
	to := from.Add(4 * time.Hour)
	spendFixture(t, dir, from)
	s, err := ReadSpend(dir, from, to, spendPrices)
	if err != nil {
		t.Fatal(err)
	}
	// a1 $4.60 + a2 $6 + b1 $2 + c2 $2.
	if math.Abs(s.USD-14.60) > 1e-9 {
		t.Errorf("usd %.6f, want 14.60", s.USD)
	}
	if s.Requests != 5 || s.Transcripts != 3 || s.Sessions != 2 {
		t.Errorf("requests %d, transcripts %d, sessions %d; want 5, 3, 2", s.Requests, s.Transcripts, s.Sessions)
	}
	if want := (Tokens{Input: 3_600_000, CacheWrite5m: 1_000_000, CacheWrite1h: 1_000_000, CacheRead: 10_000_000, Output: 50_000}); s.Tokens != want {
		t.Errorf("tokens %+v, want %+v", s.Tokens, want)
	}
	if !slices.Equal(s.Unpriced, []string{"x-unpriced"}) {
		t.Errorf("unpriced %v", s.Unpriced)
	}
	var got []string
	for _, m := range s.Models {
		got = append(got, m.Model)
	}
	if want := []string{"m", "m (fast)", "m-20251001", "x-unpriced"}; !slices.Equal(got, want) {
		t.Errorf("models %v, want %v", got, want)
	}
	if m := s.Models[0]; m.Requests != 2 || math.Abs(m.USD-6.60) > 1e-9 || !m.Priced {
		t.Errorf("m %+v, want 2 requests at $6.60", m)
	}
	if m := s.Models[3]; m.Priced || m.USD != 0 || m.Tokens.Input != 500_000 {
		t.Errorf("x-unpriced %+v", m)
	}
}

func TestReadSpendWithoutTheDirectory(t *testing.T) {
	if _, err := ReadSpend(filepath.Join(t.TempDir(), "none"), time.Time{}, time.Now(), spendPrices); err == nil {
		t.Error("no error for a missing projects directory")
	}
}
