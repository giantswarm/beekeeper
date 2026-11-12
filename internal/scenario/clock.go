//go:build scenario

package scenario

import (
	"bytes"
	"errors"
	"fmt"
	"time"

	"gopkg.in/yaml.v3"
)

// Clock is the world's clock (internal/clock): every process of the
// scenario reads its time, which runs as the system's does and moves
// forward at each advance step.
type Clock struct {
	// Start is the world's time as the scenario starts, RFC 3339; the
	// system's time when empty.
	Start string `yaml:"start"`
}

// StartTime is Start parsed; zero for the system's time.
func (c Clock) StartTime() (time.Time, error) {
	if c.Start == "" {
		return time.Time{}, nil
	}
	return time.Parse(time.RFC3339, c.Start)
}

// maxAdvanceSteps bounds an advance's steps.
const maxAdvanceSteps = 10000

// Advance moves the world's clock forward By and returns once every
// process has fired the timers, tickers and sleeps then due. With Step it
// moves in steps of Step (the last one the rest), and after each step
// Settle, when set, is polled like a wait_for until its expectations hold,
// its calls counted from that step on. With Quiet each step waits, before
// it moves the clock and once more after the last, until the desk has been
// quiet that long: no process armed a timer on the clock and no fake was
// called, so the work a step woke is done and waits on the clock again.
// Written as a duration ("advance: 5m") it is By alone.
type Advance struct {
	By     Duration `yaml:"by"`
	Step   Duration `yaml:"step"`
	Quiet  Duration `yaml:"quiet"`
	Settle *WaitFor `yaml:"settle"`
}

// UnmarshalYAML reads a duration or a mapping, strictly: a custom
// unmarshaler does not inherit the decoder's known-fields check, so the
// mapping is decoded again by a strict decoder of its own.
func (a *Advance) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		*a = Advance{}
		return n.Decode(&a.By)
	}
	raw, err := yaml.Marshal(n)
	if err != nil {
		return err
	}
	type plain Advance
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	var p plain
	if err := dec.Decode(&p); err != nil {
		return fmt.Errorf("line %d: advance: %w", n.Line, err)
	}
	*a = Advance(p)
	return nil
}

// String is the advance as the report names it.
func (a Advance) String() string {
	s := "by " + a.By.String()
	if a.Step.Duration > 0 {
		s += " in steps of " + a.Step.String()
	}
	if a.Quiet.Duration > 0 {
		s += ", quiet " + a.Quiet.String()
	}
	return s
}

// maxQuietWait bounds a wait for a quiet desk.
const maxQuietWait = 10 * time.Second

func (s *Scenario) validateAdvance(where string, a *Advance, background []string) []error {
	var errs []error
	bad := func(format string, args ...any) { errs = append(errs, fmt.Errorf(where+": "+format, args...)) }
	if a.By.Duration == 0 {
		bad("advance without by")
		return errs
	}
	if a.Quiet.Duration > maxQuietWait/2 {
		bad("advance quiet %s exceeds %s", a.Quiet, maxQuietWait/2)
	}
	if a.Step.Duration > a.By.Duration {
		bad("advance step %s exceeds its by %s", a.Step, a.By)
	} else if a.Step.Duration > 0 && a.By.Duration/a.Step.Duration > maxAdvanceSteps {
		bad("advance by %s in steps of %s takes more than %d steps", a.By, a.Step, maxAdvanceSteps)
	}
	if w := a.Settle; w != nil {
		if w.Timeout.Duration == 0 {
			bad("settle without a timeout is unbounded")
		} else if w.Timeout.Duration > s.Timeout.Duration {
			bad("settle timeout %s exceeds the scenario's %s", w.Timeout, s.Timeout)
		}
		if len(w.Expect) == 0 {
			bad("settle without expectations")
		}
		for j, e := range w.Expect {
			errs = append(errs, s.validateExpect(fmt.Sprintf("%s settle[%d]", where, j), e, KindWaitFor, background, false)...)
		}
	}
	return errs
}

// errNoClock is an advance without the world's clock.
var errNoClock = errors.New("the world has no clock")
