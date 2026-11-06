package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/peer"
)

const relayInstructionDelivered = "delivered"

// relayInstruction queues the payload before notifying the CLI. The peer
// notification contains no copy of the payload, so a later peer delivery cannot
// repeat an instruction already consumed by a boundary hook.
func (a *app) relayInstruction(ctx context.Context, session, address, msg string) (string, error) {
	path, err := peer.QueueInstruction(a.cfg.StateDir, session, msg)
	if err != nil {
		return "", err
	}
	_, err = (peer.Sender{Dir: a.cfg.StateDir}).Send(ctx, address,
		"beekeeper has queued a relayed instruction for this session. It is delivered by the next beekeeper hook. If you are idle, make a tool call to receive it; do not repeat an instruction already delivered by a hook.")
	status := peer.InstructionStatus(path)
	if err != nil && status != relayInstructionDelivered {
		// No notification was accepted. Withdraw the pending instruction so callers
		// can retry without leaving a duplicate behind.
		root, openErr := os.OpenRoot(filepath.Dir(path))
		if openErr != nil {
			return reviewedMergeQueued, fmt.Errorf("instruction queued, but cannot open inbox: %w", openErr)
		}
		defer func() { _ = root.Close() }()
		if removeErr := root.Remove(filepath.Base(path)); errors.Is(removeErr, os.ErrNotExist) {
			return relayInstructionDelivered, nil
		} else if removeErr != nil {
			return reviewedMergeQueued, fmt.Errorf("instruction queued, but notification failed: %w", err)
		}
		return "", err
	}
	return status, nil
}

// instructionTarget resolves a live Claude peer without guessing between CLIs.
func (a *app) instructionTarget(to string) (*claude.Session, error) {
	table, err := plat.Machine.Processes()
	if err != nil {
		return nil, nil
	}
	sessions := claude.Discover(a.cfg, table, time.Now())
	var found *claude.Session
	for _, s := range sessions {
		matches := s.ID == to || s.HostID == to || strings.EqualFold(s.Address(), to) || strings.EqualFold(s.Name, to)
		if sock, ok := strings.CutPrefix(to, "uds:"); ok {
			matches = sock != "" && liveSocket(s.PID) == sock
		}
		if !matches || s.ID == "" {
			continue
		}
		if found != nil {
			return nil, refused("several running CLIs match the relay target; use a unique session address")
		}
		found = s
	}
	if found != nil && liveSocket(found.PID) == "" {
		if _, err := uniqueName(sessions, found); err != nil {
			return nil, err
		}
	}
	return found, nil
}

func instructionAddress(target *claude.Session) string {
	if sock := liveSocket(target.PID); sock != "" {
		return "uds:" + sock
	}
	return target.Address()
}
