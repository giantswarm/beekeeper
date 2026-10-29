package codex

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeRollout(t *testing.T, dir, rel string, mod time.Time, lines ...string) string {
	t.Helper()
	p := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		t.Fatal(err)
	}
	body := strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if !mod.IsZero() {
		if err := os.Chtimes(p, mod, mod); err != nil {
			t.Fatal(err)
		}
	}
	return p
}

func TestReadUsageDualWindowAndProSingleWindow(t *testing.T) {
	t.Run("dual window 5h primary and 7d secondary", func(t *testing.T) {
		stateDir := t.TempDir()
		sessDir := filepath.Join(t.TempDir(), "sessions")
		t0 := time.Date(2026, 10, 28, 14, 20, 0, 0, time.UTC)

		writeRollout(t, sessDir, "2026/10/28/rollout-plus.jsonl", t0,
			`{"timestamp":"2026-10-28T14:19:00Z","type":"session_meta","payload":{"id":"t-plus","cwd":"/work"}}`,
			`{"timestamp":"2026-10-28T14:20:00Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":100,"output_tokens":50}},"rate_limits":{"limit_id":"codex","primary":{"used_percent":18.5,"window_minutes":300,"resets_at":1793215200},"secondary":{"used_percent":42.0,"window_minutes":10080,"resets_at":1793726400},"credits":{"has_credits":true,"unlimited":false,"balance":25.5},"plan_type":"plus","rate_limit_reached_type":null,"spend_control_reached":false}}}`,
		)

		r, err := ReadUsage(stateDir, sessDir)
		if err != nil {
			t.Fatalf("ReadUsage: %v", err)
		}
		if !r.At.Equal(t0) {
			t.Fatalf("At = %v, want %v", r.At, t0)
		}
		if r.PlanType != "plus" {
			t.Fatalf("PlanType = %q, want plus", r.PlanType)
		}
		if r.FiveHour == nil || r.FiveHour.UsedPercent != 18.5 || r.FiveHour.WindowMinutes != 300 || !r.FiveHour.ResetsAt.Equal(time.Unix(1793215200, 0).UTC()) {
			t.Fatalf("FiveHour = %+v", r.FiveHour)
		}
		if r.SevenDay == nil || r.SevenDay.UsedPercent != 42.0 || r.SevenDay.WindowMinutes != 10080 || !r.SevenDay.ResetsAt.Equal(time.Unix(1793726400, 0).UTC()) {
			t.Fatalf("SevenDay = %+v", r.SevenDay)
		}
		if r.Credits == nil || !r.Credits.HasCredits || r.Credits.Balance == nil || *r.Credits.Balance != 25.5 {
			t.Fatalf("Credits = %+v", r.Credits)
		}
	})

	t.Run("pro plan single weekly window in primary with null secondary", func(t *testing.T) {
		stateDir := t.TempDir()
		sessDir := filepath.Join(t.TempDir(), "sessions")
		t0 := time.Date(2026, 10, 28, 15, 0, 0, 0, time.UTC)

		writeRollout(t, HomeSessionsDir(stateDir), "2026/10/28/rollout-pro.jsonl", t0,
			`{"timestamp":"2026-10-28T14:59:00Z","type":"session_meta","payload":{"id":"t-pro","cwd":"/work"}}`,
			`{"timestamp":"2026-10-28T15:00:00Z","type":"event_msg","payload":{"type":"token_count","rate_limits":{"primary":{"used_percent":14.2,"window_minutes":10080,"resets_at":1793726400},"secondary":null,"credits":null,"plan_type":"pro","rate_limit_reached_type":null,"spend_control_reached":false}}}`,
		)

		r, err := ReadUsage(stateDir, sessDir)
		if err != nil {
			t.Fatalf("ReadUsage: %v", err)
		}
		if r.FiveHour != nil {
			t.Fatalf("expected nil FiveHour on Pro single-window reading, got %+v", r.FiveHour)
		}
		if r.SevenDay == nil || r.SevenDay.UsedPercent != 14.2 || r.SevenDay.WindowMinutes != 10080 {
			t.Fatalf("SevenDay = %+v", r.SevenDay)
		}
		if r.PlanType != "pro" {
			t.Fatalf("PlanType = %q, want pro", r.PlanType)
		}
	})
}

func TestReadUsageSkipsMetadataOnlyNewestSessionAndPicksFreshestAcrossSessions(t *testing.T) {
	stateDir := t.TempDir()
	sessDir := filepath.Join(t.TempDir(), "sessions")
	tOld := time.Date(2026, 10, 28, 14, 0, 0, 0, time.UTC)
	tFresh := time.Date(2026, 10, 28, 14, 30, 0, 0, time.UTC)
	tJustStarted := time.Date(2026, 10, 28, 14, 35, 0, 0, time.UTC)

	// Session 1: older token_count event, even if touched later.
	writeRollout(t, sessDir, "2026/10/28/rollout-1.jsonl", tJustStarted,
		`{"timestamp":"2026-10-28T14:00:00Z","type":"event_msg","payload":{"type":"token_count","rate_limits":{"primary":{"used_percent":10.0,"window_minutes":300,"resets_at":1793215200},"secondary":{"used_percent":20.0,"window_minutes":10080,"resets_at":1793726400}}}}`,
	)

	// Session 2 (in isolated worker CODEX_HOME): fresher token_count event at 14:30.
	writeRollout(t, HomeSessionsDir(stateDir), "2026/10/28/rollout-2.jsonl", tFresh,
		`{"timestamp":"2026-10-28T14:10:00Z","type":"event_msg","payload":{"type":"token_count","rate_limits":{"primary":{"used_percent":15.0,"window_minutes":300,"resets_at":1793215200},"secondary":{"used_percent":25.0,"window_minutes":10080,"resets_at":1793726400}}}}`,
		`{"timestamp":"2026-10-28T14:30:00Z","type":"event_msg","payload":{"type":"token_count","rate_limits":{"primary":{"used_percent":33.0,"window_minutes":300,"resets_at":1793215200},"secondary":{"used_percent":44.0,"window_minutes":10080,"resets_at":1793726400}}}}`,
	)

	// Session 3: newest-modified rollout file, just started, has only metadata and task_started (no token_count yet).
	writeRollout(t, HomeSessionsDir(stateDir), "2026/10/28/rollout-3.jsonl", tJustStarted.Add(time.Minute),
		`{"timestamp":"2026-10-28T14:35:00Z","type":"session_meta","payload":{"id":"t-new","cwd":"/work"}}`,
		`{"timestamp":"2026-10-28T14:35:01Z","type":"turn_context","payload":{"cwd":"/work","model":"gpt-5.4"}}`,
		`{"timestamp":"2026-10-28T14:35:02Z","type":"event_msg","payload":{"type":"task_started"}}`,
	)

	_ = tOld
	r, err := ReadUsage(stateDir, sessDir)
	if err != nil {
		t.Fatalf("ReadUsage: %v", err)
	}
	if !r.At.Equal(tFresh) {
		t.Fatalf("At = %v, want %v", r.At, tFresh)
	}
	if r.FiveHour == nil || r.FiveHour.UsedPercent != 33.0 {
		t.Fatalf("FiveHour = %+v, want 33.0%%", r.FiveHour)
	}
	if r.SevenDay == nil || r.SevenDay.UsedPercent != 44.0 {
		t.Fatalf("SevenDay = %+v, want 44.0%%", r.SevenDay)
	}
}

func TestReadUsageFailsClosedOnExplicitInvalidOrStoppedFreshestEvent(t *testing.T) {
	cases := []struct {
		name    string
		line    string
		wantSub string
	}{
		{
			name:    "spend_control_reached",
			line:    `{"timestamp":"2026-10-28T15:00:00Z","type":"event_msg","payload":{"type":"token_count","rate_limits":{"primary":{"used_percent":50,"window_minutes":300,"resets_at":1793215200},"spend_control_reached":true}}}`,
			wantSub: "spend control reached",
		},
		{
			name:    "rate_limit_reached_type",
			line:    `{"timestamp":"2026-10-28T15:00:00Z","type":"event_msg","payload":{"type":"token_count","rate_limits":{"primary":{"used_percent":100,"window_minutes":300,"resets_at":1793215200},"rate_limit_reached_type":"five_hour"}}}`,
			wantSub: "rate limit reached (five_hour)",
		},
		{
			name:    "null rate_limits in token_count",
			line:    `{"timestamp":"2026-10-28T15:00:00Z","type":"event_msg","payload":{"type":"token_count","rate_limits":null}}`,
			wantSub: "null rate_limits",
		},
		{
			name:    "both primary and secondary null",
			line:    `{"timestamp":"2026-10-28T15:00:00Z","type":"event_msg","payload":{"type":"token_count","rate_limits":{"primary":null,"secondary":null}}}`,
			wantSub: "neither primary nor secondary",
		},
		{
			name:    "invalid used_percent",
			line:    `{"timestamp":"2026-10-28T15:00:00Z","type":"event_msg","payload":{"type":"token_count","rate_limits":{"primary":{"used_percent":120,"window_minutes":300}}}}`,
			wantSub: "used_percent 120 is invalid",
		},
		{
			name:    "corrupt token_count JSON line",
			line:    `{"timestamp":"2026-10-28T15:00:00Z","type":"event_msg","payload":{"type":"token_count","rate_limits":{BROKEN`,
			wantSub: "invalid codex token_count",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stateDir := t.TempDir()
			sessDir := filepath.Join(t.TempDir(), "sessions")
			tValid := time.Date(2026, 10, 28, 14, 30, 0, 0, time.UTC)
			tBroken := time.Date(2026, 10, 28, 15, 0, 0, 0, time.UTC)

			// Older session has a valid reading.
			writeRollout(t, sessDir, "2026/10/28/rollout-valid.jsonl", tValid,
				`{"timestamp":"2026-10-28T14:30:00Z","type":"event_msg","payload":{"type":"token_count","rate_limits":{"primary":{"used_percent":20,"window_minutes":300,"resets_at":1793215200},"secondary":{"used_percent":30,"window_minutes":10080,"resets_at":1793726400}}}}`,
			)
			// Newer session has an explicit invalid or stopped token_count reading.
			writeRollout(t, HomeSessionsDir(stateDir), "2026/10/28/rollout-bad.jsonl", tBroken,
				`{"timestamp":"2026-10-28T14:59:00Z","type":"session_meta","payload":{"id":"t-bad","cwd":"/work"}}`,
				tc.line,
			)

			_, err := ReadUsage(stateDir, sessDir)
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantSub)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("err = %v, want substring %q", err, tc.wantSub)
			}
		})
	}
}

func TestReadUsageFailsClosedWhenNoTokenCountExists(t *testing.T) {
	stateDir := t.TempDir()
	sessDir := filepath.Join(t.TempDir(), "sessions")

	if _, err := ReadUsage(stateDir, sessDir); err == nil {
		t.Fatal("expected error when no rollout files exist")
	}

	writeRollout(t, sessDir, "2026/10/28/rollout-meta.jsonl", time.Now(),
		`{"timestamp":"2026-10-28T14:35:00Z","type":"session_meta","payload":{"id":"t-meta","cwd":"/work"}}`,
	)
	if _, err := ReadUsage(stateDir, sessDir); err == nil {
		t.Fatal("expected error when rollout files contain no token_count event")
	}
}

func TestReadUsageReviewR1Regressions(t *testing.T) {
	t.Run("truncated or corrupt rollout line beats older valid session even when session_meta is older", func(t *testing.T) {
		tMetaOld := time.Date(2026, 10, 28, 14, 0, 0, 0, time.UTC)
		tValid := time.Date(2026, 10, 28, 14, 30, 0, 0, time.UTC)
		tCorrupt := time.Date(2026, 10, 28, 15, 0, 0, 0, time.UTC)

		cases := []struct {
			name string
			line string
		}{
			{
				name: "truncated before token_count with timestamp prefix",
				line: `{"timestamp":"2026-10-28T15:00:00Z","type":"event_m`,
			},
			{
				name: "truncated inside token_count with timestamp prefix",
				line: `{"timestamp":"2026-10-28T15:00:00Z","type":"event_msg","payload":{"type":"token_count","rate_limits":{BROKEN`,
			},
			{
				name: "truncated inside timestamp falls back to max(lastLineAt, mod)",
				line: `{"timestamp":"2026-10-28T15:0`,
			},
			{
				name: "scanner ErrTooLong on oversized line",
				line: `{"timestamp":"2026-10-28T15:00:00Z","type":"response_item","payload":"` + strings.Repeat("x", maxLine+1024),
			},
		}

		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				stateDir := t.TempDir()
				sessDir := filepath.Join(t.TempDir(), "sessions")

				// Session 1 has a valid token_count at 14:30.
				writeRollout(t, sessDir, "2026/10/28/rollout-valid.jsonl", tValid,
					`{"timestamp":"2026-10-28T14:30:00Z","type":"event_msg","payload":{"type":"token_count","rate_limits":{"primary":{"used_percent":20,"window_minutes":300,"resets_at":1793215200},"secondary":{"used_percent":30,"window_minutes":10080,"resets_at":1793726400}}}}`,
				)

				// Session 2 started earlier (session_meta at 14:00 < 14:30), then wrote a truncated/corrupt tail at 15:00.
				_ = tMetaOld
				writeRollout(t, HomeSessionsDir(stateDir), "2026/10/28/rollout-corrupt.jsonl", tCorrupt,
					`{"timestamp":"2026-10-28T14:00:00Z","type":"session_meta","payload":{"id":"t-corrupt","cwd":"/work"}}`,
					tc.line,
				)

				if _, err := ReadUsage(stateDir, sessDir); err == nil {
					t.Fatalf("expected ReadUsage to fail closed on %s, got nil", tc.name)
				}
			})
		}
	})

	t.Run("window_minutes, resets_at, and 7-day window validation", func(t *testing.T) {
		tEvent := time.Date(2026, 10, 28, 15, 0, 0, 0, time.UTC)
		cases := []struct {
			name    string
			line    string
			wantSub string
		}{
			{
				name:    "missing window_minutes",
				line:    `{"timestamp":"2026-10-28T15:00:00Z","type":"event_msg","payload":{"type":"token_count","rate_limits":{"primary":{"used_percent":10,"resets_at":1793215200},"secondary":{"used_percent":20,"window_minutes":10080,"resets_at":1793726400}}}}`,
				wantSub: "primary.window_minutes is missing",
			},
			{
				name:    "non-weekly window_minutes 43200 (free plan monthly)",
				line:    `{"timestamp":"2026-10-28T15:00:00Z","type":"event_msg","payload":{"type":"token_count","rate_limits":{"primary":{"used_percent":10,"window_minutes":43200,"resets_at":1793726400},"secondary":null}}}`,
				wantSub: "window_minutes 43200 is unsupported",
			},
			{
				name:    "missing resets_at on 7-day window",
				line:    `{"timestamp":"2026-10-28T15:00:00Z","type":"event_msg","payload":{"type":"token_count","rate_limits":{"primary":{"used_percent":10,"window_minutes":10080},"secondary":null}}}`,
				wantSub: "primary.resets_at is missing",
			},
			{
				name:    "expired resets_at at or before event timestamp",
				line:    `{"timestamp":"2026-10-28T15:00:00Z","type":"event_msg","payload":{"type":"token_count","rate_limits":{"primary":{"used_percent":99,"window_minutes":10080,"resets_at":"2026-10-28T14:59:00Z"},"secondary":null}}}`,
				wantSub: "is not after event timestamp",
			},
			{
				name:    "resets_at exceeds window_minutes from event timestamp",
				line:    `{"timestamp":"2026-10-28T15:00:00Z","type":"event_msg","payload":{"type":"token_count","rate_limits":{"primary":{"used_percent":10,"window_minutes":300,"resets_at":"2026-10-29T00:00:00Z"},"secondary":{"used_percent":20,"window_minutes":10080,"resets_at":1793726400}}}}`,
				wantSub: "exceeds 300m window",
			},
			{
				name:    "5h window only without 7d window",
				line:    `{"timestamp":"2026-10-28T15:00:00Z","type":"event_msg","payload":{"type":"token_count","rate_limits":{"primary":{"used_percent":10,"window_minutes":300,"resets_at":1793215200},"secondary":null}}}`,
				wantSub: "omits 7-day window",
			},
		}

		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				stateDir := t.TempDir()
				sessDir := filepath.Join(t.TempDir(), "sessions")
				writeRollout(t, sessDir, "2026/10/28/rollout.jsonl", tEvent, tc.line)
				_, err := ReadUsage(stateDir, sessDir)
				if err == nil || !strings.Contains(err.Error(), tc.wantSub) {
					t.Fatalf("err = %v, want substring %q", err, tc.wantSub)
				}
			})
		}
	})

	t.Run("bounded rollout scanning: stops before historical files and seeks last 4 MiB tail", func(t *testing.T) {
		stateDir := t.TempDir()
		sessDir := filepath.Join(t.TempDir(), "sessions")
		tOld := time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC)
		tFresh := time.Date(2026, 10, 28, 15, 0, 0, 0, time.UTC)

		// Old historical rollout file (mod = Oct 20) contains corrupt JSON that would fail if scanned,
		// proving ReadUsage sorts by mod descending and stops once rf.mod < best.at - rolloutModSlack.
		writeRollout(t, sessDir, "2026/10/20/rollout-ancient.jsonl", tOld, `{CORRUPT_HISTORICAL`)

		// Fresh rollout file > 4 MiB (4.5 MiB of response_item lines followed by a valid token_count event).
		largePath := filepath.Join(HomeSessionsDir(stateDir), "2026/10/28/rollout-large.jsonl")
		if err := os.MkdirAll(filepath.Dir(largePath), 0o750); err != nil {
			t.Fatal(err)
		}
		f, err := os.OpenFile(largePath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600) //nolint:gosec // test rollout file
		if err != nil {
			t.Fatal(err)
		}
		padLine := `{"timestamp":"2026-10-28T14:50:00Z","type":"response_item","payload":{"type":"message","text":"` + strings.Repeat("a", 4000) + `"}}` + "\n"
		for written := 0; written < maxRolloutTailBytes+512*1024; written += len(padLine) {
			if _, werr := f.WriteString(padLine); werr != nil {
				_ = f.Close()
				t.Fatal(werr)
			}
		}
		tailEvent := `{"timestamp":"2026-10-28T15:00:00Z","type":"event_msg","payload":{"type":"token_count","rate_limits":{"primary":{"used_percent":22.5,"window_minutes":300,"resets_at":1793215200},"secondary":{"used_percent":48.0,"window_minutes":10080,"resets_at":1793726400},"plan_type":"plus"}}}` + "\n"
		if _, werr := f.WriteString(tailEvent); werr != nil {
			_ = f.Close()
			t.Fatal(werr)
		}
		if cerr := f.Close(); cerr != nil {
			t.Fatal(cerr)
		}
		if err := os.Chtimes(largePath, tFresh, tFresh); err != nil {
			t.Fatal(err)
		}

		r, err := ReadUsage(stateDir, sessDir)
		if err != nil {
			t.Fatalf("ReadUsage on >4 MiB rollout with historical files: %v", err)
		}
		if r.FiveHour == nil || r.FiveHour.UsedPercent != 22.5 || r.SevenDay == nil || r.SevenDay.UsedPercent != 48.0 {
			t.Fatalf("unexpected reading from >4 MiB tail: %+v / %+v", r.FiveHour, r.SevenDay)
		}
	})
}
