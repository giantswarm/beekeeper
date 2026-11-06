package guard

import (
	"strings"
	"testing"
)

// psForm is the masked read a process-table refusal names.
const psForm = "beekeeper ps"

func TestHookDockerImageConfigFormats(t *testing.T) {
	for _, command := range []string{"docker inspect", "docker image inspect"} {
		for _, flag := range []string{"--format ", "--format=", "-f "} {
			for _, tc := range []struct {
				format string
				deny   bool
			}{
				{`{{.Config.Entrypoint}}`, false},
				{`{{json .Config.Cmd}}`, false},
				{`{{.Config.User}} {{.Config.WorkingDir}}`, false},
				{`{{json .Config.ExposedPorts}}`, false},
				{`{{- json .Config.Entrypoint -}}`, false},
				{`{{range .Config.Env}}{{println (index (split . "=") 0)}}{{end}}`, false},
				{`{{.Config.User}} {{range .Config.Env}}{{println (index (split . "=") 0)}}{{end}}`, false},
				{`{{json .Config.Env}}`, true},
				{`{{.Config.Env}}`, true},
				{`{{range .Config.Env}}{{println .}}{{end}}`, true},
				{`{{range .Config.Env}}{{println (index (split . "=") 1)}}{{end}}`, true},
				{`{{range .Config.Env}}{{println (index (split . "=") 0)}}{{println .}}{{end}}`, true},
				{`{{range .Config.Env}}{{println (index (split . "=") 0)}}{{end}}{{json .Config.Env}}`, true},
				{`{{.Config.User}} {{json .Config}}`, true},
				{`{{.Config.User}} {{.Args}}`, true},
				{`{{.Config.User}} {{json .}}`, true},
				{`{{index .Config "Env"}}`, true},
				{`{{$env := .Config.Env}}{{json $env}}`, true},
			} {
				cmd := command + " " + flag + "'" + tc.format + "' image"
				t.Run(cmd, func(t *testing.T) {
					d := decide(t, hook(), "/", cmd, nil)
					denied := d != nil && d.PermissionDecision == decisionDeny
					if denied != tc.deny {
						t.Fatalf("deny = %v, want %v: %+v", denied, tc.deny, d)
					}
				})
			}
		}
	}
}

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
		"docker inspect manifest inspect",
		"docker --context manifest inspect ollama",
		"docker manifest inspect sample:latest && docker inspect container",
		"docker buildx imagetools inspect sample:latest && docker container inspect container",
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
		"/usr/bin/ps aux",
		"sudo ps -ef",
		"watch ps -o cmd",
		"ssh host ps aux",
		"docker exec c ps -ef",
		"kubectl exec p -- ps aux",
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
		"ps -u fixture-person -o pid,comm",
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
		"docker manifest inspect sample:latest",
		"docker manifest inspect --verbose sample:latest",
		"docker buildx imagetools inspect sample:latest",
		"docker buildx imagetools inspect --raw sample:latest",
		"docker buildx imagetools inspect --format '{{json .}}' sample:latest",
		"docker manifest inspect sample:latest && docker buildx imagetools inspect sample:latest",
		"beekeeper ps",
		"beekeeper ps claude 1234",
		"kill 1234",
		"ollama ps",
		"ollama ps qwen3",
		"curl -s localhost:11434/api/ps",
		"curl -s http://127.0.0.1:11434/api/ps -H 'Accept: application/json'",
		"curl -sf localhost:11434/api/ps -w '%{http_code}'",
		"curl http://127.0.0.1:11434/api/ps | jq -r '.models[].name'",
		"wget -qO- http://127.0.0.1:11434/api/ps --header 'Accept: x'",
		"docker compose ps aux",
		"podman pod ps",
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
