package proc

import (
	"os"
	"testing"
)

func TestParseStat(t *testing.T) {
	stat := []byte("4242 (tmux: server) (x)) S 1 4242 4242 0 -1 4194560 1 0 0 0 0 0 0 0 20 0 1 0 123456 1 1 18446744073709551615 0 0 0 0 0 0 0 0 0 0 0 0 17 3 0 0 0 0 0")
	comm, ppid, start, err := parseStat(stat)
	if err != nil || comm != "tmux: server) (x)" || ppid != 1 || start != 123456 {
		t.Fatalf("parseStat = %q, %d, %d, %v", comm, ppid, start, err)
	}
}

func TestReadFindsItself(t *testing.T) {
	if _, err := os.Stat("/proc/self/stat"); err != nil {
		t.Skip("no /proc")
	}
	tab, err := Read()
	if err != nil {
		t.Fatal(err)
	}
	me := tab.ByPID[os.Getpid()]
	if me == nil {
		t.Fatal("own process missing")
	}
	anc := tab.Ancestors(me.PID)
	if len(anc) == 0 || anc[0].PID != os.Getppid() {
		t.Errorf("ancestors = %v", anc)
	}
}
