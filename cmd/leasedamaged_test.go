package cmd

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/lease"
)

// A lease whose holder record cannot be read stays held and named, and
// fails nothing else: another resource is claimed, the damaged one's grant
// does not expire, lease list does not call it free, a plain release says
// how to free it and a forced release frees it.
func TestLeaseDamagedRecordIsScopedAndForceReleased(t *testing.T) {
	a := claimApp(t, claimGrant(labTwo, claimFirst, 2*time.Hour))
	dir := lease.Dir(a.cfg.LeaseDir)
	if err := os.Mkdir(filepath.Join(a.cfg.LeaseDir, labTwo), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(a.cfg.LeaseDir, labTwo, "holder.json"), []byte(`{"env":`), 0o600); err != nil {
		t.Fatal(err)
	}

	if out, err := runLease(a, "claim", graveler, "-p", "my proof"); err != nil || out != "held by you since 02:00: claimed graveler\n" {
		t.Errorf("claim of another resource next to a damaged lease: %q, %v", out, err)
	}
	if got := grantees(t, a, labTwo); !slices.Equal(got, []string{claimFirst.Name}) {
		t.Errorf("the damaged lease's grant expired as if it were free: %q", got)
	}
	out, err := runLease(a, "list")
	if err != nil || !strings.Contains(out, "unreadable holder record") {
		t.Errorf("lease list does not show the damaged lease: %q, %v", out, err)
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "free:") && strings.Contains(line, labTwo) {
			t.Errorf("lease list calls the damaged lease free: %q", line)
		}
	}

	out, err = runLease(a, "release", labTwo)
	if Code(err) != ExitRefused || !strings.Contains(out+errText(err), "release "+labTwo+" --force") {
		t.Errorf("plain release of a damaged lease: %q, %v (exit %d)", out, err, Code(err))
	}
	if out, err := runLease(a, "release", labTwo, "--force"); err != nil || !strings.HasPrefix(out, "released "+labTwo) {
		t.Errorf("forced release of a damaged lease: %q, %v", out, err)
	}
	if h, err := dir.Get(labTwo); err != nil || h != nil {
		t.Errorf("after the forced release: %+v, %v", h, err)
	}
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
