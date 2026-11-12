//go:build scenario && linux

package scenario

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/giantswarm/beekeeper/internal/clock"
)

// clockSocket is the world's clock, named to every process by clock.Env.
const clockSocket = workDir + "/clock.sock"

// startClock serves the world's clock at the scenario's start time.
func (d *driver) startClock() error {
	start, err := d.s.Given.Clock.StartTime()
	if err != nil {
		return err
	}
	d.clock = clock.NewServer(start)
	return d.clock.Serve(clockSocket)
}

// advanceRecord is one step of an advance in the scenario's clock.jsonl:
// the real time it took to move the clock, and the waits for a quiet desk
// before it.
type advanceRecord struct {
	Step  int    `json:"step"`
	Moved string `json:"moved"`
	clock.Advanced
	Real  time.Duration `json:"real"`
	Quiet time.Duration `json:"quiet,omitempty"`
	Error string        `json:"error,omitempty"`
}

// advance moves the world's clock as a moves it, step by step, settling
// after each step; it returns what the report names it by and the
// failures.
func (d *driver) advance(ctx context.Context, a *Advance) (args, failures []string) {
	args = []string{a.String()}
	if d.clock == nil {
		return args, []string{errNoClock.Error()}
	}
	log, err := os.OpenFile(filepath.Join(artifactsDir, "clock.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return args, []string{err.Error()}
	}
	defer func() { _ = log.Close() }()
	enc := json.NewEncoder(log)
	step := a.Step.Duration
	if step == 0 {
		step = a.By.Duration
	}
	var moved time.Duration
	var fired int
	for n := 1; moved < a.By.Duration; n++ {
		if ctx.Err() != nil {
			return args, []string{fmt.Sprintf("the scenario timed out after %s of the advance %s", moved, a)}
		}
		inc := min(step, a.By.Duration-moved)
		waited, err := d.quiet(ctx, a.Quiet.Duration)
		if err != nil {
			return args, []string{fmt.Sprintf("before %s of the advance: %v", moved+inc, err)}
		}
		since := len(d.world.Calls())
		started := time.Now()
		res, err := d.clock.Advance(inc)
		moved += inc
		fired += res.Fired
		rec := advanceRecord{Step: n, Moved: moved.String(), Advanced: res, Real: time.Since(started), Quiet: waited}
		if err != nil {
			rec.Error = err.Error()
		}
		_ = enc.Encode(rec)
		if err != nil {
			return args, []string{fmt.Sprintf("at %s of the advance: %v", moved, err)}
		}
		if a.Settle != nil {
			if f := d.settle(ctx, a.Settle, since); len(f) > 0 {
				for _, line := range f {
					failures = append(failures, fmt.Sprintf("settling after %s of the advance: %s", moved, line))
				}
				return args, failures
			}
		}
	}
	if _, err := d.quiet(ctx, a.Quiet.Duration); err != nil {
		return args, []string{fmt.Sprintf("after the advance: %v", err)}
	}
	args = append(args, "now "+d.clock.Now().UTC().Format(time.RFC3339), fmt.Sprintf("fired %d", fired))
	return args, nil
}

// quiet waits until no process has armed a timer on the world's clock and
// no fake was called for q (none for zero), at most maxQuietWait; it
// returns how long it waited. A call is seen when the world's log grows.
func (d *driver) quiet(ctx context.Context, q time.Duration) (time.Duration, error) {
	if q <= 0 {
		return 0, nil
	}
	started := time.Now()
	calls, called := len(d.world.Calls()), started
	for {
		now := time.Now()
		if n := len(d.world.Calls()); n != calls {
			calls, called = n, now
		}
		last := called
		if armed := d.clock.LastArmed(); armed.After(last) {
			last = armed
		}
		if now.Sub(last) >= q {
			return now.Sub(started), nil
		}
		if now.Sub(started) > maxQuietWait {
			return now.Sub(started), fmt.Errorf("the desk was not quiet for %s within %s: a timer was armed or a fake called %s ago", q, maxQuietWait, now.Sub(last).Round(time.Millisecond))
		}
		select {
		case <-ctx.Done():
			return now.Sub(started), ctx.Err()
		case <-time.After(min(q-now.Sub(last), 5*time.Millisecond)):
		}
	}
}

// settle polls w's expectations until all hold, its calls counted from the
// world's call since on.
func (d *driver) settle(ctx context.Context, w *WaitFor, since int) []string {
	interval := w.Interval.Duration
	if interval == 0 {
		interval = 100 * time.Millisecond
	}
	deadline := time.Now().Add(w.Timeout.Duration)
	for {
		v := d.globals(View{Kind: KindWaitFor})
		v.Calls = d.callsSince(since)
		var failures []string
		for j, e := range w.Expect {
			if err := Check(e, v); err != nil {
				failures = append(failures, fmt.Sprintf("settle[%d] %s: %v", j, e.Kind(), err))
			}
		}
		if len(failures) == 0 {
			return nil
		}
		if time.Now().Add(interval).After(deadline) || ctx.Err() != nil {
			return append(failures, fmt.Sprintf("still failing after %s", w.Timeout))
		}
		select {
		case <-ctx.Done():
		case <-time.After(interval):
		}
	}
}

// callsSince counts the world's calls after the first since, as
// World.Count counts them all.
func (d *driver) callsSince(since int) func(program string, args []string) (int, error) {
	return func(program string, args []string) (int, error) {
		pats, err := compileArgs(args)
		if err != nil {
			return 0, err
		}
		n := 0
		for _, c := range d.world.Calls() {
			if c.Seq > since && (program == "" || c.Program == program) && matchArgs(pats, c.Args) {
				n++
			}
		}
		return n, nil
	}
}
