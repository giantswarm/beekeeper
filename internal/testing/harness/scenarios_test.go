package harness

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/klog/v2"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
)

var (
	flagConcept = flag.String("scenario.concept", "", "run only the scenarios of this concept")
	flagTag     = flag.String("scenario.tag", "", "run only the scenarios with this tag")
	flagPool    = flag.Int("scenario.pool", envInt("BEEKEEPER_SCENARIO_POOL", 2), "k8senv API servers shared by the scenarios ($BEEKEEPER_SCENARIO_POOL; CI: 4–8); run as many scenarios at once with -parallel")
	flagPhases  = flag.Bool("scenario.phases", false, "log each scenario's set-up phases")
	flagLogs    = flag.Bool("scenario.logs", false, "log the engine's output of passing scenarios too")
)

var (
	envOnce sync.Once
	env     *Env
	envErr  error
)

func TestMain(m *testing.M) {
	flag.Parse()
	ctrllog.SetLogger(logr.Discard())
	klog.SetLogger(logr.Discard())
	klog.SetOutput(io.Discard)
	code := m.Run()
	if env != nil {
		env.Shutdown()
	}
	os.Exit(code)
}

// sharedEnv starts the API server pool and Postgres once, on the first scenario run.
func sharedEnv(t *testing.T) *Env {
	t.Helper()
	envOnce.Do(func() {
		pool := max(1, *flagPool)
		t0 := time.Now()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		env, envErr = StartEnv(ctx, EnvOptions{Pool: pool, CRDDir: "../../../config/crd"})
		if envErr == nil {
			t.Logf("environment: %d API servers and Postgres started in %s", pool, time.Since(t0).Round(time.Millisecond))
		}
	})
	if errors.Is(envErr, ErrNoDeps) {
		t.Skipf("scenarios not run: %v", envErr)
	}
	if envErr != nil {
		t.Fatalf("environment: %v", envErr)
	}
	return env
}

type timing struct {
	name, concept string
	setup, total  time.Duration
	budget        time.Duration
	failed        bool
	reason        string
}

// TestScenarios runs every scenario (filtered by -run, -scenario.concept and -scenario.tag), each in
// its own world, in parallel. Scenarios over their time budget are listed at the end.
func TestScenarios(t *testing.T) {
	all, err := LoadDir(scenariosDir)
	if err != nil {
		t.Fatal(err)
	}
	selected := Filter(all, *flagConcept, *flagTag)
	if len(selected) == 0 {
		t.Fatalf("no scenario has concept %q and tag %q", *flagConcept, *flagTag)
	}
	e := sharedEnv(t)

	var mu sync.Mutex
	var timings []timing
	start := time.Now()
	t.Cleanup(func() { report(t, timings, time.Since(start)) })
	for _, sc := range selected {
		sc := sc
		t.Run(sc.Name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
			defer cancel()
			t0 := time.Now()
			record := func(x timing) {
				x.name, x.concept, x.budget, x.total = sc.Name, sc.Concept, sc.Budget(), time.Since(t0)
				mu.Lock()
				timings = append(timings, x)
				mu.Unlock()
			}
			w, err := NewWorld(t, e, sc)
			if err != nil {
				logs := ""
				if w != nil {
					logs = w.Logs.String()
				}
				record(timing{setup: time.Since(t0), failed: true, reason: oneLine("setup: " + err.Error())})
				t.Fatalf("setup: %v%s", err, tail(logs))
			}
			setup := time.Since(t0)
			if *flagPhases {
				t.Logf("set-up: %v", w.Phases)
			}
			res := Run(ctx, w)
			record(timing{setup: setup, failed: res.Failed(), reason: res.Summary()})
			if res.Failed() {
				t.Errorf("%s\n%s%s", sc.Path, res.Report(), tail(w.Logs.String()))
			} else if *flagLogs {
				t.Log(w.Logs.String())
			}
		})
	}
}

func envInt(name string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(name)); err == nil && v > 0 {
		return v
	}
	return def
}

func tail(logs string) string {
	if logs == "" {
		return ""
	}
	lines := strings.Split(logs, "\n")
	if len(lines) > 60 {
		lines = lines[len(lines)-60:]
	}
	return "\nlast engine log lines:\n  " + strings.Join(lines, "\n  ")
}

// report logs the suite's timings and every scenario over its budget (contract §3.4).
func report(t *testing.T, ts []timing, wall time.Duration) {
	if len(ts) == 0 {
		return
	}
	sort.Slice(ts, func(i, j int) bool { return ts[i].total > ts[j].total })
	var sum, setup time.Duration
	var over []string
	failed := 0
	for _, x := range ts {
		sum += x.total
		setup += x.setup
		if x.failed {
			failed++
		}
		if x.total > x.budget {
			over = append(over, fmt.Sprintf("  %-60s %6s (budget %s, setup %s)", x.name, x.total.Round(time.Millisecond), x.budget, x.setup.Round(time.Millisecond)))
		}
	}
	median := ts[len(ts)/2].total
	t.Logf("%d scenarios (%d failed) in %s wall; per scenario: median %s, max %s (%s), mean setup %s",
		len(ts), failed, wall.Round(time.Millisecond), median.Round(time.Millisecond), ts[0].total.Round(time.Millisecond), ts[0].name,
		(setup / time.Duration(len(ts))).Round(time.Millisecond))
	if len(over) > 0 {
		t.Logf("%d scenarios over their budget:\n%s", len(over), strings.Join(over, "\n"))
	}
	if failed > 0 {
		sort.Slice(ts, func(i, j int) bool {
			ci, cj := conceptIndex(ts[i].concept), conceptIndex(ts[j].concept)
			if ci != cj {
				return ci < cj
			}
			return ts[i].name < ts[j].name
		})
		var b strings.Builder
		concept := ""
		for _, x := range ts {
			if !x.failed {
				continue
			}
			if x.concept != concept {
				concept = x.concept
				fmt.Fprintf(&b, "%s:\n", concept)
			}
			fmt.Fprintf(&b, "  %s: %s\n", x.name, x.reason)
		}
		t.Logf("failures by concept:\n%s", b.String())
	}
}

func conceptIndex(c string) int {
	for i, x := range Concepts {
		if x == c {
			return i
		}
	}
	return len(Concepts)
}
