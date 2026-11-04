package execution

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
)

const (
	checkOperation       = "check"
	inventoryOperation   = "inventory"
	missingOperation     = "missing"
	syncControlOperation = "sync-control"
	stopOperation        = "stop"
)

// InventoryReader lists only journal-backed executions of one coordinator.
// Missing confirms both journal absence and an inactive owned unit; a missing
// journal alone never proves that a process ended.
type InventoryReader interface {
	Inventory(context.Context, string) ([]Ref, error)
	Missing(context.Context, Ref) (bool, error)
}

func (t SSHTransport) Inventory(ctx context.Context, coordinator string) ([]Ref, error) {
	var refs []Ref
	err := t.Call(ctx, inventoryOperation, Ref{}, coordinator, &refs)
	return refs, err
}

func (t SSHTransport) Missing(ctx context.Context, ref Ref) (bool, error) {
	var absent bool
	err := t.Call(ctx, missingOperation, ref, nil, &absent)
	return absent, err
}

func (s *Server) Inventory(ctx context.Context, coordinator string) ([]Ref, error) {
	if !ValidIdentity(coordinator) {
		return nil, errors.New("invalid inventory coordinator")
	}
	if _, ok := s.Config.Coordinators[coordinator]; !ok {
		return nil, errors.New("unknown inventory coordinator")
	}
	if err := ownedDirectory(s.Root); err != nil {
		return nil, err
	}
	paths, err := filepath.Glob(filepath.Join(s.Root, "*.json"))
	if err != nil {
		return nil, err
	}
	refs := []Ref{}
	for _, p := range paths {
		id := strings.TrimSuffix(filepath.Base(p), ".json")
		if _, err := uuid.Parse(id); err != nil {
			continue
		}
		var j Journal
		if err := readJSON(p, &j); err != nil {
			return nil, err
		}
		if string(j.Handle.Ref.Execution) != id {
			return nil, errors.New("inventory journal identity mismatch")
		}
		j, err = s.Read(j.Handle.Ref)
		if err != nil {
			return nil, err
		}
		if j.Request.Coordinator != coordinator || j.StopAcknowledged {
			continue
		}
		if j.Phase == Exited || j.Phase == Failed {
			o, err := s.Inspect(ctx, j.Handle.Ref)
			if err != nil {
				return nil, errors.New("cannot establish owned unit state")
			}
			if o.ProcessState == Exited || o.ProcessState == Failed {
				continue
			}
		}
		refs = append(refs, j.Handle.Ref)
	}
	return refs, nil
}

func (s *Server) Missing(ctx context.Context, ref Ref) (bool, error) {
	// Serialize with start/stop on this identity. Never stop a unit whose
	// journal disappeared: ownership can no longer be verified.
	l, _, err := s.lock(ctx, ref)
	if err != nil {
		return false, err
	}
	defer func() { _ = l.Unlock() }()
	if _, err := s.Read(ref); !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	if s.Launcher == nil {
		return false, errors.New("cannot confirm missing execution unit")
	}
	status := s.Launcher.State(ctx, UnitName(ref))
	return status == unitInactive || status == Failed, nil
}
