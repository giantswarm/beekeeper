package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSchedulerIntervalsRejectNegative(t *testing.T) {
	for _, key := range []string{"watch.interval", "alerts.every", "alerts.pagerduty.every", "supervisor.loopInterval"} {
		t.Run(key, func(t *testing.T) {
			p := writeConfig(t, "")
			raw, err := apply(nil, []Setting{{Key: key, Value: "-1s"}}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p, raw, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(p); err == nil || !strings.Contains(err.Error(), key) {
				t.Errorf("negative scheduler interval loaded: %v", err)
			}
			if findings, err := Check(p); err != nil || len(findings) != 1 || !strings.HasPrefix(findings[0], key+":") {
				t.Errorf("negative scheduler interval checked: %v, %v", findings, err)
			}
			const original = "grantTTL: 20m\n"
			if err := os.WriteFile(p, []byte(original), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := Set(p, []Setting{{Key: key, Value: "-1s"}}); !errors.Is(err, ErrInvalid) {
				t.Errorf("negative scheduler edit accepted: %v", err)
			}
			if raw, err := os.ReadFile(filepath.Clean(p)); err != nil || string(raw) != original {
				t.Errorf("refused edit changed file: %v", err)
			}
		})
	}
}

func TestSchedulerIntervalsKeepDefaultsAndDisableSentinels(t *testing.T) {
	for _, value := range []string{"0s", "1s"} {
		raw := fmt.Sprintf("watch: {interval: %s, batchInterval: -1s}\nalerts: {every: %s, pagerduty: {every: %s}}\nsupervisor: {loopInterval: %s}\ndoctor: {freeEvery: -1s}\n", value, value, value, value)
		c, err := Load(writeConfig(t, raw))
		if err != nil {
			t.Fatal(err)
		}
		if c.Watch.Interval.Duration <= 0 || c.Alerts.Every.Duration <= 0 || c.Alerts.PagerDuty.Every.Duration <= 0 || c.Supervisor.LoopInterval.Duration <= 0 {
			t.Fatal("scheduler interval is not positive after defaulting")
		}
		if c.Watch.BatchInterval.Duration >= 0 || c.Doctor.FreeEvery.Duration >= 0 {
			t.Fatal("documented negative disable sentinel was changed")
		}
	}
}
