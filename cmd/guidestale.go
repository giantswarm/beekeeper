package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/giantswarm/beekeeper/internal/platform"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/pkg/project"
)

// guideSelf is what a guide watch says about itself: GUIDE WATCH STALE once
// self-update replaced its binary, and GUIDE SAVE REFUSED, with the lines
// the refused save would have recorded, while a newer release's state
// refuses its saves. Both name the command that re-arms it, which is its
// owner's to run: the watch never re-executes itself.
type guideSelf struct {
	pid  int
	args []string
	// replaced is the path of this watch's executable and whether another
	// file was renamed over it since it started; versionOf is the version
	// a binary file reports. The tests replace both.
	replaced  func() (string, bool)
	versionOf func(context.Context, string) string

	stale bool
	// refused is the feed of the last refused save it said, so a save
	// refused again for the same lines says nothing new.
	refused *string
}

func newGuideSelf() *guideSelf {
	g := &guideSelf{pid: os.Getpid(), args: os.Args, versionOf: binaryVersion}
	g.replaced = func() (string, bool) {
		b := platform.ProcessBinary(g.pid)
		if b == nil {
			return "", false
		}
		return b.Path, b.Replaced()
	}
	return g
}

// lines are the lines of one poll whose feed said feed and ended with err.
func (g *guideSelf) lines(ctx context.Context, feed []string, err error) []string {
	var out []string
	if !g.stale {
		if path, ok := g.replaced(); ok {
			g.stale = true
			out = append(out, fmt.Sprintf("GUIDE WATCH STALE: this watch (pid %d) runs %s; self-update installed %s at %s, a re-arm picks it up: %s",
				g.pid, project.Version(), g.versionOf(ctx, path), path, rearm(g.args)))
		}
	}
	var stale *state.StaleWriterError
	switch {
	case errors.As(err, &stale):
		said := strings.Join(feed, "\n")
		if g.refused != nil && *g.refused == said {
			return out
		}
		g.refused = &said
		out = append(out, fmt.Sprintf("GUIDE SAVE REFUSED: %v; re-arm this watch: %s", err, rearm(g.args)))
		return append(out, feed...)
	case err != nil:
		return append(out, fmt.Sprintf("GUIDE FEED: %v", err))
	}
	g.refused = nil
	return append(out, feed...)
}
