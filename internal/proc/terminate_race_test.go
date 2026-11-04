package proc

import (
	"os"
	"syscall"
	"testing"
	"time"
)

func TestTerminateMatchingPinsIdentityBeforeChecks(t *testing.T) {
	for _, phase := range []string{"TERM", "KILL"} {
		t.Run(phase, func(t *testing.T) {
			// Model PID reuse during the caller's lifecycle recheck. An
			// opened handle stays bound to its generation even after exit.
			generation := 1
			start := time.Unix(1700000000, 0)
			opened, closed, checks := 0, 0, 0
			var signals []syscall.Signal
			files := terminationIO{
				started: func(int) (time.Time, error) {
					return start.Add(time.Duration(generation-1) * time.Second), nil
				},
				open: func(int) (processSignals, error) {
					bound := generation
					opened++
					return processSignals{
						signal: func(sig syscall.Signal) error {
							if bound != generation {
								return os.ErrProcessDone
							}
							if bound != 1 {
								t.Errorf("replacement process received %s", sig)
							}
							signals = append(signals, sig)
							return nil
						},
						close: func() { closed++ },
					}, nil
				},
			}
			match := func(*Process) bool {
				checks++
				if phase == "TERM" || checks == 2 {
					generation++
				}
				return true
			}
			sent := terminateMatching([]*Process{{PID: 42, Start: start}}, 0, match, files)
			wantSent := 0
			if phase == "KILL" {
				wantSent = 1
			}
			if len(sent) != wantSent || len(signals) != wantSent {
				t.Errorf("sent=%v signals=%v, want %d original processes signalled", sent, signals, wantSent)
			}
			if opened != closed || opened != checks {
				t.Errorf("handles opened=%d closed=%d checks=%d", opened, closed, checks)
			}
		})
	}
}
