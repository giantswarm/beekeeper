package fatal

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestFatalRestartDedupAndVerification(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()
	q := Queue{}
	failed := Observation{ID: "harness", Failed: true, Evidence: "cannot start"}
	if q.Observe(failed, now) == "" {
		t.Fatal("no detection")
	}
	if q.Observe(failed, now) != "" || len(q.Incidents) != 1 {
		t.Fatal("duplicated")
	}
	x := q.Reserve(now, func(string) bool { return false })
	if x == nil || x.Attempts != 1 {
		t.Fatal("no reservation")
	}
	if err := q.Save(dir); err != nil {
		t.Fatal(err)
	}
	q, err := Read(dir)
	if err != nil {
		t.Fatal(err)
	}
	if q.Reserve(now.Add(time.Hour), func(string) bool { return true }) != nil {
		t.Fatal("live owner duplicated")
	}
	if q.Observe(Observation{ID: "harness", Unknown: true}, now) != "" || q.Barrier("ordinary") == nil {
		t.Fatal("unknown cleared outage")
	}
	if q.Barrier(x.Owner) != nil {
		t.Fatal("repair blocked")
	}
	if q.Observe(Observation{ID: "harness"}, now.Add(time.Minute)) == "" || q.Barrier("ordinary") != nil {
		t.Fatal("verified recovery did not release barrier")
	}
}

func TestFatalRetryBudgetAndMultipleOutages(t *testing.T) {
	now := time.Now().UTC()
	q := Queue{}
	q.Observe(Observation{ID: "first", Failed: true}, now)
	q.Observe(Observation{ID: "second", Failed: true}, now)
	x := q.Reserve(now, func(string) bool { return false })
	if x == nil || q.Barrier(x.Owner) != nil {
		t.Fatal("repair blocked by second outage")
	}
	if q.Reserve(now, func(name string) bool { return name == x.Owner }) != nil {
		t.Fatal("parallel repair")
	}
	q.Observe(Observation{ID: "second", Disabled: true}, now)
	for i := 1; i < 3; i++ {
		if q.Reserve(now.Add(time.Duration(i)*10*time.Minute), func(string) bool { return false }) == nil {
			t.Fatal("retry missing")
		}
	}
	if q.Reserve(now.Add(time.Hour), func(string) bool { return false }) != nil {
		t.Fatal("unbounded retry")
	}
	if q.Barrier("ordinary") == nil {
		t.Fatal("exhausted repair cleared outage")
	}
	q.Observe(Observation{ID: "first", Disabled: true}, now.Add(time.Hour))
	if q.Barrier("ordinary") != nil {
		t.Fatal("deliberately disabled component still blocks")
	}
}

func TestFatalCorruptJournalFailsClosed(t *testing.T) {
	dir := t.TempDir()
	q := Queue{}
	q.Observe(Observation{ID: "guard", Failed: true}, time.Now())
	if err := q.Save(dir); err != nil {
		t.Fatal(err)
	}
	got, err := Read(dir)
	if err != nil || len(got.Incidents) != 1 {
		t.Fatalf("round trip: %+v %v", got, err)
	}
	if err := os.WriteFile(filepath.Join(dir, File), []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(dir); err == nil {
		t.Fatal("corrupt journal read as empty")
	}
}
