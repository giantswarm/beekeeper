package execution

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/giantswarm/beekeeper/internal/worktrees"
)

// Cleanup records retryable retirement separately from process termination.
// Retained work is never expired automatically: publication is not proved by
// a clean status or by the worker having reported completion.
type Cleanup struct {
	At       time.Time `json:"at"`
	Error    string    `json:"error,omitempty"`
	Retained bool      `json:"retained,omitempty"`
}

type Leftover struct {
	Execution  string `json:"execution"`
	Generation uint64 `json:"generation,omitempty"`
	Class      string `json:"class"`
	Reason     string `json:"reason"`
	Action     string `json:"action"`
}

const executionLogRetention = 7 * 24 * time.Hour

func safeOwnedPath(root, path string) error {
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil || resolved != filepath.Clean(root) || !within(root, path) || path == root {
		return errors.New("cleanup path is outside its owned root")
	}
	for p := path; p != root; p = filepath.Dir(p) {
		fi, err := os.Lstat(p)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			return errors.New("cleanup path contains a symlink")
		}
	}
	return nil
}

// workerEnvironment places temporary files and language caches under the
// execution root. Harness authentication remains in its provisioned location.
func (s *Server) workerEnvironment(j Journal, env []string) ([]string, error) {
	root := filepath.Join(s.Config.WorkspaceRoot, string(j.Handle.Ref.Execution), "runtime")
	if err := safeOwnedPath(s.Config.WorkspaceRoot, root); err != nil {
		return nil, err
	}
	values := map[string]string{
		"HOME":   filepath.Join(root, "home"),
		"TMPDIR": filepath.Join(root, "tmp"), "TMP": filepath.Join(root, "tmp"), "TEMP": filepath.Join(root, "tmp"),
		"XDG_CACHE_HOME": filepath.Join(root, "cache"), "XDG_STATE_HOME": filepath.Join(root, "state"),
		"GOCACHE": filepath.Join(root, "go-build"), "GOMODCACHE": filepath.Join(root, "go-mod"),
		"GOPATH": filepath.Join(root, "go"), "CARGO_HOME": filepath.Join(root, "cargo"),
	}
	if j.Request.Harness == HarnessAgy {
		values["HOME"] = s.agyMCPHomeDir(j.Handle.Ref)
	}
	for _, p := range values {
		pathRoot := s.Config.WorkspaceRoot
		if !within(pathRoot, p) {
			pathRoot = s.Root
		}
		if err := safeOwnedPath(pathRoot, p); err != nil {
			return nil, err
		}
		if err := os.MkdirAll(p, 0700); err != nil {
			return nil, err
		}
	}
	out := make([]string, 0, len(env)+len(values))
	if j.Request.Harness == HarnessClaude && s.Config.Harnesses[HarnessClaude].ConfigDir == "" {
		if home := os.Getenv("HOME"); home != "" {
			out = append(out, "CLAUDE_CONFIG_DIR="+filepath.Join(home, ".claude"))
		}
	}
	if home := os.Getenv("HOME"); home != "" {
		out = append(out, "RUSTUP_HOME="+filepath.Join(home, ".rustup"))
	}
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		if _, ok := values[k]; !ok {
			out = append(out, kv)
		}
	}
	for k, v := range values {
		out = append(out, k+"="+v)
	}
	return out, nil
}

// retireLocked is called only while holding the execution lock, after the
// service supervisor has confirmed termination. Failures remain retryable.
func (s *Server) retireLocked(ctx context.Context, j *Journal) {
	if j.Handle.Ref.Validate() != nil || j.Handle.Workspace != "" && j.Handle.Workspace != filepath.Join(s.Config.WorkspaceRoot, string(j.Handle.Ref.Execution), "workspace") {
		j.Cleanup = &Cleanup{At: s.now(), Error: "invalid execution cleanup ownership"}
		return
	}
	result := &Cleanup{At: s.now()}
	j.Cleanup = result
	fail := func(err error) {
		if err != nil && result.Error == "" {
			result.Error = "execution cleanup pending; retry stop after inspecting owned paths"
		}
	}
	if err := s.RevokeGitHub(*j); err != nil {
		result.Error = "authorization revocation pending; retry execution stop"
	}
	id := string(j.Handle.Ref.Execution)
	for _, p := range []string{filepath.Join(s.Config.WorkspaceRoot, id, "runtime"), s.agyMCPHomeDir(j.Handle.Ref), filepath.Join(s.Root, workerGitHubBinDir, id)} {
		root := s.Root
		if within(s.Config.WorkspaceRoot, p) {
			root = s.Config.WorkspaceRoot
		}
		if err := safeOwnedPath(root, p); err != nil {
			fail(err)
			continue
		}
		fail(worktrees.RemoveAll(p))
	}
	if result.Error != "" {
		return
	}
	parent := filepath.Join(s.Config.WorkspaceRoot, id)
	if err := safeOwnedPath(s.Config.WorkspaceRoot, parent); err != nil {
		fail(err)
		return
	}
	_, statErr := os.Lstat(parent)
	if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		fail(statErr)
		return
	}
	if !errors.Is(statErr, os.ErrNotExist) {
		// Every ref and reflog is included: a worker may have switched branches or
		// reset HEAD after creating an unpublished commit. Ignored files also count.
		workspace := filepath.Join(parent, "workspace")
		for _, path := range []string{workspace, filepath.Join(parent, "repository.git")} {
			if err := safeOwnedPath(s.Config.WorkspaceRoot, path); err != nil {
				fail(err)
				return
			}
		}
		children, err := os.ReadDir(parent)
		if err != nil {
			fail(err)
			return
		}
		for _, child := range children {
			if child.Name() != "workspace" && child.Name() != "repository.git" {
				result.Retained = true
				return
			}
		}
		status, err := s.git(ctx, workspace, "status", "--porcelain", "-z", "--untracked-files=all", "--ignored")
		commits, commitErr := s.git(ctx, workspace, "rev-list", "--all", "--reflog", "--not", j.Handle.Commit)
		disposable := true
		for _, entry := range strings.Split(string(status), "\x00") {
			if entry != "" && !strings.HasPrefix(entry, "!! .beekeeper-scratch/") {
				disposable = false
				break
			}
		}
		if err != nil || commitErr != nil || strings.TrimSpace(string(commits)) != "" || !disposable {
			result.Retained = true
			return
		}
		// The scratch directory is disposable only when the repository still
		// excludes it; other ignored files force retention above.
		fail(worktrees.RemoveAll(parent))
		if result.Error != "" {
			return
		}
	}
	log := filepath.Join(s.Root, id+".log")
	if fi, err := os.Lstat(log); err == nil && s.now().Sub(fi.ModTime()) > executionLogRetention {
		if err := safeOwnedPath(s.Root, log); err != nil {
			fail(err)
		} else {
			fail(os.Remove(log))
		}
	}
	source := filepath.Dir(s.sourcePath(j.Handle.Ref))
	if err := safeOwnedPath(s.Root, source); err != nil {
		fail(err)
	} else {
		fail(worktrees.RemoveAll(source))
	}
}

// AuditLeftovers is read-only. Unknown ownership never authorizes deletion.
func (s *Server) AuditLeftovers() ([]Leftover, error) {
	entries, err := os.ReadDir(s.Config.WorkspaceRoot)
	if err != nil {
		return nil, err
	}
	var findings []Leftover
	for _, entry := range entries {
		id := entry.Name()
		var j Journal
		err := readJSON(filepath.Join(s.Root, id+".json"), &j)
		f := Leftover{Execution: id, Class: "unknown", Reason: "workspace root has no verified execution journal", Action: "establish ownership before cleanup"}
		if err == nil && string(j.Handle.Ref.Execution) == id {
			if _, err = s.Read(j.Handle.Ref); err != nil {
				return nil, err
			}
			f.Generation = uint64(j.Handle.Ref.Generation)
			if !j.StopAcknowledged && j.Phase != Exited && j.Phase != Failed {
				f.Class = "active"
				f.Reason = "execution has not been retired"
				f.Action = "reconcile execution before cleanup"
				if j.betweenTurns() && !j.Completed {
					f.Class = "parked"
					f.Reason = "unfinished execution is between turns"
					f.Action = "resume or explicitly stop execution"
				}
			} else if j.Cleanup != nil && j.Cleanup.Retained {
				f.Class = "recovery"
				f.Reason = "work may be dirty or unpublished"
				f.Action = "publish or recover work before removal"
			} else {
				f.Class = "overdue"
				f.Reason = "terminal execution still has owned files"
				f.Action = "retry execution stop"
			}
			if j.Cleanup != nil && j.Cleanup.Error != "" {
				f.Class = "overdue"
				f.Reason = j.Cleanup.Error
				f.Action = "retry execution stop"
			}
		}
		if entry.Type()&os.ModeSymlink != 0 {
			f.Class = "unknown"
			f.Reason = "managed entry is a symlink"
			f.Action = "inspect ownership; do not follow link"
		}
		findings = append(findings, f)
	}
	for _, kind := range []string{"sources", workerGitHubBinDir} {
		managed := filepath.Join(s.Root, kind)
		if err := safeOwnedPath(s.Root, managed); err != nil {
			findings = append(findings, Leftover{Class: "unknown", Reason: "managed " + kind + " root failed containment checks", Action: "inspect ownership; do not follow link"})
			continue
		}
		entries, err := os.ReadDir(managed)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		for _, entry := range entries {
			var j Journal
			err := readJSON(filepath.Join(s.Root, entry.Name()+".json"), &j)
			if err != nil || string(j.Handle.Ref.Execution) != entry.Name() || entry.Type()&os.ModeSymlink != 0 {
				findings = append(findings, Leftover{Execution: entry.Name(), Class: "unknown", Reason: "unclassified managed " + kind + " entry", Action: "establish ownership before cleanup"})
			} else if j.StopAcknowledged && (j.Cleanup == nil || !j.Cleanup.Retained || kind == workerGitHubBinDir) {
				findings = append(findings, Leftover{Execution: entry.Name(), Generation: j.Handle.Ref.Generation, Class: "overdue", Reason: "terminal execution retains " + kind, Action: "retry execution stop"})
			}
		}
	}
	journals, err := filepath.Glob(filepath.Join(s.Root, "*.json"))
	if err != nil {
		return nil, err
	}
	for _, path := range journals {
		var j Journal
		if err := readJSON(path, &j); err != nil || j.Handle.Ref.Validate() != nil || j.Cleanup == nil || j.Cleanup.Error == "" {
			continue
		}
		findings = append(findings, Leftover{Execution: string(j.Handle.Ref.Execution), Generation: j.Handle.Ref.Generation, Class: "overdue", Reason: j.Cleanup.Error, Action: "retry execution stop"})
	}
	logs, err := filepath.Glob(filepath.Join(s.Root, "*.log"))
	if err != nil {
		return nil, err
	}
	for _, path := range logs {
		fi, err := os.Lstat(path)
		if err != nil {
			return nil, err
		}
		if s.now().Sub(fi.ModTime()) <= executionLogRetention {
			continue
		}
		id := strings.TrimSuffix(filepath.Base(path), ".log")
		var j Journal
		if err := readJSON(filepath.Join(s.Root, id+".json"), &j); err != nil || j.Handle.Ref.Validate() != nil {
			findings = append(findings, Leftover{Execution: id, Class: "unknown", Reason: "log has no verified execution journal", Action: "establish ownership before cleanup"})
			continue
		}
		if !j.StopAcknowledged && j.Phase != Exited && j.Phase != Failed || j.Cleanup != nil && j.Cleanup.Retained {
			continue
		}
		findings = append(findings, Leftover{Execution: id, Generation: j.Handle.Ref.Generation, Class: "overdue", Reason: "terminal log exceeds seven day retention", Action: "retry execution stop"})
	}
	return findings, nil
}

func (s *Server) workerGitHubEnvironment(j Journal, env []string) ([]string, error) {
	env, err := s.workerEnvironment(j, env)
	if err != nil {
		return nil, err
	}
	return s.GitHubEnvironment(j, env)
}

// retireTerminal preserves the observed exit/failure reason and serializes
// retirement with any concurrent control or start operation.
func (s *Server) retireTerminal(ctx context.Context, ref Ref) error {
	l, p, err := s.lock(ctx, ref)
	if err != nil {
		return err
	}
	defer func() { _ = l.Unlock() }()
	j, err := s.Read(ref)
	if err != nil {
		return err
	}
	if j.betweenTurns() && !j.Completed {
		return nil
	}
	if s.Launcher == nil {
		return errors.New("cannot confirm terminal execution supervisor state")
	}
	state := s.Launcher.State(ctx, j.Handle.Unit)
	if state != unitInactive && state != Failed {
		return nil
	}
	if j.Phase != Exited && j.Phase != Failed {
		if j.Completed {
			j.Phase = Exited
		} else {
			j.Phase = Failed
			if j.LastError == "" {
				j.LastError = "owned harness ended unexpectedly"
			}
		}
		j.InTurn = false
	}
	s.retireLocked(ctx, &j)
	return atomicFile(p, mustJSON(j))
}
