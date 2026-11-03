package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/gocache"
)

// A Go cache cleared under a running build breaks it: a compile reads an
// entry the clear removed. beekeeper cache clear go waits for the beekeeper
// runs that hold the build cache (gocache.Hold), holds back the runs that
// start meanwhile, clears, and logs the clear (gocache.clear). The hook
// refuses the go command's own clears and a removal of the caches, naming
// it (goCacheRefusal).

const goCacheClearVerb = "gocache.clear"

// goCacheClearWait is how long a clear waits for the runs by default.
const goCacheClearWait = time.Hour

// goModCacheDir is the module cache a clear with --modcache clears. Tests
// replace it, as goCacheDir.
var goModCacheDir = gocache.ModDir

func (a *app) cacheCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "cache",
		Short: "Clear a cache the machine's builds share, without breaking a running build",
	}
	c.AddCommand(a.cacheClearCmd())
	return c
}

func (a *app) cacheClearCmd() *cobra.Command {
	var (
		modcache bool
		wait     time.Duration
	)
	c := &cobra.Command{
		Use:   "clear go [--modcache] [--wait 1h]",
		Short: "Clear the Go build cache once no beekeeper run uses it",
		Long: `clear go clears the Go build cache every session's builds share ($GOCACHE,
else ~/.cache/go-build), and with --modcache the module cache too
($GOMODCACHE, else $GOPATH/pkg/mod), with go clean -cache and -modcache.

A beekeeper run holds the build cache for the length of its command. The
clear waits up to --wait for the runs that hold it, saying so once, and a
run that starts while it waits waits behind it; no doctor trim runs
meanwhile. A compile that reads an entry the clear removed fails, which the
wait prevents. The clear is logged (` + goCacheClearVerb + `).

It is refused inside a beekeeper run, which would wait on its own run: run
it as a command of its own.

The hook refuses go clean -cache, go clean -modcache and a removal of either
cache's directory in a session, naming this command.`,
		Args:      cobra.MatchAll(cobra.ExactArgs(1), cobra.OnlyValidArgs),
		ValidArgs: []string{"go"},
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, cancel := context.WithTimeout(cmd.Context(), wait)
			defer cancel()
			line, err := a.clearGoCache(ctx, modcache, func(s string) { _, _ = fmt.Fprintln(cmd.ErrOrStderr(), s) })
			if err != nil {
				return err
			}
			_, err = fmt.Fprintln(a.out, line)
			return err
		},
	}
	c.Flags().BoolVar(&modcache, "modcache", false, "clear the module cache too")
	c.Flags().DurationVar(&wait, "wait", goCacheClearWait, "how long to wait for the beekeeper runs that hold the build cache")
	return c
}

// clearGoCache clears the build cache, and the module cache with modcache,
// holding the build cache's use lock exclusive: it waits for the runs that
// hold it until ctx ends, saying so with say. It returns the line it logs.
func (a *app) clearGoCache(ctx context.Context, modcache bool, say func(string)) (string, error) {
	dir := goCacheDir()
	if dir == "" {
		return "", errors.New("the Go build cache is off (GOCACHE=off) or unknown")
	}
	if held := os.Getenv(gocache.HeldEnv); held == dir {
		return "", fmt.Errorf("refused: this runs inside a beekeeper run, which holds the Go build cache %s: the clear would wait on its own run. Run `beekeeper cache clear go` as a command of its own", dir)
	}
	t0 := time.Now()
	release, err := gocache.Hold(ctx, dir, func() {
		say(fmt.Sprintf("waiting for the beekeeper runs that hold the Go build cache %s (new runs wait behind the clear)", dir))
	})
	if errors.Is(err, context.DeadlineExceeded) {
		return "", fmt.Errorf("beekeeper runs still hold the Go build cache %s after %s; nothing cleared (--wait)", dir, time.Since(t0).Round(time.Second))
	}
	if err != nil {
		return "", err
	}
	defer release()
	before, _ := gocache.Scan(dir)
	if err := goClean(ctx, dir, "", "-cache"); err != nil {
		return "", err
	}
	line := fmt.Sprintf("Go build cache %s cleared: %s freed", dir, gibStr(before.Bytes))
	if modcache {
		mod := goModCacheDir()
		if mod == "" {
			return "", errors.New("the Go module cache is unknown (no GOMODCACHE, GOPATH or home directory)")
		}
		if err := goClean(ctx, dir, mod, "-modcache"); err != nil {
			return "", fmt.Errorf("%s; the module cache: %w", line, err)
		}
		line += "; module cache " + mod + " cleared"
	}
	if d := time.Since(t0); d >= time.Second {
		line += fmt.Sprintf(", after waiting %s for the runs", d.Round(time.Second))
	}
	by, _ := a.caller()
	_ = a.store.Log(event(by, goCacheClearVerb, "%s", line))
	return line, nil
}

// goClean runs go clean with flag on the build cache dir and the module
// cache mod ("" for the go command's own), in dir, outside any module.
func goClean(ctx context.Context, dir, mod, flag string) error {
	c := exec.CommandContext(ctx, "go", "clean", flag) //nolint:gosec // a fixed go clean flag
	c.Dir, c.Env = dir, append(os.Environ(), "GOCACHE="+dir, "GOFLAGS=", "GO111MODULE=")
	if mod != "" {
		c.Env = append(c.Env, "GOMODCACHE="+mod)
	}
	if out, err := c.CombinedOutput(); err != nil {
		return fmt.Errorf("go clean %s: %w: %s", flag, err, strings.TrimSpace(string(out)))
	}
	return nil
}
