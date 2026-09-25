package alerts

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestOneOwner(t *testing.T) {
	dir := t.TempDir()
	first, second := NewStore(dir), NewStore(dir)
	if owned, _, err := first.Own(); !owned || err != nil {
		t.Fatalf("first: %v %v", owned, err)
	}
	owned, owner, err := second.Own()
	if owned || err != nil || owner.PID != os.Getpid() {
		t.Fatalf("second while the first owns: %v %+v %v", owned, owner, err)
	}
	if err := first.Release(); err != nil {
		t.Fatal(err)
	}
	if owned, _, _ := second.Own(); !owned {
		t.Error("the second does not take over once the first is gone")
	}
}

func TestImportKeepsWhatIsKnown(t *testing.T) {
	legacy := t.TempDir()
	for name, raw := range map[string]string{
		instA: `{"reachable": true, "alerts": {"b6dd": {"severity": "page", "team": "bumblebee", "alertname": "X", "cluster": "alpha", "where": "-", "since": ""}}}`,
		instB: `{"reachable": false}`,
		instC: `{"reachable": true, "alerts": {}}`,
	} {
		if err := os.WriteFile(filepath.Join(legacy, name+".json"), []byte(raw), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	s := NewStore(t.TempDir())
	st, _ := s.Load()
	st.Installations[instC] = &Installation{Reachable: true, Alerts: Set{"k": {}}}
	done, err := Import(st, legacy)
	if err != nil || !slices.Equal(done, []string{instA, instB}) {
		t.Fatalf("imported %v, %v", done, err)
	}
	if err := s.Save(st); err != nil {
		t.Fatal(err)
	}
	st, _ = s.Load()
	a, b, g := st.Installations[instA], st.Installations[instB], st.Installations[instC]
	if !a.Reachable || a.Alerts["b6dd"].Team != "bumblebee" || b.Reachable || b.Alerts != nil || len(g.Alerts) != 1 {
		t.Errorf("after import: %+v %+v %+v", a, b, g)
	}
	// An imported set is a baseline: the next reading prints only what changed.
	if lines, _ := rules.Step(instA, a, ok(), now); len(lines) != 1 || lines[0] != "ALERT RESOLVED alpha PAGE BUMBLEBEE X -" {
		t.Errorf("lines = %q", lines)
	}
}
