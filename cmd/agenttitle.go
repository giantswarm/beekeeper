package cmd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/proc"
)

// retitleWait bounds the wait for the desktop to record the title the
// session set on beekeeper's request: one short desktop turn.
const retitleWait = 2 * time.Minute

// The desktop handles every claude://resume link twice, and when the second
// delivery arrives while the first import still runs, both import: the
// first touches the transcript, the second then drops its read of the
// transcript's title and model as stale, and its untitled record is the one
// the desktop keeps. The file may show the first import's title for a
// moment, the desktop's own record has none, and the session shows untitled
// in the sidebar and to ListAgents under a default name ("<dir>-<n>"), which
// a message by its name does not reach. Nothing the desktop reads from
// outside restores it: it rereads neither the transcript nor its record
// files. The session itself can: the desktop's set_session_title tool,
// which its desktop turns have, records the title as set by an agent, which
// the desktop's own titling never overwrites.

// keepTitle gives a started session whose desktop record lost its name the
// name back, once its first turn ended and the desktop warmed its CLI: it
// asks the session, through that CLI's socket, to set its own title. It
// returns what it found or did, one line.
func (a *app) keepTitle(ctx context.Context, id, name string) (string, error) {
	host := "local_" + id
	record := func() string {
		if r, ok := claude.ReadRecord(a.cfg, host); ok {
			return r.Title
		}
		return ""
	}
	return retitle(ctx, name, record, func(ctx context.Context) string { return desktopSocket(ctx, id) }, a.peerSend, retitleWait)
}

// retitle is keepTitle's decision: title reads the desktop's record, socket
// waits for the session's desktop CLI socket (empty: none), send delivers
// the request.
func retitle(ctx context.Context, name string, title func() string, socket func(context.Context) string,
	send func(ctx context.Context, to, msg string) error, wait time.Duration,
) (string, error) {
	was := title()
	if was == name {
		return fmt.Sprintf("the desktop keeps its title %q", name), nil
	}
	sock := socket(ctx)
	if sock == "" {
		return "", fmt.Errorf("the desktop recorded %s and runs no CLI of the session to retitle it", recorded(was, name))
	}
	if err := send(ctx, "uds:"+sock, retitleRequest(name)); err != nil {
		return "", fmt.Errorf("the desktop recorded %s; asking the session to retitle itself: %w", recorded(was, name), err)
	}
	ctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		if title() == name {
			return fmt.Sprintf("the desktop had recorded %s: the session retitled itself %q", recorded(was, name), name), nil
		}
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("the desktop recorded %s and the session did not retitle itself within %s", recorded(was, name), wait)
		case <-tick.C:
		}
	}
}

// recorded names the title the desktop recorded instead of name.
func recorded(title, name string) string {
	if title == "" {
		return fmt.Sprintf("no title instead of %q", name)
	}
	return fmt.Sprintf("%q instead of %q", title, name)
}

// retitleRequest is the message that has the session set its desktop title.
func retitleRequest(name string) string {
	return fmt.Sprintf("beekeeper: the desktop lost this session's title. Call the set_session_title tool once for this session with the title %q, then end the turn without another tool call and without a reply.", name)
}

// desktopSocket waits up to twinWait for the desktop's CLI of session id and
// returns its peer socket, the address a message reaches it by whatever its
// title; empty when none came.
func desktopSocket(ctx context.Context, id string) string {
	ctx, cancel := context.WithTimeout(ctx, twinWait)
	defer cancel()
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	for {
		if t, err := proc.Read(); err == nil {
			if p := desktopTwin(t, id); p != nil {
				if sock := peerSocket(runtimeDir(), p.PID); fileExists(sock) {
					return sock
				}
			}
		}
		select {
		case <-ctx.Done():
			return ""
		case <-tick.C:
		}
	}
}

// peerSocket is the socket Claude Code's CLI pid takes peer messages on.
func peerSocket(runtime string, pid int) string {
	return filepath.Join(runtime, "cc-socks", strconv.Itoa(pid)+".sock")
}

// runtimeDir is the user's runtime directory.
func runtimeDir() string {
	if d := os.Getenv("XDG_RUNTIME_DIR"); d != "" {
		return d
	}
	return filepath.Join("/run/user", strconv.Itoa(os.Getuid()))
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
