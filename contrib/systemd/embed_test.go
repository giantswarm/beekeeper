package systemd

import (
	"strings"
	"testing"
)

func TestStandbyService(t *testing.T) {
	if got := StandbyService(true); got != NotifyService || !strings.Contains(got, "\nExecStart="+NotifyServiceExe+notifyWatch+"\n") {
		t.Errorf("with notify: not the shipped unit:\n%s", got)
	}
	off := StandbyService(false)
	if !strings.HasPrefix(off, "# beekeeper watch --standby as a systemd user unit") ||
		!strings.Contains(off, "\nExecStart="+NotifyServiceExe+" watch --standby\n") || strings.Contains(off, notifyWatch) {
		t.Errorf("without notify: the header or the watch still notifies:\n%s", off)
	}
	if len(NotifyService)-len(off) != 2*len(" --notify") {
		t.Errorf("without notify: more than the header's and ExecStart's flag changed:\n%s", off)
	}
}
