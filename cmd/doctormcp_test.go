package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/proc"
)

func testMCPContainer(now time.Time) mcpContainer {
	c := mcpContainer{ID: strings.Repeat("a", 64), Name: "/mcp-fixture", Created: now.Add(-time.Hour)}
	c.Config.Image = "example/mcp-server:latest"
	c.Config.OpenStdin, c.Config.AttachStdin, c.State.Running = true, true, true
	return c
}

func TestOrphanMCP(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name   string
		change func(*mcpContainer)
		args   []string
		want   bool
	}{
		{name: "ended client", want: true},
		{name: "registry with port", change: func(c *mcpContainer) { c.Config.Image = "registry.example:5000/tools/mcp-server:v1" }, want: true},
		{name: "MCP namespace", change: func(c *mcpContainer) { c.Config.Image = "mcp/fixture:v1" }, want: true},
		{name: "running session client", args: []string{"/usr/bin/docker", "run", "--rm", "-i", "example/mcp-server:latest"}},
		{name: "unrostered client", args: []string{"docker", "--context", "local", "run", "-i", "example/mcp-server:latest"}},
		{name: "attached by name", args: []string{"docker", "attach", "mcp-fixture"}},
		{name: "started by short ID", args: []string{"docker", "start", "-ai", strings.Repeat("a", 12)}},
		{name: "other image client", args: []string{"docker", "run", "-i", "other"}, want: true},
		{name: "detached", change: func(c *mcpContainer) { c.Config.AttachStdin = false }},
		{name: "closed stdin", change: func(c *mcpContainer) { c.Config.OpenStdin = false }},
		{name: "terminal", change: func(c *mcpContainer) { c.Config.Tty = true }},
		{name: "ordinary service", change: func(c *mcpContainer) { c.Config.Image = "example/service:latest" }},
		{name: "starting", change: func(c *mcpContainer) { c.Created = now }},
		{name: "unknown age", change: func(c *mcpContainer) { c.Created = time.Time{} }},
		{name: "stopped", change: func(c *mcpContainer) { c.State.Running = false }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := testMCPContainer(now)
			if tc.change != nil {
				tc.change(&c)
			}
			table := proc.NewTable(&proc.Process{PID: 42, Args: tc.args})
			if got := orphanMCP(c, table, now); got != tc.want {
				t.Fatalf("orphan = %v, want %v", got, tc.want)
			}
		})
	}
	if orphanMCP(testMCPContainer(now), nil, now) {
		t.Fatal("unknown ownership must stay")
	}
}

func TestCleanMCP(t *testing.T) {
	for _, mode := range []string{"stop", "dry", "live", "process error", "stop error", "inspect error"} {
		t.Run(mode, func(t *testing.T) {
			now := time.Now()
			c := testMCPContainer(now)
			var calls [][]string
			docker := func(_ context.Context, args ...string) ([]byte, error) {
				calls = append(calls, args)
				switch args[0] {
				case "ps":
					return []byte(c.ID), nil
				case "inspect":
					if mode == "inspect error" {
						return nil, errors.New("unavailable")
					}
					return json.Marshal([]mcpContainer{c})
				case "stop":
					if mode == "stop error" {
						return nil, errors.New("failed")
					}
					return nil, nil
				default:
					t.Fatalf("unexpected call: %v", args)
					return nil, nil
				}
			}
			processes := func() (*proc.Table, error) {
				if mode == "process error" {
					return nil, errors.New("unavailable")
				}
				if mode == "live" {
					return proc.NewTable(&proc.Process{PID: 42, Args: []string{"docker", "run", "-i", c.Config.Image}}), nil
				}
				return proc.NewTable(), nil
			}
			lines := cleanMCP(context.Background(), mode == "dry", now, docker, processes)
			stopped := mode == "stop" || mode == "stop error"
			if got := calls[len(calls)-1][0] == "stop"; got != stopped {
				t.Fatalf("stop called = %v, calls %v", got, calls)
			}
			if stopped && !reflect.DeepEqual(calls[len(calls)-1], []string{"stop", "--timeout", "10", c.ID}) {
				t.Fatalf("stop args: %v", calls)
			}
			switch mode {
			case "live":
				if len(lines) != 0 {
					t.Fatal(lines)
				}
			case "dry":
				if len(lines) != 1 || !strings.Contains(lines[0], `would stop MCP container "mcp-fixture"`) {
					t.Fatal(lines)
				}
			case "stop":
				if len(lines) != 1 || !strings.Contains(lines[0], `stopped MCP container "mcp-fixture"`) {
					t.Fatal(lines)
				}
			default:
				if len(lines) != 1 {
					t.Fatal(lines)
				}
			}
		})
	}
}
