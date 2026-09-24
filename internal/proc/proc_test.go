package proc

import (
	"os"
	"testing"
)

func TestParseStat(t *testing.T) {
	raw := []byte("4242 (tmux: server) (x)) S 1 4242 4242 0 -1 4194560 1 0 0 0 300 45 0 0 20 0 1 0 123456 1 2560 18446744073709551615 0 0 0 0 0 0 0 0 0 0 0 0 17 3 0 0 0 0 0")
	st, err := parseStat(raw)
	want := stat{comm: "tmux: server) (x)", ppid: 1, cpu: 345, start: 123456, rssPages: 2560}
	if err != nil || st != want {
		t.Fatalf("parseStat = %+v, %v; want %+v", st, err, want)
	}
	if _, err := parseStat([]byte("1 (x) S 0 1 1")); err == nil {
		t.Fatal("a short stat parsed")
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
