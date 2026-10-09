package systemd

import (
	"strings"
	"testing"
)

func TestStandbyService(t *testing.T) {
	if got := StandbyService(true); got != NotifyService || !strings.Contains(got, notifyExecStart) {
		t.Errorf("with notify: not the shipped unit:\n%s", got)
	}
	off := StandbyService(false)
	if !strings.Contains(off, "\nExecStart="+NotifyServiceExe+" watch --standby\n") || strings.Contains(off, notifyExecStart) {
		t.Errorf("without notify: the watch still notifies:\n%s", off)
	}
	if len(NotifyService)-len(off) != len(" --notify") {
		t.Errorf("without notify: more than the ExecStart flag changed:\n%s", off)
	}
}
