package guard

import (
	"strings"
	"testing"
)

// psForm is the masked read a process-table refusal names.
const psForm = "beekeeper ps"

func TestHookRefusesCommandLineReads(t *testing.T) {
	for _, cmd := range []string{
		"ps aux",
		"ps auxww | grep claude",
		"ps ax",
		"ps -ef",
		"ps -eF --forest",
		"ps -eo pid,ppid,etime,args",
		"ps -eo pid,args --sort=-etime | head",
		"ps -o args= -p 1234",
		"ps -p 1234 -o pid=,cmd=",
		"ps --format pid,command",
		"ps -e --format=pid,command",
		"ps axo pid,command",
		"ps -eo pid,comm e",
		"ps ww -p 1",
		"ps -fp 1234",
		"ps -u root -f",
		"pgrep -a claude",
		"pgrep -af 'devctl pr wait'",
		"pgrep -fa devctl",
		"pgrep --list-full claude",
		"pstree -a",
		"pstree -ap 1234",
		"pstree --arguments",
		"cat /proc/1234/cmdline",
		"tr '\\0' ' ' < /proc/1234/cmdline",
		"tr '\\0' ' ' </proc/$pid/cmdline",
		`xargs -0 < "/proc/$pid/cmdline"`,
		"cat /proc/self/environ",
		"strings /proc/1/environ",
		"for f in /proc/*/cmdline; do cat \"$f\"; done",
		"cat /proc/1/task/2/cmdline",
		"docker inspect ollama",
		"docker container inspect ollama",
		"docker inspect -f '{{json .Config.Env}}' ollama",
		"docker inspect --format='{{.Args}}' ollama",
		"docker inspect --format '{{json .}}' ollama",
		"podman inspect x",
		"docker ps --no-trunc",
		"docker ps -a --no-trunc --filter name=x",
		"docker container ls --no-trunc",
		"bash -c 'ps aux | grep x'",
		"timeout 5 ps -ef",
		"ps -ef | grep -c claude",
		"cat /proc/1234/cmdline | sha256sum",
	} {
		d := decide(t, hook(), "/", cmd, nil)
		if d == nil || d.PermissionDecision != decisionDeny {
			t.Errorf("%q: want a refusal, got %+v", cmd, d)
			continue
		}
		if !strings.Contains(d.Reason, psForm) || !strings.HasPrefix(d.Reason, "Refused: `") {
			t.Errorf("%q: the refusal does not name %q:\n%s", cmd, psForm, d.Reason)
		}
	}
}

func TestHookPassesMaskedProcessReads(t *testing.T) {
	for _, cmd := range []string{
		"ps",
		"ps -e",
		"ps -eo pid,ppid,etime,time,rss,comm",
		"ps -o pid=,comm= -p 1234",
		"ps -p 1234",
		"ps -C claude -o pid,etime",
		"ps -u teemow -o pid,comm",
		"ps -eo pid,comm --sort=-rss | head",
		"ps axc",
		"ps axo pid,comm",
		"ps aux | wc -l",
		"pgrep -c claude",
		"pgrep claude",
		"pgrep -f 'devctl pr wait'",
		"pgrep -l claude",
		"pgrep -d a claude",
		"pstree -p 1234",
		"pstree",
		"ls /proc/1234/cmdline",
		"wc -c /proc/1234/cmdline",
		"cat /proc/1234/cmdline | wc -c",
		"cat /proc/1234/status",
		"cat /proc/loadavg",
		"docker inspect --format '{{.Name}} {{.State.Status}}' ollama",
		"docker inspect -f '{{.State.Health.Status}}' ollama",
		"docker ps",
		"docker ps --format '{{.Names}}' --no-trunc",
		"docker ps -f name=x",
		"beekeeper ps",
		"beekeeper ps claude 1234",
		"kill 1234",
	} {
		if d := decide(t, hook(), "/", cmd, nil); d != nil && d.PermissionDecision == decisionDeny {
			t.Errorf("%q: want no refusal, got %s", cmd, d.Reason)
		}
	}
}

func TestPsLeaks(t *testing.T) {
	for _, c := range []struct {
		args string
		want bool
	}{
		{"", false},
		{"-e", false},
		{"-ef", true},
		{"aux", true},
		{"axc", false},
		{"axce", true},
		{"-o comm -f", false},
		{"-o pid -o args", true},
		{"-o pid,args:40", true},
		{"-O rss", false},
		{"-Oargs", true},
		{"1234", false},
		{"-- -f", false},
	} {
		if got := psLeaks(strings.Fields(c.args)); got != c.want {
			t.Errorf("ps %s: got %v, want %v", c.args, got, c.want)
		}
	}
}
