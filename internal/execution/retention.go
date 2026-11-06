package execution

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"
)

const LogRetention = 30 * 24 * time.Hour

// homeLeftoverBound caps the home state entries one audit names.
const homeLeftoverBound = 256

// The audit states of what stays on a host past or beside an execution.
const (
	resourceShared   = "shared"
	resourceTerminal = "terminal"
)

const claudeSettingsFile = "settings.json"
const claudeRulesFile = "CLAUDE.md"
const unitReloading = "reloading"

// ResourceFinding contains metadata only, never journal secrets or file contents.
type ResourceFinding struct {
	Orphan      bool      `json:"orphan,omitempty"`
	Ref         Ref       `json:"ref"`
	Coordinator string    `json:"coordinator,omitempty"`
	Path        string    `json:"path"`
	State       string    `json:"state"`
	Reason      string    `json:"reason"`
	Retry       string    `json:"retry"`
	RetainedAt  time.Time `json:"retainedAt,omitzero"`
	EligibleAt  time.Time `json:"eligibleAt,omitzero"`
}

// AuditResources does not call Inspect (which can acknowledge a stop), create
// directories, or read credentials. Unknown ownership always blocks mutation.
func (s *Server) AuditResources(ctx context.Context) ([]ResourceFinding, error) {
	real, err := filepath.EvalSymlinks(s.Root)
	if err != nil {
		return nil, err
	}
	if real != s.Root {
		return nil, errors.New("managed root resolves through a symlink")
	}
	rootDir, err := os.Open(s.Root)
	if err != nil {
		return nil, err
	}
	entries, err := rootDir.ReadDir(4097)
	_ = rootDir.Close()
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	if len(entries) > 4096 {
		return nil, errors.New("resource inventory exceeds audit bound")
	}
	findings := []ResourceFinding{}
	owners := map[string]Journal{}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") || e.Name() == "policy.json" {
			continue
		}
		// Only execution journal filenames may be decoded. Other JSON files
		// can contain shared authentication data and remain unclassified.
		candidate := Ref{Host: s.Config.Identity, Execution: ExecutionID(strings.TrimSuffix(e.Name(), ".json")), Generation: 1}
		if candidate.Validate() != nil || !e.Type().IsRegular() {
			continue
		}
		var j Journal
		if readJSON(filepath.Join(s.Root, e.Name()), &j) != nil {
			continue
		}
		if e.Name() != string(j.Handle.Ref.Execution)+".json" {
			continue
		}
		// Validate ownership without Read, whose journalPath creates directories.
		if j.Handle.Ref.Validate() != nil || j.Handle.Ref.Host != s.Config.Identity || j.Request.Ref != j.Handle.Ref || j.Request.Digest() != j.Digest || j.Handle.Unit != UnitName(j.Handle.Ref) || j.Handle.ConversationID != j.Request.ConversationID {
			continue
		}
		if _, ok := s.Config.Coordinators[j.Request.Coordinator]; !ok {
			continue
		}
		owners[string(j.Handle.Ref.Execution)] = j
	}
	// Inventory immediate children only of explicitly managed containers. Never
	// traverse execution contents or follow symlinks into home/tmp/source trees.
	paths := map[string]os.DirEntry{}
	for _, e := range entries {
		paths[filepath.Join(s.Root, e.Name())] = e
	}
	retiredRoots := []string{filepath.Join(s.Config.WorkspaceRoot, retiredWorkspaces), filepath.Join(s.Root, retiredResources), filepath.Join(s.Root, recoveryBundles)}
	roots := append([]string{s.Config.WorkspaceRoot, filepath.Join(s.Root, "sources"), filepath.Join(s.Root, "control"), filepath.Join(s.Root, "github-bin"), filepath.Join(s.Root, bundlesDir)}, retiredRoots...)
	for _, root := range roots {
		if root == "" {
			continue
		}
		real, err := filepath.EvalSymlinks(root)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil || real != root {
			findings = append(findings, ResourceFinding{Path: root, State: Unknown, Reason: "managed container unavailable or symlinked", Retry: "establish ownership; do not delete"})
			continue
		}
		dir, err := os.Open(filepath.Clean(root))
		if err != nil {
			return nil, err
		}
		children, err := dir.ReadDir(4097)
		_ = dir.Close()
		if err != nil && !errors.Is(err, io.EOF) {
			return nil, err
		}
		if len(paths)+len(children) > 4096 {
			return nil, errors.New("resource inventory exceeds audit bound")
		}
		for _, e := range children {
			paths[filepath.Join(root, e.Name())] = e
		}
	}
	names := make([]string, 0, len(paths))
	for p := range paths {
		names = append(names, p)
	}
	sort.Strings(names)
	states := map[string]string{}
	live := map[string]bool{}
	for id, j := range owners {
		if s.Launcher != nil {
			states[id] = s.Launcher.State(ctx, j.Handle.Unit)
		}
		if j.Retirement == nil && j.Request.BundleDigest != "" {
			live[j.Request.BundleDigest] = true
		}
	}
	for _, path := range names {
		e := paths[path]
		row := ResourceFinding{Path: path, State: Unknown, Reason: "unclassified managed entry; ownership unproven", Retry: "establish ownership; do not delete"}
		name := e.Name()
		// Match only the resource names created in each managed container.
		// Suffix chains and UUID-prefixed unrelated files prove no ownership.
		id := ""
		if filepath.Dir(path) == s.Root {
			for owner := range owners {
				if name == owner+".json" || name == owner+".json.lock" || name == owner+".log" || name == owner+"-agy-home" {
					id = owner
					break
				}
				if receipt, ok := strings.CutPrefix(name, owner+"-"); ok {
					if message, ok := strings.CutSuffix(receipt, ".accepted"); ok && ValidIdentity(message) {
						id = owner
						break
					}
				}
			}
		} else if slices.Contains(retiredRoots, filepath.Dir(path)) {
			id = retiredOwner(name, owners)
		} else if e.IsDir() {
			id = name
		}

		if j, ok := owners[id]; ok && e.Type()&os.ModeSymlink == 0 {
			row.Ref, row.Coordinator = j.Handle.Ref, j.Request.Coordinator
			row.RetainedAt = j.RetainedAt
			if !j.RetainedAt.IsZero() {
				row.EligibleAt = j.RetainedAt.Add(LogRetention)
			}
			row.State, row.Reason, row.Retry = Unknown, "supervisor state unavailable", "retry hosts check when host supervisor is reachable"
			if s.Launcher != nil {
				switch states[id] {
				case unitActive, Activating, unitReloading, "deactivating": //nolint:goconst // Mirror supervisor lifecycle states in the read-only audit.
					row.State, row.Reason, row.Retry = "active", "owned execution in progress", "leave active execution in place"
					if !j.InTurn {
						row.State = "parked"
					}
				case unitInactive, Failed:
					row.State, row.Reason, row.Retry = resourceTerminal, "recovery artifacts retained; publication is not proven by exit", fmt.Sprintf("review recovery evidence, then beekeeper agents hosts reconcile <host> %s %d", j.Handle.Ref.Execution, j.Handle.Ref.Generation)
					if !j.StopAcknowledged || name == id+"-agy-home" {
						row.Reason = "overdue lifecycle cleanup"
					}

					if !j.RetainedAt.IsZero() && name == id+".log" {
						row.Reason = "managed log retention"
						if !s.now().Before(j.RetainedAt.Add(LogRetention)) {
							row.Reason = "log past retention; review/recovery release required"
						}
					}
					if j.RevocationFailed {
						row.Reason = "failed credential revocation"
					}
				}
			}
			if name != id+".log" {
				retirementRow(&row, j, s.now())
			}
		}
		// These are shared policy/runtime roots, never per-execution garbage.
		if filepath.Dir(path) == s.Root {
			switch name {
			case "policy.json", claudeRulesFile, claudeSettingsFile, "omp-guard.mjs", "codex-home", "agy-home", "sources", "control", "github-bin", admissionLock, bundlesDir, retiredResources, recoveryBundles: //nolint:goconst // Explicit inventory of shared on-disk names.
				if e.Type()&os.ModeSymlink == 0 {
					row.State = resourceShared
					row.Reason = "shared policy, toolchain or retained recovery container"
					row.Retry = "host owner reviews contents; no recursive deletion"
				}
			}
		}
		if path == retiredRoots[0] && e.Type()&os.ModeSymlink == 0 {
			row.State, row.Reason, row.Retry = resourceShared, "retired workspace container", "host owner reviews contents; no recursive deletion"
		}
		if filepath.Dir(path) == filepath.Join(s.Root, bundlesDir) {
			s.bundleRow(&row, e, live)
		}
		findings = append(findings, row)
		if filepath.Dir(path) == s.Config.WorkspaceRoot && e.IsDir() && e.Type()&os.ModeSymlink == 0 {
			repository := filepath.Join(path, "repository.git")
			if real, err := filepath.EvalSymlinks(repository); err == nil && real == repository {
				refs, err := s.git(ctx, repository, "for-each-ref", "--count=4097", "--format=%(refname)", "refs/heads/beekeeper-worker/")
				if err != nil {
					findings = append(findings, ResourceFinding{Path: repository, State: Unknown, Reason: "cannot inventory managed refs", Retry: "restore repository access; do not delete"})
				} else {
					for _, ref := range strings.Fields(string(refs)) {
						refRow := row
						refRow.Path = repository + ":" + ref
						if ref != "refs/heads/beekeeper-worker/"+id {
							refRow.Ref = Ref{}
							refRow.Coordinator = ""
							refRow.State = Unknown
							refRow.Reason = "orphan managed ref; ownership unproven"
							refRow.Retry = "establish ref ownership; retain unpublished commits"
						}
						findings = append(findings, refRow)
						if len(findings) > 4096 {
							return nil, errors.New("resource inventory exceeds audit bound")
						}
					}
				}
			}
		}
	}
	sharedPaths := map[string]string{}
	for repo, path := range s.Config.Repositories {
		sharedPaths[path] = "shared source for " + repo + "; host owner retains"
	}
	for name, harness := range s.Config.Harnesses {
		if harness.ConfigDir != "" {
			sharedPaths[harness.ConfigDir] = "shared authentication location for " + name + "; contents not inspected"
		}
	}
	for name, tool := range s.Config.Tools {
		sharedPaths[tool.Executable] = "shared toolchain " + name + "; host owner retains"
	}
	for path, reason := range sharedPaths {
		findings = append(findings, ResourceFinding{Path: path, State: resourceShared, Reason: reason, Retry: "host owner manages shared lifetime; no execution cleanup"})
	}
	findings = append(findings, s.homeLeftovers()...)
	if len(findings) > 4096 {
		return nil, errors.New("resource inventory exceeds audit bound")
	}
	sort.Slice(findings, func(i, j int) bool { return findings[i].Path < findings[j].Path })
	return findings, nil
}

// retiredOwner names the execution a moved-aside entry belongs to. Entries are
// labelled <execution>-<generation>, a recovery bundle with a .bundle suffix; a
// name that matches no journal proves no ownership.
func retiredOwner(name string, owners map[string]Journal) string {
	label := strings.TrimSuffix(name, ".bundle")
	for id, j := range owners {
		if label == retiredLabel(j.Handle.Ref) {
			return id
		}
	}
	return ""
}

// retirementRow overrides row with the retirement j entered: what moved aside is
// accounted for by its stage and retention, and a retirement that stopped halfway
// is overdue under its recorded error.
func retirementRow(row *ResourceFinding, j Journal, now time.Time) {
	x := j.Retirement
	if x == nil {
		return
	}
	retry := fmt.Sprintf("beekeeper agents hosts reconcile <host> %s %d", j.Handle.Ref.Execution, j.Handle.Ref.Generation)
	if !x.Finished() {
		row.State, row.Reason, row.Retry = resourceTerminal, "overdue lifecycle cleanup: retirement stopped at "+x.Stage, retry
		if x.LastError != "" {
			row.Reason += ": " + x.LastError
		}
		return
	}
	if j.RevocationFailed {
		row.State, row.Reason, row.Retry = resourceTerminal, "failed credential revocation", retry
		return
	}
	row.State, row.RetainedAt, row.EligibleAt = "retired", x.RetiredAt, time.Time{}
	switch {
	case !j.PurgedAt.IsZero():
		row.Reason, row.Retry = "retired evidence kept after its resources were removed", "desk reviews evidence; no automatic removal"
	case x.Recovery != nil:
		row.Reason, row.Retry = "retained recovery work", "publish or recover the work; it has no expiry"
	case !j.Disposable:
		row.Reason, row.Retry = "retained without a disposal proof", "desk reviews it; it has no expiry"
	case now.Before(x.RetainUntil):
		row.Reason, row.Retry, row.EligibleAt = "managed retention", "none: a later retirement removes it", x.RetainUntil
	default:
		row.Reason, row.Retry, row.EligibleAt = "past retention; removal pending", "the next retirement or "+retry+" removes it", x.RetainUntil
	}
}

// homeLeftovers lists by name the immediate children of the account's XDG state
// directory, where a worker that wrote a literal ~ path leaves what no execution
// root holds. Nothing proves their owner, so they are reported and never touched,
// and neither their contents nor anything else under home is read.
func (s *Server) homeLeftovers() []ResourceFinding {
	incomplete := func(path, reason string) ResourceFinding {
		return ResourceFinding{Path: path, State: Unknown, Reason: reason, Retry: "host owner reviews the directory; a clean audit cannot be claimed"}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return []ResourceFinding{incomplete("~", "home state unreadable: the account has no home")}
	}
	root := filepath.Join(home, ".local", "state")
	dir, err := os.Open(filepath.Clean(root)) //nolint:gosec // fixed location under the host account's home
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return []ResourceFinding{incomplete(root, "home state unreadable")}
	}
	defer func() { _ = dir.Close() }()
	entries, err := dir.ReadDir(homeLeftoverBound + 1)
	if err != nil && !errors.Is(err, io.EOF) {
		return []ResourceFinding{incomplete(root, "home state unreadable")}
	}
	rows := []ResourceFinding{}
	if len(entries) > homeLeftoverBound {
		entries = entries[:homeLeftoverBound]
		rows = append(rows, incomplete(root, fmt.Sprintf("home state holds more than %d entries; the rest are not listed", homeLeftoverBound)))
	}
	for _, e := range entries {
		path := filepath.Join(root, e.Name())
		if within(path, s.Root) {
			continue
		}
		rows = append(rows, ResourceFinding{Path: path, State: Unknown, Reason: "unclassified home state; no execution owns it", Retry: "host owner establishes provenance; no automatic removal"})
	}
	return rows
}

// ReconcileResource is intentionally one execution per call. The execution lock
// serializes with starts. It enters the one retirement every terminal path uses,
// so unpublished work is preserved before anything moves aside, then removes what
// earlier retirements proved disposable. ReleaseEvidence is an explicit desk
// attestation that this log is no longer needed for publication review or recovery.
func (s *Server) ReconcileResource(ctx context.Context, ref Ref, releaseEvidence bool) error {
	j, err := s.Read(ref)
	if err != nil {
		return err
	}
	if _, ok := s.Config.Coordinators[j.Request.Coordinator]; !ok {
		return errors.New("unknown execution owner")
	}
	if s.Launcher == nil {
		return errors.New("supervisor unavailable")
	}
	status := s.Launcher.State(ctx, j.Handle.Unit)
	if status != unitInactive && status != Failed {
		return errors.New("execution is active or unknown")
	}
	if _, err = s.stop(ctx, ref, StopRequest{ID: "resource-retention", Reason: "terminal resource reconciliation"}, true); err != nil {
		return err
	}
	var revokeErr error
	err = s.update(ctx, ref, func(j *Journal) error {
		status := s.Launcher.State(ctx, j.Handle.Unit)
		if status != unitInactive && status != Failed {
			return errors.New("execution state changed")
		}
		revokeErr = s.RevokeGitHub(*j)
		if revokeErr != nil {
			j.RevocationFailed = true
			return nil
		}
		j.RevocationFailed = false
		if j.RetainedAt.IsZero() {
			j.RetainedAt = s.now()
			return nil
		}
		if !releaseEvidence || s.now().Before(j.RetainedAt.Add(LogRetention)) {
			return nil
		}
		p := filepath.Join(s.Root, string(ref.Execution)+".log")
		fi, err := os.Lstat(p)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if !fi.Mode().IsRegular() {
			return errors.New("retained log is not a regular file")
		}
		return os.Remove(p)
	})
	if revokeErr != nil {
		return errors.Join(err, errors.New("credential revocation failed; retry reconciliation"))
	}
	if err != nil {
		return err
	}
	// Last, so work that cannot be preserved never holds back a revocation.
	_, err = s.Retire(ctx, ref, RetireRequest{ID: "resource-retention", Trigger: RetireTriggerStop, Reason: "terminal resource reconciliation"})
	return err
}

// ReportOrphans joins the host's resources with a desk's registered execution
// inventory. Other coordinators' resources are accounted for, never adopted.
// An orphan remains in its observed active/parked/terminal/unknown state.
func ReportOrphans(rows []ResourceFinding, coordinator string, registered []Ref) {
	known := make(map[Ref]bool, len(registered))
	for _, ref := range registered {
		known[ref] = true
	}
	for i := range rows {
		row := &rows[i]
		if row.Coordinator != coordinator || row.Ref.Validate() != nil || known[row.Ref] {
			continue
		}
		row.Orphan = true
		row.Reason = "orphan owned resource absent from coordinator inventory; " + row.Reason
		row.Retry = "restore coordinator registration and review recovery evidence before explicit reconciliation"
	}
}
