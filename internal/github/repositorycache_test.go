package github

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const cacheTestRepo = "example/cache"

func fileResponse(status int, etag, body string) []byte {
	return []byte(fmt.Sprintf("HTTP/2.0 %d Status\r\nETag: %s\r\n\r\n%s", status, etag, body))
}

func TestRepositoryReadsWorkingHour(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	now := start
	heads, charged, conditional := 0, 0, 0
	cache := newRepositoryCache(func(_ context.Context, args ...string) ([]byte, error) {
		if slices.Contains(args, ghFlagJQ) {
			heads++
			charged++
			return []byte("commit\n"), nil
		}
		if !slices.Contains(args, "--include") {
			t.Fatal("file read omitted response headers")
		}
		if slices.Contains(args, `If-None-Match: "v1"`) {
			conditional++
			return fileResponse(304, `"v1"`, ""), nil
		}
		charged++
		return fileResponse(200, `"v1"`, "manifest"), nil
	}, func() time.Time { return now })
	// Ten repositories, two files per repository, one loop pass per ten
	// seconds. Only the first file reads spend REST points; all refreshes
	// carry their validator. Mutating the returned bytes cannot poison it.
	for tick := range 360 {
		now = start.Add(time.Duration(tick) * 10 * time.Second)
		for repo := range 10 {
			name := fmt.Sprintf("example/repo%d", repo)
			sha, err := defaultBranch(t.Context(), cache.call, name)
			if err != nil || sha != "commit" {
				t.Fatalf("head: %q, %v", sha, err)
			}
			for _, path := range []string{"manifest.json", "installation.json"} {
				body, err := rawFile(t.Context(), cache.call, name, path, sha)
				if err != nil || string(body) != "manifest" {
					t.Fatalf("file: %q, %v", body, err)
				}
				body[0] = 'x'
			}
		}
	}
	if heads != 10 || charged != 30 || conditional != 1180 || charged+conditional >= 2500 {
		t.Fatalf("hour: heads=%d charged=%d conditional=%d", heads, charged, conditional)
	}
	now = start.Add(time.Hour)
	if _, err := defaultBranch(t.Context(), cache.call, "example/repo0"); err != nil || heads != 11 {
		t.Fatalf("hourly head refresh: heads=%d err=%v", heads, err)
	}
}

func TestRepositoryFileChangesErrorsAndAbsence(t *testing.T) {
	now := time.Now()
	calls := 0
	cache := newRepositoryCache(func(_ context.Context, args ...string) ([]byte, error) {
		calls++
		switch calls {
		case 1:
			return fileResponse(200, `"first"`, "old"), nil
		case 2:
			if !slices.Contains(args, `If-None-Match: "first"`) {
				t.Fatal("changed read omitted old ETag")
			}
			return fileResponse(200, `"second"`, "new"), nil
		case 3:
			if !slices.Contains(args, `If-None-Match: "second"`) {
				t.Fatal("validator was not replaced")
			}
			return nil, errors.New("HTTP 403 forbidden")
		case 4:
			return fileResponse(404, "", "missing"), errors.New("HTTP 404 not found")
		default:
			return fileResponse(200, `"created"`, ""), nil
		}
	}, func() time.Time { return now })
	read := func() ([]byte, error) { return rawFile(t.Context(), cache.call, cacheTestRepo, "file", "main") }
	for _, want := range []string{"old", "new"} {
		body, err := read()
		if err != nil || string(body) != want {
			t.Fatalf("read = %q, %v; want %q", body, err, want)
		}
		now = now.Add(time.Minute)
	}
	if body, err := read(); err == nil || len(body) != 0 {
		t.Fatalf("auth failure served stale body: %q, %v", body, err)
	}
	for range 3 {
		if _, err := read(); !notFound(err) {
			t.Fatalf("missing file: %v", err)
		}
	}
	if calls != 4 {
		t.Fatalf("missing file was not cached: %d calls", calls)
	}
	now = now.Add(5 * time.Minute)
	if body, err := read(); err != nil || len(body) != 0 || calls != 5 {
		t.Fatalf("new empty file: %q, %v (%d calls)", body, err, calls)
	}
}

func TestRepositoryCacheCoalescesAndCancels(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	cache := newRepositoryCache(func(_ context.Context, _ ...string) ([]byte, error) {
		if calls.Add(1) == 1 {
			close(entered)
		}
		<-release
		return fileResponse(200, `"v1"`, "body"), nil
	}, time.Now)
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			body, err := rawFile(t.Context(), cache.call, cacheTestRepo, "file", "main")
			if err != nil || string(body) != "body" {
				t.Errorf("coalesced read: %q, %v", body, err)
			}
		})
	}
	<-entered
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := rawFile(ctx, cache.call, cacheTestRepo, "file", "main"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled waiter: %v", err)
	}
	close(release)
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("concurrent reads issued %d calls", calls.Load())
	}
}

func TestRepositoryCacheKeysAndBounds(t *testing.T) {
	calls := 0
	cache := newRepositoryCache(func(_ context.Context, _ ...string) ([]byte, error) {
		calls++
		return fileResponse(200, `"v1"`, "body"), nil
	}, time.Now)
	for _, tc := range []struct{ repo, path, ref, acting string }{
		{cacheTestRepo, "file", "main", cacheTestRepo},
		{"example/other", "file", "main", cacheTestRepo},
		{cacheTestRepo, "other", "main", cacheTestRepo},
		{cacheTestRepo, "file", "other", cacheTestRepo},
		{cacheTestRepo, "file", "main", "example/other"},
	} {
		for range 2 {
			if _, err := rawFile(ActingOn(t.Context(), tc.acting), cache.call, tc.repo, tc.path, tc.ref); err != nil {
				t.Fatal(err)
			}
		}
	}
	if calls != 5 {
		t.Fatalf("repo/path/ref/routing cache keys: %d calls", calls)
	}
	for range 2 {
		if _, err := cache.call(t.Context(), ghAPI, "repos/example/cache", "-X", "PATCH"); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 7 {
		t.Fatal("unrelated requests were cached")
	}
	for i := range repositoryCacheEntries + 1 {
		if _, err := rawFile(t.Context(), cache.call, cacheTestRepo, fmt.Sprintf("file%d", i), "main"); err != nil {
			t.Fatal(err)
		}
	}
	if len(cache.entries) > repositoryCacheEntries {
		t.Fatal("cache exceeded entry bound")
	}
	cache.run = func(_ context.Context, _ ...string) ([]byte, error) {
		return fileResponse(200, `"large"`, strings.Repeat("x", repositoryCacheBytes+1)), nil
	}
	if _, err := rawFile(t.Context(), cache.call, cacheTestRepo, "large", "main"); err != nil || cache.bytes > repositoryCacheBytes {
		t.Fatalf("cache exceeded byte bound: %d, %v", cache.bytes, err)
	}
}
