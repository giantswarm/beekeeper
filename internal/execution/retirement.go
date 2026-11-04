package execution

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// OwnedResource is an intent recorded before creation, not a discovery of files
// to delete. Paths are derived again before use; a manifest is never authority
// to remove an arbitrary path. Retained resources include all recovery evidence.
type OwnedResource struct {
	Kind    string `json:"kind"`
	Path    string `json:"path"`
	GitRef  string `json:"gitRef,omitempty"`
	Retain  bool   `json:"retain"`
	Retired bool   `json:"retired,omitempty"`
}

type ResourceInventory struct {
	Ref       Ref             `json:"ref"`
	Resources []OwnedResource `json:"resources"`
	Recovery  string          `json:"recovery,omitempty"`
	Started   time.Time       `json:"started,omitzero"`
	Finished  time.Time       `json:"finished,omitzero"`
	Revoked   bool            `json:"revoked"`
	LastError string          `json:"lastError,omitempty"`
}

func (s *Server) ownedRoot(r Ref) string {
	return filepath.Join(s.Root, "owned", string(r.Execution), fmt.Sprint(r.Generation))
}

func (s *Server) resourcePaths(r Ref) []OwnedResource {
	parent := filepath.Join(s.Config.WorkspaceRoot, string(r.Execution))
	return []OwnedResource{
		{Kind: "worktree", Path: filepath.Join(parent, "workspace"), Retain: true},
		{Kind: "branch-repository", Path: filepath.Join(parent, "repository.git"), GitRef: "refs/heads/beekeeper-worker/" + string(r.Execution), Retain: true},
		{Kind: "scratch", Path: filepath.Join(parent, "workspace", ".beekeeper-scratch"), Retain: true},
		{Kind: "source-bundle", Path: s.sourcePath(r), Retain: true},
		{Kind: "worker-temporary", Path: filepath.Join(s.ownedRoot(r), "runtime")},
		{Kind: "credential-handles", Path: filepath.Join(s.Root, workerGitHubBinDir, string(r.Execution))},
		{Kind: "mcp-home", Path: s.agyMCPHomeDir(r)},
	}
}

func (s *Server) inventoryPath(r Ref) string {
	return filepath.Join(s.Root, "resources", string(r.Execution)+".json")
}

// Caller holds the execution lock, including during source upload before a
// journal exists. This also prevents a different generation adopting paths.
func (s *Server) resourceInventory(r Ref) (ResourceInventory, error) {
	var inv ResourceInventory
	if err := r.Validate(); err != nil {
		return inv, err
	}
	if r.Host != s.Config.Identity {
		return inv, errors.New("resource host mismatch")
	}
	p := s.inventoryPath(r)
	if err := ownedDirectory(filepath.Dir(p)); err != nil {
		return inv, err
	}
	err := readJSON(p, &inv)
	if errors.Is(err, os.ErrNotExist) {
		// Never adopt pre-existing, unrecorded paths (including legacy workers).
		// Such workers need explicit recovery, not speculative cleanup.
		for _, resource := range s.resourcePaths(r) {
			if _, statErr := os.Lstat(resource.Path); !errors.Is(statErr, os.ErrNotExist) {
				return inv, errors.New("unrecorded execution resource already exists")
			}
		}
		inv = ResourceInventory{Ref: r, Resources: s.resourcePaths(r)}
		err = atomicFile(p, mustJSON(inv))
	}
	if err != nil {
		return inv, err
	}
	if inv.Recovery != "" && inv.Recovery != filepath.Join(s.Root, "resources", string(r.Execution)+"-recovery.json") {
		return inv, errors.New("recovery reference containment mismatch")
	}
	expected := s.resourcePaths(r)
	if inv.Ref != r || len(inv.Resources) != len(expected) {
		return inv, errors.New("resource inventory ownership mismatch")
	}
	for i, resource := range inv.Resources {
		if resource.Kind != expected[i].Kind || resource.GitRef != expected[i].GitRef || resource.Path != expected[i].Path || resource.Retain != expected[i].Retain {
			return inv, errors.New("resource inventory containment mismatch")
		}
	}
	return inv, nil
}

// workerEnvironment routes downloads, scratch and caches into a recorded root.
// Provisioned host toolchains and repositories are deliberately excluded.
func (s *Server) workerEnvironment(r Ref, env []string) ([]string, error) {
	l, _, err := s.lock(context.Background(), r)
	if err != nil {
		return nil, err
	}
	defer func() { _ = l.Unlock() }()
	inv, err := s.resourceInventory(r)
	if err != nil {
		return nil, err
	}
	if !inv.Started.IsZero() {
		return nil, errors.New("execution retirement already started")
	}
	root := filepath.Join(s.ownedRoot(r), "runtime")
	for _, name := range []string{"tmp", "cache", "go", "mod", "build"} {
		if err := ownedDirectory(filepath.Join(root, name)); err != nil {
			return nil, err
		}
	}
	return append(env, "TMPDIR="+filepath.Join(root, "tmp"), "TMP="+filepath.Join(root, "tmp"), "TEMP="+filepath.Join(root, "tmp"), "XDG_CACHE_HOME="+filepath.Join(root, "cache"), "GOPATH="+filepath.Join(root, "go"), "GOMODCACHE="+filepath.Join(root, "mod"), "GOCACHE="+filepath.Join(root, "build")), nil
}

// Retire is resumable and never hard deletes. An inactive supervisor is required
// even for completed reports: a report alone does not prove the worker stopped.
// Parked/between-turn workers remain unfinished. Recovery paths stay stable per
// the unpublished-work preservation contract; retention expiry is a separate
// operation and cannot discard these artifacts during this cleanup proof.
func (s *Server) Retire(ctx context.Context, r Ref) error {
	l, _, err := s.lock(ctx, r)
	if err != nil {
		return err
	}
	defer func() { _ = l.Unlock() }()
	j, err := s.Read(r)
	if err != nil {
		return err
	}
	return s.retireLocked(ctx, j)
}

func (s *Server) retireLocked(ctx context.Context, j Journal) (retErr error) {
	r := j.Handle.Ref
	if j.ActivationAttempted {
		if s.Launcher == nil {
			return errors.New("retirement awaits supervisor proof")
		}
		state := s.Launcher.State(ctx, j.Handle.Unit)
		if state != unitInactive && state != Failed {
			return nil
		}
	} else if j.Phase != Failed && !j.StopAcknowledged {
		return nil
	}
	inv, err := s.resourceInventory(r)
	if err != nil {
		return err
	}
	if !inv.Finished.IsZero() {
		return nil
	}
	save := func() error { return atomicFile(s.inventoryPath(r), mustJSON(inv)) }
	defer func() {
		if retErr != nil {
			inv.LastError = retErr.Error()
			retErr = errors.Join(retErr, save())
		}
	}()
	if inv.Started.IsZero() {
		inv.Started = s.now()
	}
	// This durable recovery reference protects dirty, untracked and unpublished
	// work without attempting a commit or changing the worker's branch/index.
	if inv.Recovery == "" {
		inv.Recovery = filepath.Join(s.Root, "resources", string(r.Execution)+"-recovery.json")
		recovery := struct {
			Ref       Ref             `json:"ref"`
			Journal   Journal         `json:"journal"`
			Resources []OwnedResource `json:"resources"`
		}{r, j, inv.Resources}
		if err := atomicFile(inv.Recovery, mustJSON(recovery)); err != nil {
			return err
		}
	}
	if err := save(); err != nil {
		return err
	}
	// Persist intent before contacting the coordinator. A failed callback leaves
	// Finished unset so inspection/stop retries it, independently of runner exit.
	if !inv.Revoked {
		if err := s.RevokeGitHub(j); err != nil {
			return err
		}
		inv.Revoked = true
		if err := save(); err != nil {
			return err
		}
	}
	for i := range inv.Resources {
		resource := &inv.Resources[i]
		if resource.Retain || resource.Retired {
			continue
		}
		// Check every parent, including absent resources, before touching a path.
		parent := filepath.Dir(resource.Path)
		if err := ownedDirectory(parent); err != nil {
			return err
		}
		quarantine := filepath.Join(parent, filepath.Base(resource.Path)+".retired")
		fi, err := os.Lstat(resource.Path)
		if err == nil {
			if fi.Mode()&os.ModeSymlink != 0 {
				return errors.New("owned resource is a symlink")
			}
			if _, err := os.Lstat(quarantine); !errors.Is(err, os.ErrNotExist) {
				return errors.New("retirement destination already exists")
			}
			if err := os.Rename(resource.Path, quarantine); err != nil {
				return err
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		} else {
			// A retry may find the destination after a crash between rename
			// and journal persistence. Never accept a substituted symlink.
			retired, err := os.Lstat(quarantine)
			if err == nil && !retired.IsDir() {
				return errors.New("retired resource is not a directory")
			}
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
		// Persist the rename before recording its completion in another directory.
		d, err := os.Open(parent)
		if err != nil {
			return err
		}
		err = d.Sync()
		closeErr := d.Close()
		if err != nil || closeErr != nil {
			return errors.Join(err, closeErr)
		}
		resource.Retired = true
		if err := save(); err != nil {
			return err
		}
	}
	inv.Finished, inv.LastError = s.now(), ""
	return save()
}
