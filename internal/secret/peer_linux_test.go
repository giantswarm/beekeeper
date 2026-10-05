package secret

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// asBroker makes this test process the broker unit's main process, running
// this binary.
func asBroker(t *testing.T) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	brokerAs(t, os.Getpid(), exe)
}

func brokerAs(t *testing.T, pid int, bin string) {
	t.Helper()
	prev := brokerProcess
	brokerProcess = func() (int, string, error) { return pid, bin, nil }
	t.Cleanup(func() { brokerProcess = prev })
}

func TestParseUnitShow(t *testing.T) {
	out := "MainPID=2435198\nExecStart={ path=/home/u/.go/bin/beekeeper ; argv[]=/home/u/.go/bin/beekeeper sandbox broker ; ignore_errors=no ; start_time=[n/a] ; stop_time=[n/a] ; pid=0 ; code=(null) ; status=0/0 }\n"
	pid, bin, err := parseUnitShow(out)
	if err != nil || pid != 2435198 || bin != "/home/u/.go/bin/beekeeper" {
		t.Fatalf("parseUnitShow: %d, %q, %v", pid, bin, err)
	}
	if _, _, err := parseUnitShow("MainPID=0\nExecStart={ path=/x ; }\n"); err == nil || !strings.Contains(err.Error(), "not running") {
		t.Errorf("a stopped unit: %v", err)
	}
	if _, _, err := parseUnitShow("MainPID=7\nExecStart=\n"); err == nil {
		t.Error("a unit without a binary was taken")
	}
}

func TestPeerIsTheBrokerUnit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.sock")
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	exe, _ := os.Executable()
	for _, tc := range []struct {
		name string
		pid  int
		bin  string
		want string
	}{
		{"the broker", os.Getpid(), exe, ""},
		{"another process", os.Getpid() + 1, exe, "not " + BrokerUnit + "'s main process"},
		{"another binary", os.Getpid(), "/usr/bin/true", "not this beekeeper"},
	} {
		brokerAs(t, tc.pid, tc.bin)
		c, err := net.Dial("unix", path)
		if err != nil {
			t.Fatal(err)
		}
		err = Peer(c)
		_ = c.Close()
		switch {
		case tc.want == "" && err != nil:
			t.Errorf("%s: %v", tc.name, err)
		case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
			t.Errorf("%s: %v, want %q", tc.name, err, tc.want)
		}
	}
}
