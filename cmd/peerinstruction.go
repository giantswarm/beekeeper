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
	"github.com/giantswarm/beekeeper/internal/state"
)

const (
	relayInstructionConsumed = "consumed"
	relayPeerAccepted        = "peer-accepted"
	relayMessageVerb         = "agents.message"
)

// relayInstruction records the payload before attempting transport. The Guide
// need not run hooks, so its peer envelope carries the payload itself. Workers
// keep the separate hook queue to avoid executing a peer copy a second time.
func (a *app) relayInstruction(ctx context.Context, session, address, msg string) (string, error) {
	// Hook entry points load configuration without opening the command store.
	store := a.store
	if store == nil {
		var err error
		store, err = state.Open(a.cfg.StateDir)
		if err != nil {
			return "", err
		}
	}
	st, err := store.Read()
	if err != nil {
		return "", err
	}
	by, err := a.caller()
	if err != nil {
		by = state.Party{Name: noSession}
	}
	// A peer acknowledgement proves queue acceptance, not consumption. The log
	// is the durable recovery path even if a recipient has no hook, refuses a
	// cross-session message, or hands its role to another session.
	if err := store.Record(event(by, relayMessageVerb, "to %s (session %s), recorded before peer send; consumption unconfirmed:\n%s", address, session, msg)); err != nil {
		return "", fmt.Errorf("relay not sent: cannot retain message in log: %w", err)
	}
	if a.guideInstructionSession(st, session) {
		_, err := (peer.Sender{Dir: a.cfg.StateDir}).Send(ctx, address, msg)
		if err != nil {
			return "", fmt.Errorf("peer acceptance unconfirmed; message retained in beekeeper log --verb %s: %w", relayMessageVerb, err)
		}
		return relayPeerAccepted, nil
	}
	path, err := peer.QueueInstruction(a.cfg.StateDir, session, msg)
	if err != nil {
		return "", err
	}
	_, err = (peer.Sender{Dir: a.cfg.StateDir}).Send(ctx, address,
		"beekeeper has queued a relayed instruction for this session. A beekeeper hook can consume it at the next tool or turn boundary. Without hooks, read the message in beekeeper log --verb agents.message. Do not repeat an instruction already consumed by a hook.")
	status := peer.InstructionStatus(path)
	if err != nil && status != relayInstructionConsumed {
		// Notification acceptance was not confirmed. Withdraw the hook copy so
		// an explicit retry leaves only one hook payload; the log retains the text.
		root, openErr := os.OpenRoot(filepath.Dir(path))
		if openErr != nil {
			return reviewedMergeQueued, fmt.Errorf("instruction queued, but cannot open inbox: %w", openErr)
		}
		defer func() { _ = root.Close() }()
		if removeErr := root.Remove(filepath.Base(path)); errors.Is(removeErr, os.ErrNotExist) {
			return relayInstructionConsumed, nil
		} else if removeErr != nil {
			return reviewedMergeQueued, fmt.Errorf("instruction queued, but notification failed: %w", err)
		}
		return "", err
	}
	return status, nil
}

// Match session identity, never a title that a relieved holder may still carry.
// A desktop row can point at a newer CLI than the holder's recorded session.
func (a *app) guideInstructionSession(st *state.State, session string) bool {
	if session == "" || st.Guide == nil || st.Guide.Holder == nil {
		return false
	}
	holder := st.Guide.Holder
	if holder.Session == session {
		return true
	}
	if holder.HostSession != "" {
		if r, ok := claude.ReadRecord(a.cfg, holder.HostSession); ok {
			return r.CLISessionID == session
		}
	}
	return false
}

func relayInstructionOutcome(status string) string {
	switch status {
	case relayPeerAccepted:
		return "accepted by peer queue; recipient consumption unconfirmed; message retained in beekeeper log --verb " + relayMessageVerb
	case relayInstructionConsumed:
		return "consumed by a beekeeper hook; recipient acknowledgement unconfirmed"
	default:
		return "queued for a beekeeper hook; consumption unconfirmed; without hooks read beekeeper log --verb " + relayMessageVerb
	}
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
