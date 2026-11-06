package execution

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/gofrs/flock"
	"github.com/google/uuid"

	"github.com/giantswarm/beekeeper/internal/worktrees"
)

const (
	bundleStatusOperation = "bundle-status"
	bundleSyncOperation   = "bundle-sync"

	bundlesDir      = "bundles"
	bundleManifest  = "bundle.json"
	bundleStaging   = ".partial"
	codexBundleHome = "codex-home"
	// maxInstalledBundles bounds the bundles a host reads for a status or a
	// prepare; a host past it refuses instead of silently skipping any.
	maxInstalledBundles = 256
	// bundlePruneBatch bounds the unbound bundles one sync removes.
	bundlePruneBatch = 8
)

// BundleStatus is the host's answer about one bundle digest. Installed says that
// digest is whole and unchanged; Problem says why one that exists cannot be used.
// Latest is the newest whole bundle, and Artifacts its path to content digest:
// metadata only, so a coordinator can name what differs.
type BundleStatus struct {
	Digest    string            `json:"digest"`
	Installed bool              `json:"installed"`
	Problem   string            `json:"problem,omitempty"`
	Latest    string            `json:"latest,omitempty"`
	Artifacts map[string]string `json:"artifacts,omitempty"`
}

// BundleReport is the coordinator's comparison in hosts check. Differs names the
// artifacts of the expected bundle the host's newest one lacks or holds otherwise.
type BundleReport struct {
	Expected  string   `json:"expected"`
	Installed string   `json:"installed,omitempty"`
	Current   bool     `json:"current"`
	Differs   []string `json:"differs,omitempty"`
	Problem   string   `json:"problem,omitempty"`
}

// Compare reports expected against the host's status. A host behind is not a
// fault: the next start transfers the bundle.
func (b CoordinatorBundle) Compare(st BundleStatus) BundleReport {
	r := BundleReport{Expected: b.Digest(), Installed: st.Latest, Current: st.Installed, Problem: st.Problem}
	if st.Installed {
		r.Installed = st.Digest
		return r
	}
	seen := map[string]bool{}
	for _, a := range b.Artifacts {
		seen[a.Artifact] = true
		if st.Artifacts[a.Artifact] != a.Digest {
			r.Differs = append(r.Differs, a.Artifact)
		}
	}
	for artifact := range st.Artifacts {
		if !seen[artifact] {
			r.Differs = append(r.Differs, artifact)
		}
	}
	sort.Strings(r.Differs)
	return r
}

// BundleSyncer is the optional host operation behind a start's synchronization.
type BundleSyncer interface {
	BundleStatus(context.Context, string) (BundleStatus, error)
	SyncBundle(context.Context, CoordinatorBundle) (string, error)
}

func (t SSHTransport) BundleStatus(ctx context.Context, digest string) (BundleStatus, error) {
	var x BundleStatus
	err := t.Call(ctx, bundleStatusOperation, Ref{}, digest, &x)
	return x, err
}

func (t SSHTransport) SyncBundle(ctx context.Context, b CoordinatorBundle) (string, error) {
	var digest string
	err := t.Call(ctx, bundleSyncOperation, Ref{}, b, &digest)
	if err == nil && digest != b.Digest() {
		err = errors.New("host installed another bundle than the one sent")
	}
	return digest, err
}

// SyncCurrentBundle makes b available on the host and returns the digest a start
// binds. It transfers only what the host lacks; any failure starts no worker.
func SyncCurrentBundle(ctx context.Context, c BundleSyncer, b CoordinatorBundle) (string, error) {
	digest := b.Digest()
	st, err := c.BundleStatus(ctx, digest)
	if err != nil {
		return "", err
	}
	if st.Problem != "" {
		return "", errors.New(st.Problem)
	}
	if st.Installed {
		return digest, nil
	}
	return c.SyncBundle(ctx, b)
}

func (s *Server) bundleDir(digest string) string {
	return filepath.Join(s.Root, bundlesDir, strings.TrimPrefix(digest, "sha256:"))
}

// rulesFile is the system prompt file of a claude or omp worker, and the source
// of an agy worker's GEMINI.md when it runs under a bundle.
func (s *Server) rulesFile() string {
	if s.coordinatorBundle != nil {
		return filepath.Join(s.bundleDir(s.coordinatorBundle.Digest()), claudeRulesFile)
	}
	return filepath.Join(s.Root, claudeRulesFile)
}

// withBundle is this server bound to a private snapshot of b.
func (s *Server) withBundle(b CoordinatorBundle) (*Server, error) {
	if err := b.Validate(); err != nil {
		return nil, err
	}
	totalRules := len(s.Rules)
	for _, a := range b.Artifacts {
		if strings.HasPrefix(a.Artifact, bundleRulesKind+"/") {
			totalRules += len("\n\nCoordinator rules:\n") + len(a.Content)
		}
	}
	if totalRules > maxArtifactBytes {
		return nil, errors.New("combined host and coordinator rules exceed the worker document limit")
	}
	var snapshot CoordinatorBundle
	if err := Decode(mustJSON(b), &snapshot); err != nil {
		return nil, err
	}
	bound := *s
	bound.coordinatorBundle = &snapshot
	return &bound, nil
}

// boundRules are the host's rules followed by the bundle's, with each skill's
// files inline: only codex reads skills from its home.
func (s *Server) boundRules() []byte {
	rules := append([]byte(nil), s.Rules...)
	for _, a := range s.coordinatorBundle.Artifacts {
		heading := "\n\nCoordinator " + a.Artifact + ":\n"
		if strings.HasPrefix(a.Artifact, bundleRulesKind+"/") {
			heading = "\n\nCoordinator rules:\n"
		}
		rules = append(append(rules, heading...), a.Content...)
	}
	return rules
}

// bundleFiles are the managed files of a bound server's bundle directory.
func (s *Server) bundleFiles() map[string][]byte {
	files := map[string][]byte{bundleManifest: mustJSON(s.coordinatorBundle), claudeRulesFile: s.boundRules()}
	if _, ok := s.Config.Harnesses[HarnessCodex]; ok {
		for name, b := range s.codexFiles() {
			files[filepath.Join(codexBundleHome, name)] = b
		}
	}
	return files
}

func writeBundleFiles(dir string, files map[string][]byte) error {
	for name, b := range files {
		p := filepath.Join(dir, name)
		if err := ownedDirectory(filepath.Dir(p)); err != nil {
			return err
		}
		if err := atomicFile(p, b); err != nil {
			return err
		}
	}
	return nil
}

// verifyBundle reads every managed file of a bound server's directory back
// against what its bundle and the host's rules make it.
func (s *Server) verifyBundle() error {
	dir := s.bundleDir(s.coordinatorBundle.Digest())
	if resolved, err := filepath.EvalSymlinks(dir); err != nil || resolved != dir {
		return errors.New("coordinator bundle is not installed in the host's managed directory")
	}
	for name, want := range s.bundleFiles() {
		b, err := readRegular(filepath.Join(dir, name), MaxWire)
		if err != nil || Digest(b) != Digest(want) {
			return fmt.Errorf("installed coordinator bundle %s does not match its digest: %s changed", s.coordinatorBundle.Digest(), name)
		}
	}
	return nil
}

// readBundle reads the manifest a directory holds under the digest it claims.
func (s *Server) readBundle(digest string) (CoordinatorBundle, error) {
	var b CoordinatorBundle
	data, err := readRegular(filepath.Join(s.bundleDir(digest), bundleManifest), MaxWire)
	if err != nil {
		return b, err
	}
	if err := Decode(data, &b); err != nil {
		return b, errors.New("installed coordinator bundle is unreadable")
	}
	if b.Digest() != digest {
		return b, fmt.Errorf("installed coordinator bundle %s does not match its digest", digest)
	}
	return b, nil
}

// boundTo is the server of one execution: on digest's bundle, verified whole, or
// this server for an execution that has none. A digest the host lacks fails.
func (s *Server) boundTo(digest string) (*Server, error) {
	if digest == "" {
		return s, nil
	}
	b, err := s.readBundle(digest)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("coordinator bundle %s is not installed on this host", digest)
	}
	if err != nil {
		return nil, err
	}
	bound, err := s.withBundle(b)
	if err != nil {
		return nil, err
	}
	return bound, bound.verifyBundle()
}

// installedBundles are the digests of the host's bundle directories, newest first.
func (s *Server) installedBundles() ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(s.Root, bundlesDir))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if len(entries) > maxInstalledBundles {
		return nil, errors.New("installed coordinator bundles exceed their bound")
	}
	type installed struct {
		digest string
		at     time.Time
	}
	var found []installed
	for _, e := range entries {
		digest := "sha256:" + e.Name()
		if !e.IsDir() || !ValidDigest(digest) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			return nil, err
		}
		found = append(found, installed{digest, info.ModTime()})
	}
	sort.Slice(found, func(i, j int) bool { return found[i].at.After(found[j].at) })
	digests := make([]string, 0, len(found))
	for _, f := range found {
		digests = append(digests, f.digest)
	}
	return digests, nil
}

// BundleStatus never repairs: a changed bundle stays a Problem until an operator
// looks, since an execution may be bound to it.
func (s *Server) BundleStatus(digest string) (BundleStatus, error) {
	st := BundleStatus{Digest: digest}
	if !ValidDigest(digest) {
		return st, errors.New("invalid coordinator bundle digest")
	}
	if _, err := s.boundTo(digest); err == nil {
		st.Installed = true
	} else if _, statErr := os.Lstat(s.bundleDir(digest)); statErr == nil {
		st.Problem = err.Error()
	}
	digests, err := s.installedBundles()
	if err != nil {
		return st, err
	}
	for _, candidate := range digests {
		b, err := s.readBundle(candidate)
		if err != nil {
			continue
		}
		st.Latest, st.Artifacts = candidate, map[string]string{}
		for _, a := range b.Artifacts {
			st.Artifacts[a.Artifact] = a.Digest
		}
		break
	}
	return st, nil
}

// InstallBundle publishes a bundle under its digest with one rename, so a start
// or a concurrent sync sees all of it or none. Repeating it finds it installed. It
// holds the host's admission and only ever adds: what an execution is bound to
// stays as it was.
func (s *Server) InstallBundle(ctx context.Context, b CoordinatorBundle) (string, error) {
	bound, err := s.withBundle(b)
	if err != nil {
		return "", err
	}
	if err := ownedDirectory(filepath.Join(s.Root, bundlesDir)); err != nil {
		return "", err
	}
	admission := flock.New(filepath.Join(s.Root, admissionLock))
	wait, cancel := context.WithTimeout(ctx, quiesceWait)
	defer cancel()
	if ok, err := admission.TryLockContext(wait, 20*time.Millisecond); err != nil || !ok {
		return "", errors.New("host admission is busy")
	}
	defer func() { _ = admission.Unlock() }()
	digest := b.Digest()
	dir := s.bundleDir(digest)
	if _, err := os.Lstat(dir); err == nil {
		return digest, bound.verifyBundle()
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	stage := dir + bundleStaging
	if err := worktrees.RemoveAll(stage); err != nil {
		return "", err
	}
	if err := ownedDirectory(stage); err != nil {
		return "", err
	}
	if err := writeBundleFiles(stage, bound.bundleFiles()); err != nil {
		return "", err
	}
	if err := os.Rename(stage, dir); err != nil {
		return "", err
	}
	if err := syncDirectory(filepath.Dir(dir)); err != nil {
		return "", err
	}
	if err := bound.linkCodexLogin(); err != nil {
		return "", err
	}
	s.pruneBundles(digest)
	return digest, nil
}

// refreshBundles rewrites the managed files of every whole bundle for the rules
// and guards a prepare installs, leaving what workers wrote there: a parked codex
// thread resumes in the same home on the updated host.
func (s *Server) refreshBundles() error {
	digests, err := s.installedBundles()
	if err != nil {
		return err
	}
	for _, digest := range digests {
		b, err := s.readBundle(digest)
		if err != nil {
			continue
		}
		bound, err := s.withBundle(b)
		if err != nil {
			return err
		}
		if err := writeBundleFiles(s.bundleDir(digest), bound.bundleFiles()); err != nil {
			return err
		}
	}
	return nil
}

// bundlesInUse are the bundles of executions that have not entered retirement.
// An unreadable journal could name any of them, so it fails the whole answer.
func (s *Server) bundlesInUse() (map[string]bool, error) {
	paths, err := s.journalFiles()
	if err != nil {
		return nil, err
	}
	used := map[string]bool{}
	for _, p := range paths {
		if _, err := uuid.Parse(strings.TrimSuffix(filepath.Base(p), ".json")); err != nil {
			continue
		}
		var j Journal
		if err := readJSON(p, &j); err != nil {
			return nil, err
		}
		if j.Retirement == nil && j.Request.BundleDigest != "" {
			used[j.Request.BundleDigest] = true
		}
	}
	return used, nil
}

// pruneBundles removes bundles no live execution is bound to once they are past
// the host's retention, a few per sync. The caller holds the admission, so no
// start binds one meanwhile; a failure leaves them for the next sync.
func (s *Server) pruneBundles(keep string) {
	used, err := s.bundlesInUse()
	if err != nil {
		return
	}
	digests, err := s.installedBundles()
	if err != nil {
		return
	}
	slices.Reverse(digests)
	pruned := 0
	for _, digest := range digests {
		info, err := os.Lstat(s.bundleDir(digest))
		if pruned == bundlePruneBatch || err != nil || digest == keep || used[digest] || s.now().Before(info.ModTime().Add(s.Config.retention())) {
			continue
		}
		if worktrees.RemoveAll(s.bundleDir(digest)) == nil {
			pruned++
		}
	}
}

// bundleRow classifies one entry of the bundle container. live holds the bundles
// of unretired executions.
func (s *Server) bundleRow(row *ResourceFinding, e os.DirEntry, live map[string]bool) {
	digest := "sha256:" + e.Name()
	if !e.IsDir() || !ValidDigest(digest) {
		return
	}
	row.State, row.Reason, row.Retry = "shared", "coordinator policy bundle bound to a live execution", "leave it: its worker runs under it"
	if live[digest] {
		return
	}
	row.Reason, row.Retry = "coordinator policy bundle no live execution is bound to", "a later bundle sync removes it past retention"
	if info, err := e.Info(); err == nil {
		row.EligibleAt = info.ModTime().Add(s.Config.retention())
	}
}
