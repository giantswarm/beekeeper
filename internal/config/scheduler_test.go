package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSchedulerIntervals(t *testing.T) {
	for _, tc := range []struct {
		key          string
		raw          string
		get          func(*Config) time.Duration
		defaultValue time.Duration
	}{
		{"watch.interval", "watch: {interval: VALUE}", func(c *Config) time.Duration { return c.Watch.Interval.Duration }, 30 * time.Second},
		{"alerts.every", "alerts: {every: VALUE}", func(c *Config) time.Duration { return c.Alerts.Every.Duration }, 5 * time.Minute},
		{"alerts.pagerduty.every", "alerts: {pagerduty: {every: VALUE}}", func(c *Config) time.Duration { return c.Alerts.PagerDuty.Every.Duration }, time.Minute},
		{"supervisor.loopInterval", "supervisor: {loopInterval: VALUE}", func(c *Config) time.Duration { return c.Supervisor.LoopInterval.Duration }, 30 * time.Second},
	} {
		t.Run(tc.key, func(t *testing.T) {
			for _, value := range []string{"-1s", "0s", "2s"} {
				t.Run(value, func(t *testing.T) {
					p := writeConfig(t, strings.ReplaceAll(tc.raw, "VALUE", value))
					c, err := Load(p)
					findings, checkErr := Check(p)
					if checkErr != nil {
						t.Fatal(checkErr)
					}
					if value == "-1s" {
						if err == nil || !strings.Contains(err.Error(), tc.key+":") {
							t.Errorf("Load error = %v; want key %s", err, tc.key)
						}
						if len(findings) != 1 || !strings.HasPrefix(findings[0], tc.key+":") {
							t.Errorf("Check = %v; want key %s", findings, tc.key)
						}
					} else {
						if err != nil {
							t.Fatal(err)
						}
						want := 2 * time.Second
						if value == "0s" {
							want = tc.defaultValue
						}
						if got := tc.get(c); got != want {
							t.Errorf("Load interval = %s; want %s", got, want)
						}
						if len(findings) != 0 {
							t.Errorf("Check = %v", findings)
						}
					}
					const original = "# preserve this file\ngrantTTL: 20m\n"
					target := writeConfig(t, original)
					setConfig, setErr := Set(target, []Setting{{tc.key, value}})
					if value == "-1s" {
						if !errors.Is(setErr, ErrInvalid) || !strings.Contains(setErr.Error(), tc.key+":") {
							t.Errorf("Set error = %v; want ErrInvalid and key %s", setErr, tc.key)
						}
						raw, readErr := os.ReadFile(filepath.Clean(target))
						if readErr != nil || string(raw) != original {
							t.Errorf("refused Set changed file: %q, %v", raw, readErr)
						}
					} else {
						if setErr != nil {
							t.Fatal(setErr)
						}
						if tc.get(setConfig) != tc.get(c) {
							t.Errorf("Set interval = %s; Load interval = %s", tc.get(setConfig), tc.get(c))
						}
					}
				})
			}
		})
	}
}

func TestSchedulerValidationPreservesDisableSentinels(t *testing.T) {
	settings := []Setting{
		{"capacity.usage.command", "[echo]"},
		{"watch.batchInterval", "-1s"},
		{"board.tidyEvery", "-1s"},
		{"board.escalateEvery", "-1s"},
		{"doctor.freeEvery", "-1s"},
		{"doctor.worktreesEvery", "-1s"},
		{"capacity.usage.reportEvery", "-1s"},
	}
	p := writeConfig(t, "")
	if _, err := Set(p, settings); err != nil {
		t.Fatal(err)
	}
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	for _, got := range []time.Duration{c.Watch.BatchInterval.Duration, c.Board.TidyEvery.Duration, c.Board.EscalateEvery.Duration, c.Doctor.FreeEvery.Duration, c.Doctor.WorktreesEvery.Duration, c.Capacity.Usage.ReportEvery.Duration} {
		if got != -time.Second {
			t.Errorf("disable sentinel = %s; want -1s", got)
		}
	}
	if findings, err := Check(p); err != nil || len(findings) != 0 {
		t.Errorf("Check = %v, %v", findings, err)
	}
}
