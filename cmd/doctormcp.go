package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/giantswarm/beekeeper/internal/proc"
)

// Docker keeps an attached stdio container alive after its client exits.
// Restrict cleanup to MCP images with attached stdin, never detached services.
type mcpContainer struct {
	ID      string
	Name    string
	Created time.Time
	Config  struct {
		Image       string
		OpenStdin   bool
		AttachStdin bool
		Tty         bool
	}
	State struct{ Running bool }
}

func orphanMCP(c mcpContainer, t *proc.Table, now time.Time) bool {
	image := strings.Split(filepath.Base(strings.Split(c.Config.Image, "@")[0]), ":")[0]
	mcp := image == "mcp" || strings.HasPrefix(image, "mcp-") || strings.HasSuffix(image, "-mcp") || strings.HasPrefix(c.Config.Image, "mcp/") || strings.Contains(c.Config.Image, "/mcp/")
	if !mcp || !c.State.Running || !c.Config.OpenStdin || !c.Config.AttachStdin || c.Config.Tty || c.Created.IsZero() || now.Sub(c.Created) < time.Minute || t == nil {
		return false
	}
	// A client may belong to a session absent from beekeeper's roster. Keep
	// every matching client, including wrappers and clients of other containers
	// of this image: uncertainty must never stop a running session's server.
	for _, p := range t.ByPID {
		if len(p.Args) < 2 || filepath.Base(p.Args[0]) != "docker" {
			continue
		}
		if !slices.ContainsFunc(p.Args[1:], func(arg string) bool { return arg == workerGitHubRunGroup || arg == labStart || arg == "attach" }) {
			continue
		}
		for _, arg := range p.Args[1:] {
			if arg == c.Config.Image || arg == strings.TrimPrefix(c.Name, "/") || arg == c.ID || len(arg) >= 12 && strings.HasPrefix(c.ID, arg) {
				return false
			}
		}
	}
	return true
}

// doctorMCP reads Docker before the process table, so a client starting a
// container is included. The creation grace also covers startup and reconnects.
func (a *app) doctorMCP(ctx context.Context, dry bool) []string {
	return cleanMCP(ctx, dry, a.now, a.labDockerCall, proc.Read)
}

func cleanMCP(ctx context.Context, dry bool, now time.Time, docker func(context.Context, ...string) ([]byte, error), processes func() (*proc.Table, error)) []string {
	out, err := docker(ctx, "ps", "--quiet", "--no-trunc")
	if err != nil || len(strings.Fields(string(out))) == 0 {
		return nil
	}
	ids := strings.Fields(string(out))
	out, err = docker(ctx, slices.Insert(ids, 0, "inspect")...)
	if err != nil {
		return []string{"MCP containers: inspection failed"}
	}
	var containers []mcpContainer
	if json.Unmarshal(out, &containers) != nil {
		return []string{"MCP containers: invalid inspection response"}
	}
	t, err := processes()
	if err != nil {
		return []string{"MCP containers: process ownership unavailable"}
	}
	var lines []string
	for _, c := range containers {
		if !orphanMCP(c, t, now) {
			continue
		}
		name := strings.TrimPrefix(c.Name, "/")
		if name == "" {
			name = c.ID
		}
		if dry {
			lines = append(lines, fmt.Sprintf("would stop MCP container %q (its stdio client ended)", name))
		} else if _, err := docker(ctx, labStop, "--timeout", "10", c.ID); err != nil {
			lines = append(lines, fmt.Sprintf("failed to stop MCP container %q", name))
		} else {
			lines = append(lines, fmt.Sprintf("stopped MCP container %q (its stdio client ended)", name))
		}
	}
	return lines
}
