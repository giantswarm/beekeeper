package remotebuild

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/gofrs/flock"
)

// builderRecord stays on the initiating machine. No host names or worktree
// paths need to be sent to the cluster to identify its builders.
type builderRecord struct {
	Root       string
	Host       string
	Kubeconfig string
	Context    string
	Namespace  string
}

func builderName(host, root string) string {
	sum := sha256.Sum256([]byte(host + "\x00" + root))
	return "bkb-" + hex.EncodeToString(sum[:12])
}

func (r Runner) remember(root, host, name string) error {
	dir := filepath.Join(r.StateDir, "remote-build")
	record := builderRecord{root, host, r.Config.Kubeconfig, r.Config.Context, r.Config.Namespace}
	raw, err := json.Marshal(record)
	if err != nil {
		return err
	}
	// The worktree lock serializes readers and writers of this record.
	return os.WriteFile(filepath.Join(dir, name+".json"), raw, 0o600)
}

// Sweep retires this machine's builders for missing or explicitly finished
// worktrees. Busy builders keep their journal and retry on the next sweep.
// It never lists or deletes another machine's jobs, or touches shared caches.
func (r Runner) Sweep(ctx context.Context, finished []string, dryRun bool) error {
	dir := filepath.Join(r.StateDir, "remote-build")
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	host, err := os.Hostname()
	if err != nil {
		return err
	}
	done := map[string]bool{}
	for _, root := range finished {
		root = filepath.Clean(root)
		if physical, e := filepath.EvalSymlinks(root); e == nil {
			root = physical
		}
		done[root] = true
	}
	var errs []error
	for _, entry := range entries {
		name, ok := strings.CutSuffix(entry.Name(), ".json")
		if !ok || !strings.HasPrefix(name, "bkb-") || entry.IsDir() {
			continue
		}
		lock := flock.New(filepath.Join(dir, name+".lock"))
		acquired, e := lock.TryLock()
		if e != nil {
			errs = append(errs, e)
			continue
		}
		if !acquired {
			continue
		}
		e = func() error {
			defer func() { _ = lock.Unlock() }()
			path := filepath.Join(dir, entry.Name())
			file, err := os.OpenInRoot(dir, entry.Name())
			if err != nil {
				return err
			}
			defer func() { _ = file.Close() }()
			raw, err := io.ReadAll(file)
			if err != nil {
				return err
			}
			var rec builderRecord
			if json.Unmarshal(raw, &rec) != nil {
				return fmt.Errorf("invalid builder ownership record %s", name)
			}
			if rec.Host != host {
				return nil
			}
			if !filepath.IsAbs(rec.Root) || builderName(host, rec.Root) != name {
				return fmt.Errorf("invalid builder ownership record %s", name)
			}
			_, statErr := os.Stat(rec.Root)
			if !done[rec.Root] && !errors.Is(statErr, os.ErrNotExist) {
				return statErr
			}
			if dryRun {
				if r.Stdout != nil {
					_, _ = fmt.Fprintf(r.Stdout, "worktrees: would retire builder %s\n", name)
				}
				return nil
			}
			if rec.Kubeconfig == "" || rec.Context == "" || rec.Namespace == "" {
				return errors.New("incomplete builder connection record")
			}
			target := r
			target.Config.Kubeconfig, target.Config.Context, target.Config.Namespace = rec.Kubeconfig, rec.Context, rec.Namespace
			// A successful delete (including already absent) is required before
			// forgetting ownership, so transient outages are retried later.
			if err := target.kubectl(ctx, nil, io.Discard, "delete", "job", name, "--ignore-not-found=true", "--wait=false"); err != nil {
				return fmt.Errorf("cannot retire builder %s", name)
			}
			if err := os.Remove(path); err != nil {
				return err
			}
			if r.Stdout != nil {
				_, _ = fmt.Fprintf(r.Stdout, "worktrees: retired builder %s\n", name)
			}
			return nil
		}()
		errs = append(errs, e)
	}
	return errors.Join(errs...)
}
