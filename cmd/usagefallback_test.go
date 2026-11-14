package cmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/capacity"
	"github.com/giantswarm/beekeeper/internal/codex"
	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/loop"
	"github.com/giantswarm/beekeeper/internal/state"
)

func TestCodexUnreadableFallback(t *testing.T) {
	for _, providers := range []bool{false, true} {
		for _, chores := range []bool{false, true} {
			for _, saved := range []string{ghNone, "good", "high", "expired", "expired-high"} {
				for _, failure := range []string{"read", "validation"} {
					t.Run(fmt.Sprintf("providers=%t/chores=%t/saved=%s/%s", providers, chores, saved, failure), func(t *testing.T) {
						_, cfgPath := isolateUsageTestEnv(t)
						if err := os.WriteFile(cfgPath, []byte("{}\n"), 0o600); err != nil {
							t.Fatal(err)
						}
						cfg, err := config.Load(cfgPath)
						if err != nil {
							t.Fatal(err)
						}
						store, err := state.OpenVersion(cfg.StateDir, "dev")
						if err != nil {
							t.Fatal(err)
						}
						clk := newFakeClock(t)
						now := clk.Now()
						u := config.Usage{Command: config.DefaultCodexUsageCommand}
						cfg.Capacity = config.Capacity{Ceiling: 3, Usage: u}
						file := usageFile
						if providers {
							cfg.Capacity = config.Capacity{Ceiling: 3, Providers: map[string]config.Provider{
								codex.Harness: {Ceiling: 3, Usage: u},
							}}
							file = usageProviderFile(codex.Harness)
						}
						original := capacity.UsageReading{At: now.Add(-6 * time.Minute), SourceAt: now.Add(-6 * time.Minute),
							SingleWindow: true, SevenDay: &capacity.UsageWindow{UsedPercent: 10, ResetsAt: now.Add(24 * time.Hour)}}
						if strings.Contains(saved, "high") {
							original.SevenDay.UsedPercent = 100
						}
						if strings.HasPrefix(saved, "expired") {
							original.At = now.Add(-24 * time.Hour)
						}
						if saved != ghNone {
							if err := store.WriteFile(file, original); err != nil {
								t.Fatal(err)
							}
						}
						var out bytes.Buffer
						a := &app{cfg: cfg, store: store, now: now, out: &out}
						a.readUsage = func(context.Context, []string, time.Duration) (capacity.UsageReading, error) {
							if failure == "validation" {
								return capacity.UsageReading{SourceAt: now.Add(10 * time.Minute)}, nil
							}
							return capacity.UsageReading{}, errors.New("reader offline")
						}
						w := a.newWatcher(false, chores)
						w.Clock, watchApp(w.Host).now = clk, now
						w.ReadHeadroom = func(context.Context, *capacity.SwapReading) *capacity.Headroom { return roomy() }
						view := w.WatchUsage(t.Context(), &state.State{}, nil)
						want := 3
						if strings.Contains(saved, "high") {
							want = 0
						}
						if view == nil || view.Ceiling != want || !strings.Contains(out.String(), "unreadable") {
							t.Fatalf("view=%+v, want ceiling %d and warning; output=%s", view, want, out.String())
						}
						var persisted capacity.UsageReading
						found, err := store.ReadFile(file, &persisted)
						if err != nil || found != (saved != ghNone) || found && (!persisted.At.Equal(original.At) || persisted.Fallback != "") {
							t.Fatalf("failure replaced last observation: found=%t reading=%+v err=%v", found, persisted, err)
						}
						if chores {
							events, err := store.Follow("test-guide-usage", func(e state.Event) bool { return e.Verb == loop.LoopFileVerb })
							if err != nil || len(events) != 1 || !strings.Contains(events[0].Detail, loop.FileReportSeparator) {
								t.Fatalf("missing durable guide report: %+v, %v", events, err)
							}
							if err := store.Update(func(st *state.State) ([]state.Event, error) {
								st.Guide = &state.Role{Holder: &state.Supervisor{Party: state.Party{Name: "fixture-guide"}, Since: now}}
								return nil, nil
							}); err != nil {
								t.Fatal(err)
							}
							st, err := store.Read()
							if err != nil {
								t.Fatal(err)
							}
							w.GuideMonitor(t.Context(), st, nil)
							st, err = store.Read()
							if err != nil || len(st.GuideWork) != 1 || !strings.Contains(st.GuideWork[0].Text, "unreadable") {
								t.Fatalf("guide did not receive failure: state=%+v err=%v", st, err)
							}
						}
					})
				}
			}
		}
	}
}

func TestCodexStopSurvivesUnreadableRefresh(t *testing.T) {
	for _, file := range []string{usageFile, usageProviderFile(codex.Harness)} {
		t.Run(file, func(t *testing.T) {
			store, err := state.OpenVersion(t.TempDir(), "dev")
			if err != nil {
				t.Fatal(err)
			}
			now := keepNow
			u := config.Usage{Command: config.DefaultCodexUsageCommand}
			healthy := capacity.UsageReading{At: now.Add(-6 * time.Minute), SingleWindow: true,
				SevenDay: &capacity.UsageWindow{UsedPercent: 10, ResetsAt: now.Add(24 * time.Hour)}}
			if err := store.WriteFile(file, healthy); err != nil {
				t.Fatal(err)
			}
			a := &app{store: store, now: now}
			a.readUsage = func(context.Context, []string, time.Duration) (capacity.UsageReading, error) {
				return capacity.UsageReading{}, codex.ErrUsageStopped
			}
			if _, err := a.resolveReading(t.Context(), file, u); !errors.Is(err, codex.ErrUsageStopped) {
				t.Fatalf("stop masked: %v", err)
			}
			a.readUsage = func(context.Context, []string, time.Duration) (capacity.UsageReading, error) {
				return capacity.UsageReading{}, errors.New("transport offline")
			}
			if _, err := a.resolveReading(t.Context(), file, u); err == nil {
				t.Fatal("ordinary failure cleared explicit stop")
			}
			a.readUsage = func(context.Context, []string, time.Duration) (capacity.UsageReading, error) { return healthy, nil }
			if _, err := a.resolveReading(t.Context(), file, u); err != nil {
				t.Fatalf("successful read did not clear stop: %v", err)
			}
		})
	}
}

func TestCodexConfiguredFallbackHonorsQuotaLedger(t *testing.T) {
	store, err := state.OpenVersion(t.TempDir(), "dev")
	if err != nil {
		t.Fatal(err)
	}
	now := keepNow
	if err := store.WriteFile(usageQuotaFile(codex.Harness), usageQuotaLedger{Buckets: map[string]usageQuotaEntry{
		"": {At: now, ResetsAt: now.Add(time.Hour)},
	}}); err != nil {
		t.Fatal(err)
	}
	a := &app{store: store, now: now}
	p := config.Provider{Ceiling: 3, Usage: config.Usage{Command: config.DefaultCodexUsageCommand}}
	r, err := a.cachedUsageReading(usageProviderFile(codex.Harness), p.Usage, now, errors.New("offline"))
	if err != nil {
		t.Fatal(err)
	}
	pv := capacity.BuildSingleProviderView(codex.Harness, p, capacity.ProviderAgents{}, capacity.ProviderReadingResult{Reading: r, OK: true}, now)
	if pv.Ceiling != 0 || pv.Room != 0 || r.Fallback == "" {
		t.Fatalf("configured fallback bypassed quota ledger: %+v", pv)
	}
}
