package worktrees

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// PublicationCache keeps only successful graph queries on immutable commits.
// Remote advertisements live for one scan; cleanup always reads them again.
// Status, locks, liveness and removal checks are never cached.
type PublicationCache map[string]string

// Wrap shares a remote advertisement across linked worktrees of a repository
// during this scan. Graph results survive scans, keyed by repository, HEAD,
// every remote tip and the shallow boundary. Unreadable metadata bypasses it.
func (cache PublicationCache) Wrap(git Git) Git {
	remotes := map[string][]byte{}
	boundaries := map[string]string{}
	return func(ctx context.Context, dir string, args ...string) ([]byte, error) {
		remote := len(args) == 3 && args[0] == "ls-remote" && args[1] == "--heads" && args[2] == "origin"
		graph := len(args) >= 5 && args[0] == "rev-list" && args[1] == "--max-count=1" && args[2] == "--ignore-missing"
		if !remote && !graph {
			return git(ctx, dir, args...)
		}
		common, boundary, ok := publicationRepository(dir)
		if !ok {
			return git(ctx, dir, args...)
		}
		if remote {
			if out, ok := remotes[common]; ok {
				return out, nil
			}
			out, err := git(ctx, dir, args...)
			if err == nil {
				remotes[common] = out
			}
			return out, err
		}
		objects, ok := boundaries[common]
		if !ok {
			var err error
			objects, err = publicationObjects(common)
			if err != nil {
				return git(ctx, dir, args...)
			}
			boundaries[common] = objects
		}
		boundary += objects
		key := fmt.Sprintf("%x", sha256.Sum256([]byte(common+"\x00"+boundary+"\x00"+strings.Join(args, "\x00"))))
		if out, ok := cache[key]; ok {
			return []byte(out), nil
		}
		out, err := git(ctx, dir, args...)
		// A later fetch can make an ignored remote tip available. The object
		// directory metadata invalidates negative answers after such a fetch.
		if err == nil {
			cache[key] = string(out)
		}
		return out, err
	}
}

func publicationRepository(dir string) (common, boundary string, ok bool) {
	raw, err := os.ReadFile(filepath.Join(dir, ".git"))
	if err != nil {
		return "", "", false
	}
	gd, ok := strings.CutPrefix(strings.TrimSpace(string(raw)), "gitdir: ")
	if !ok {
		return "", "", false
	}
	if !filepath.IsAbs(gd) {
		gd = filepath.Join(dir, gd)
	}
	raw, err = os.ReadFile(filepath.Join(gd, "commondir"))
	if err != nil {
		return "", "", false
	}
	common = strings.TrimSpace(string(raw))
	if !filepath.IsAbs(common) {
		common = filepath.Join(gd, common)
	}
	common = filepath.Clean(common)
	// Replacement objects and grafts make commit identity mutable.
	for _, name := range []string{"refs/replace", "info/grafts", "config.worktree"} {
		if _, err := os.Stat(filepath.Join(common, name)); !os.IsNotExist(err) {
			return "", "", false
		}
	}
	if _, err := os.Stat(filepath.Join(gd, "config.worktree")); !os.IsNotExist(err) {
		return "", "", false
	}
	packed, err := os.ReadFile(filepath.Join(common, "packed-refs"))
	if err != nil && !os.IsNotExist(err) || strings.Contains(string(packed), " refs/replace/") {
		return "", "", false
	}
	raw, err = os.ReadFile(filepath.Join(common, "shallow"))
	if err != nil && !os.IsNotExist(err) {
		return "", "", false
	}
	return common, string(raw), true
}

// Fetch writes loose objects into fanout directories or packs into pack/.
// Read their metadata once per repository and scan, without spawning git.
// Alternates and externally selected object stores bypass this cache.
func publicationObjects(common string) (string, error) {
	for _, name := range []string{"GIT_OBJECT_DIRECTORY", "GIT_ALTERNATE_OBJECT_DIRECTORIES", "GIT_SHALLOW_FILE", "GIT_REPLACE_REF_BASE"} {
		if os.Getenv(name) != "" {
			return "", fmt.Errorf("external Git object configuration")
		}
	}
	root := filepath.Join(common, "objects")
	if _, err := os.Stat(filepath.Join(root, "info", "alternates")); !os.IsNotExist(err) {
		return "", fmt.Errorf("alternate object store")
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return "", err
	}
	var stamp strings.Builder
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return "", err
		}
		fmt.Fprintf(&stamp, "\x00%s:%d", entry.Name(), info.ModTime().UnixNano())
	}
	return stamp.String(), nil
}
