package github

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/textproto"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

const repositoryCacheBytes = 8 << 20
const repositoryCacheEntries = 1024

type repositoryRead struct {
	body    []byte
	etag    string
	err     error
	expires time.Time
	used    time.Time
	busy    chan struct{}
}

// Cache only the two fixed read shapes used by rawFile and defaultBranch.
// Keep the cache with the runner, not with a function pointer (closures can
// share a code address but use different credentials or test responses).
type repositoryCache struct {
	mu      sync.Mutex
	entries map[string]*repositoryRead
	bytes   int
	run     GH
	now     func() time.Time
}

func newRepositoryCache(run GH, now func() time.Time) *repositoryCache {
	return &repositoryCache{entries: map[string]*repositoryRead{}, run: run, now: now}
}

func repositoryReadKind(args []string) (endpoint string, head bool) {
	if len(args) == 4 && args[0] == ghAPI {
		if args[2] == ghFlagJQ && args[3] == ".sha" && strings.HasPrefix(args[1], "repos/") && strings.HasSuffix(args[1], "/commits/HEAD") {
			return args[1], true
		}
		if args[1] == "-H" && args[2] == "Accept: application/vnd.github.raw" && strings.HasPrefix(args[3], "repos/") && strings.Contains(args[3], "/contents/") {
			return args[3], false
		}
	}
	return "", false
}

func (c *repositoryCache) call(ctx context.Context, args ...string) ([]byte, error) {
	endpoint, head := repositoryReadKind(args)
	if endpoint == "" {
		return c.run(ctx, args...)
	}
	// Scope by credential routing metadata, never by a credential value.
	key := strings.Join([]string{os.Getenv("GH_HOST"), os.Getenv("GH_CONFIG_DIR"), os.Getenv("GH_REPO"), os.Getenv("PATH"), Repo(ctx), endpoint}, "\x00")
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		c.mu.Lock()
		e := c.entries[key]
		if e == nil {
			c.trim()
			if len(c.entries) >= repositoryCacheEntries {
				c.mu.Unlock()
				return nil, fmt.Errorf("repository read cache busy")
			}
			e = &repositoryRead{}
			c.entries[key] = e
		}
		if e.busy != nil {
			ready := e.busy
			c.mu.Unlock()
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-ready:
				continue
			}
		}
		e.used = c.now()
		if e.used.Before(e.expires) {
			body, err := bytes.Clone(e.body), e.err
			c.mu.Unlock()
			return body, err
		}
		e.busy = make(chan struct{})
		old := *e
		c.mu.Unlock()

		body, etag, err := c.fetch(ctx, args, head, old)
		ttl := time.Minute
		if head {
			// This is a commit, not the branch's name. Refresh hourly so new
			// declarations remain discoverable.
			ttl = time.Hour
		}
		if err != nil {
			ttl = 0 // Never serve a stale body after an auth or network error.
			if notFound(err) {
				ttl = 5 * time.Minute
			}
		}
		c.mu.Lock()
		c.bytes -= len(e.body)
		e.body, e.etag, e.err = bytes.Clone(body), etag, err
		e.expires = c.now().Add(ttl)
		c.bytes += len(e.body)
		close(e.busy)
		e.busy = nil
		c.trim()
		c.mu.Unlock()
		return body, err
	}
}

// Discard the oldest idle entries, bounding both bodies and key metadata.
func (c *repositoryCache) trim() {
	for c.bytes > repositoryCacheBytes || len(c.entries) >= repositoryCacheEntries {
		oldest := ""
		for key, e := range c.entries {
			if e.busy == nil && (oldest == "" || e.used.Before(c.entries[oldest].used)) {
				oldest = key
			}
		}
		if oldest == "" {
			return
		}
		c.bytes -= len(c.entries[oldest].body)
		delete(c.entries, oldest)
	}
}

func (c *repositoryCache) fetch(ctx context.Context, args []string, head bool, old repositoryRead) ([]byte, string, error) {
	if head {
		out, err := c.run(ctx, args...)
		if err == nil && strings.TrimSpace(string(out)) == "" {
			err = fmt.Errorf("default branch read returned no commit")
		}
		return out, "", err
	}
	request := append(append([]string(nil), args...), "--include")
	if old.etag != "" && old.err == nil {
		request = append(request, "-H", "If-None-Match: "+old.etag)
	}
	out, runErr := c.run(ctx, request...)
	// gh --include prints an HTTP status line, headers, then the raw body.
	r := bufio.NewReader(bytes.NewReader(out))
	statusLine, err := r.ReadString('\n')
	fields := strings.Fields(statusLine)
	if err == nil && (len(fields) < 2 || !strings.HasPrefix(fields[0], "HTTP/")) {
		err = fmt.Errorf("missing HTTP status")
	}
	if err != nil {
		if runErr != nil {
			return nil, "", runErr
		}
		return nil, "", fmt.Errorf("repository file response headers: %w", err)
	}
	status, err := strconv.Atoi(fields[1])
	if err != nil {
		return nil, "", fmt.Errorf("repository file response status: %w", err)
	}
	headers, err := textproto.NewReader(r).ReadMIMEHeader()
	if err != nil {
		return nil, "", fmt.Errorf("repository file response headers: %w", err)
	}
	if status == http.StatusNotModified && old.etag != "" && old.err == nil {
		return bytes.Clone(old.body), old.etag, nil
	}
	if runErr != nil {
		return nil, "", runErr
	}
	if status != http.StatusOK {
		return nil, "", fmt.Errorf("repository file read: HTTP %d", status)
	}
	// gh already decoded transfer encoding; its printed headers can still
	// name chunking or compression. Read the remaining output verbatim.
	body, err := io.ReadAll(r)
	return body, headers.Get("ETag"), err
}
