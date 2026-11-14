package cmd

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// The coordinator keeps one bare object store per repository. Each transfer
// repository borrows its objects through alternates, and Git negotiates with
// the alternate's refs, so a fetch from GitHub moves only what the cache lacks
// instead of the repository's whole history: that full fetch spent the
// publication's deadline on a large repository. The cache is best effort; a
// transfer without it fetches as before.

// publicationCacheSourceRef names the cache ref an execution source is kept under.
const publicationCacheSourceRef = "refs/beekeeper/source"

// publicationCache returns the repository's object store, created on first
// use, or "" when it cannot be prepared.
func (a *app) publicationCache(ctx context.Context, repository string) string {
	owner, name, ok := strings.Cut(repository, "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") || !filepath.IsLocal(owner) || !filepath.IsLocal(name) {
		return ""
	}
	cache := filepath.Join(a.cfg.StateDir, executionPublications, "cache", owner, name+".git")
	if info, err := os.Stat(filepath.Join(cache, "objects", "info")); err == nil && info.IsDir() {
		return cache
	}
	if err := os.MkdirAll(filepath.Dir(cache), 0o700); err != nil {
		return ""
	}
	if _, err := publishGit(ctx, filepath.Dir(cache), "init", "--bare", "--quiet", cache); err != nil {
		return ""
	}
	return cache
}

// borrowPublicationCache lets the bare transfer repository repo read the
// cache's objects.
func borrowPublicationCache(repo, cache string) {
	if cache == "" {
		return
	}
	_ = os.WriteFile(filepath.Join(repo, "objects", "info", "alternates"), []byte(filepath.Join(cache, "objects")+"\n"), 0o600)
}

// keepInPublicationCache copies src's history from the transfer repository
// into the cache as ref, so the next transfer negotiates from it.
func keepInPublicationCache(ctx context.Context, cache, repo, src, ref string) {
	if cache == "" {
		return
	}
	_, _ = publishGit(ctx, cache, "fetch", "--no-tags", "--no-write-fetch-head", "--", repo, "+"+src+":"+ref)
}

// publicationObjectBytes sums the files of a repository's object store, -1
// when dir is not a repository whose object store it can find.
func publicationObjectBytes(dir string) int64 {
	objects := filepath.Join(dir, "objects")
	if info, err := os.Stat(objects); err != nil || !info.IsDir() {
		objects = filepath.Join(dir, ".git", "objects")
		if info, err := os.Stat(objects); err != nil || !info.IsDir() {
			return -1
		}
	}
	var total int64
	_ = filepath.WalkDir(objects, func(_ string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if info, infoErr := d.Info(); infoErr == nil && info.Mode().IsRegular() {
			total += info.Size()
		}
		return nil
	})
	return total
}
