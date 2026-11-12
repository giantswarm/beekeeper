//go:build scenario

package scenario

import (
	"strings"
	"testing"
	"time"
)

// advanceScenario is a scenario whose second step is the advance step
// advance (a YAML flow mapping or scalar).
func advanceScenario(advance string) string {
	return "name: x\nflow: clock\nwhen:\n  - {name: loop, background: {args: [supervisor, loop]}}\n  - advance: " + advance + "\nthen:\n  - calls: {min: 0}\n"
}

func TestParseAdvance(t *testing.T) {
	s, err := Parse("short.yaml", []byte(advanceScenario("5m")))
	if err != nil {
		t.Fatal(err)
	}
	if a := s.When[1].Advance; s.When[1].Kind() != KindAdvance || a.By.Duration != 5*time.Minute || a.Step.Duration != 0 || a.Settle != nil {
		t.Fatalf("advance: 5m read as %s %+v", s.When[1].Kind(), a)
	}
	s, err = Parse("long.yaml", []byte(advanceScenario("{by: 1h, step: 5m, settle: {timeout: 2s, interval: 20ms, expect: [{calls: {program: gh, min: 1}}, {process: {name: loop, running: true}}]}}")))
	if err != nil {
		t.Fatal(err)
	}
	a := s.When[1].Advance
	if a.By.Duration != time.Hour || a.Step.Duration != 5*time.Minute || a.Settle == nil || a.Settle.Timeout.Duration != 2*time.Second || len(a.Settle.Expect) != 2 {
		t.Fatalf("the long form read as %+v", a)
	}
	if got := a.String(); got != "by 1h0m0s in steps of 5m0s" {
		t.Fatalf("String() = %q", got)
	}
	s, err = Parse("quiet.yaml", []byte(advanceScenario("{by: 30m, step: 6m, quiet: 100ms}")))
	if err != nil {
		t.Fatal(err)
	}
	if a := s.When[1].Advance; a.Quiet.Duration != 100*time.Millisecond || a.String() != "by 30m0s in steps of 6m0s, quiet 100ms" {
		t.Fatalf("quiet read as %+v (%s)", a, a.String())
	}
	s, err = Parse("start.yaml", []byte("given:\n  clock: {start: 2001-02-03T04:05:06Z}\n"+advanceScenario("1m")))
	if err != nil {
		t.Fatal(err)
	}
	if start, err := s.Given.Clock.StartTime(); err != nil || !start.Equal(time.Date(2001, 2, 3, 4, 5, 6, 0, time.UTC)) {
		t.Fatalf("given.clock.start read as %s, %v", start, err)
	}
}

// The advance step is as strict as the rest of the loader, also inside its
// own mapping, which a custom unmarshaler would otherwise read leniently.
func TestParseRejectsAdvance(t *testing.T) {
	for _, tc := range []struct{ name, advance, want string }{
		{"unknown key", "{by: 1h, stpe: 5m}", "field stpe not found"},
		{"unknown key in a settle expectation", "{by: 1h, settle: {timeout: 1s, expect: [{calls: {progam: gh, min: 1}}]}}", "field progam not found"},
		{"no by", "{step: 5m}", "advance without by"},
		{"negative", "-5m", "must be positive"},
		{"step beyond by", "{by: 5m, step: 1h}", "exceeds its by"},
		{"too many steps", "{by: 1000h, step: 1s}", "more than 10000 steps"},
		{"quiet too long", "{by: 1h, quiet: 6s}", "advance quiet 6s exceeds 5s"},
		{"unbounded settle", "{by: 1h, settle: {expect: [{calls: {min: 1}}]}}", "settle without a timeout is unbounded"},
		{"settle beyond the scenario", "{by: 1h, settle: {timeout: 2m, expect: [{calls: {min: 1}}]}}", "exceeds the scenario's"},
		{"settle without expectations", "{by: 1h, settle: {timeout: 1s}}", "settle without expectations"},
		{"settle on what it does not observe", "{by: 1h, settle: {timeout: 1s, expect: [{exit: 0}]}}", "exit is not observed for a wait_for step"},
		{"settle on an unknown process", "{by: 1h, settle: {timeout: 1s, expect: [{process: {name: watch, running: true}}]}}", "names no earlier background step"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse(tc.name+".yaml", []byte(advanceScenario(tc.advance)))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %v, want one containing %q", err, tc.want)
			}
		})
	}
	_, err := Parse("bad-start.yaml", []byte("given:\n  clock: {start: tomorrow}\n"+advanceScenario("1m")))
	if err == nil || !strings.Contains(err.Error(), "given.clock.start") {
		t.Fatalf("a bad start: %v", err)
	}
	_, err = Parse("advance-and-run.yaml", []byte(strings.Replace(advanceScenario("1m"), "  - advance: 1m", "  - {advance: 1m, run: {args: [version]}}", 1)))
	if err == nil || !strings.Contains(err.Error(), "exactly one of run, background, stop, wait_for, advance, host, github and fault") {
		t.Fatalf("an advance with a run: %v", err)
	}
}
