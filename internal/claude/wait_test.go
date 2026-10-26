package claude

import (
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/proc"
)

func TestIsWait(t *testing.T) {
	const devctl = devctlComm
	yes := [][]string{
		{devctl, "pr", "merge", "giantswarm/x", "1"},
		{devctl, "release", "wait", "giantswarm/x", "--pr", "1"},
		{"gh", "pr", "checks", "1", "--watch"},
		{"docker", "image", "save", "x"},
		{"kubectl", "port-forward", "svc/x", "8080"},
	}
	no := [][]string{{devctl, "version"}, {"gh", "pr", "view", "1"}, {"kubectl", "get", "pods"}}
	for _, args := range yes {
		if !IsWait(&proc.Process{Comm: args[0], Args: args}) {
			t.Errorf("%v is a wait", args)
		}
	}
	for _, args := range no {
		if IsWait(&proc.Process{Comm: args[0], Args: args}) {
			t.Errorf("%v is no wait", args)
		}
	}
}

// A sleep, a merge's turn at the gate and a lease's wait only wait too; a
// build, a test or a read is work.
func TestOnlyWaits(t *testing.T) {
	yes := []string{"sleep 300", "gh run watch 12", "beekeeper gate --wait 30m -- devctl pr merge o/r 1", "beekeeper lease need kind-1 --wait 9m"}
	no := []string{"go test ./...", "beekeeper run -- make test", "beekeeper lease need kind-1", "gh pr view 1", "curl -s http://x"}
	for _, a := range yes {
		args := strings.Fields(a)
		if !onlyWaits(&proc.Process{Comm: args[0], Args: args}) {
			t.Errorf("%q only waits", a)
		}
	}
	for _, a := range no {
		args := strings.Fields(a)
		if onlyWaits(&proc.Process{Comm: args[0], Args: args}) {
			t.Errorf("%q works", a)
		}
	}
}

// A session waits on its own only while every command it runs waits and
// its transcript has been quiet long enough.
func TestOwnWait(t *testing.T) {
	now := time.Date(2026, 10, 26, 20, 0, 0, 0, time.UTC)
	quiet := 5 * time.Minute
	sleep := Command{Args: "sleep 600", Remaining: 4 * time.Minute, Wait: true}
	watch := Command{Args: "gh run watch 123", Wait: true}
	build := Command{Args: "go test ./..."}
	cases := []struct {
		name string
		s    Session
		want string
	}{
		{"a sleep", Session{Commands: []Command{watch, sleep}, LastActive: now.Add(-10 * time.Minute)}, "sleep 600 (4m0s left)"},
		{"a CI watch", Session{Commands: []Command{watch}, LastActive: now.Add(-10 * time.Minute)}, "gh run watch 123"},
		{"a build beside the wait", Session{Commands: []Command{sleep, build}, LastActive: now.Add(-10 * time.Minute)}, ""},
		{"active within quiet", Session{Commands: []Command{sleep}, LastActive: now.Add(-time.Minute)}, ""},
		{"no command", Session{LastActive: now.Add(-time.Hour)}, ""},
		{"activity unknown", Session{Commands: []Command{sleep}}, ""},
	}
	for _, c := range cases {
		if got := c.s.OwnWait(now, quiet); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
	long := Command{Args: "beekeeper gate --wait 30m -- devctl pr merge giantswarm/a-long-repository-name 1234 --squash", Wait: true}
	if got := (&Session{Commands: []Command{long}, LastActive: now.Add(-time.Hour)}).OwnWait(now, quiet); len([]rune(got)) > 60 || !strings.HasSuffix(got, "…") {
		t.Errorf("a long command: %q", got)
	}
}
