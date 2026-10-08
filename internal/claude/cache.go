package claude

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/giantswarm/beekeeper/internal/config"
)

// A watch reads the same files every tick: the desktop's session records
// (thousands, hundreds of MiB) and the transcripts of the running sessions,
// among thousands of project folders. The caches below keep what a read
// found for the life of the process, so a tick pays a stat per file instead
// of a parse per record and a folder scan per transcript.

// records are the parsed desktop records by path, each with the size and
// modification time it was parsed at: a record whose file did not change is
// not parsed again.
var records = struct {
	sync.Mutex
	byPath map[string]cachedRecord
}{byPath: map[string]cachedRecord{}}

type cachedRecord struct {
	size int64
	// mod is the file's modification time, read when it was read.
	mod, read time.Time
	rec       *Record
}

// racy is how recent a modification time is too recent to vouch that a file
// or folder did not change since: a change within the clock's granularity
// keeps the time it had. Such a file is read again, such a folder listed
// again.
const racy = 2 * time.Second

// settled reports whether a file or folder modified at mod, unchanged since
// a read at readAt, is known to hold what that read found.
func settled(mod, readAt time.Time) bool { return readAt.Sub(mod) > racy }

func readRecord(path string) (*Record, bool) {
	path = filepath.Clean(path)
	fi, err := os.Stat(path)
	if err != nil {
		forgetRecord(path)
		return nil, false
	}
	records.Lock()
	c, ok := records.byPath[path]
	records.Unlock()
	if ok && c.size == fi.Size() && c.mod.Equal(fi.ModTime()) && settled(c.mod, c.read) {
		return copyRecord(c.rec)
	}
	read := time.Now()
	raw, err := os.ReadFile(path)
	if err != nil {
		forgetRecord(path)
		return nil, false
	}
	r, ok := parseRecord(raw)
	if !ok {
		r = nil // a record caught mid-write: parsed again once it changes
	}
	records.Lock()
	records.byPath[path] = cachedRecord{size: fi.Size(), mod: fi.ModTime(), read: read, rec: r}
	records.Unlock()
	return copyRecord(r)
}

func forgetRecord(path string) {
	records.Lock()
	delete(records.byPath, path)
	records.Unlock()
}

// copyRecord hands each caller a record of its own: callers may change it.
func copyRecord(r *Record) (*Record, bool) {
	if r == nil {
		return nil, false
	}
	c := *r
	return &c, true
}

// transcriptMiss is how long a session id whose transcript no project folder
// holds is not looked up across all of them again while no project folder
// was added; its own folder is still looked at every time.
const transcriptMiss = time.Minute

// transcripts are the transcript paths found, by projects folder and
// session id, and when a look across every project folder last found none.
var transcripts = struct {
	sync.Mutex
	byID   map[string]string
	missed map[string]missed
}{byID: map[string]string{}, missed: map[string]missed{}}

// missed is when a look across the project folders found no transcript, and
// the modification time the projects folder had then.
type missed struct{ at, root time.Time }

// Transcript is the path of session id's transcript (<projects>/<folder>/<id>.jsonl),
// "" while it has none. cwd, when known, is the folder the session runs in:
// Claude Code names a project folder after it, so its transcript is found
// with one stat. The path found is kept; a session that moved folders or an
// unknown cwd costs one scan of the project folders, at most every
// transcriptMiss while none holds it and no project folder was added.
func Transcript(cfg *config.Config, id, cwd string) string {
	if id == "" || strings.ContainsAny(id, `/\*?[`) {
		return ""
	}
	key := filepath.Join(cfg.Claude.ProjectsDir, id)
	transcripts.Lock()
	known := transcripts.byID[key]
	miss, wasMissed := transcripts.missed[key]
	transcripts.Unlock()
	if known != "" && exists(known) {
		return known
	}
	if cwd != "" {
		if p := filepath.Join(cfg.Claude.ProjectsDir, ProjectFolder(cwd), id+".jsonl"); exists(p) {
			return keepTranscript(key, p)
		}
	}
	var root time.Time
	if fi, err := os.Stat(cfg.Claude.ProjectsDir); err == nil {
		root = fi.ModTime()
	}
	if known == "" && wasMissed && time.Since(miss.at) < transcriptMiss && root.Equal(miss.root) && settled(root, miss.at) {
		return ""
	}
	now := time.Now()
	if m, _ := filepath.Glob(filepath.Join(cfg.Claude.ProjectsDir, "*", id+".jsonl")); len(m) > 0 {
		return keepTranscript(key, m[0])
	}
	transcripts.Lock()
	delete(transcripts.byID, key)
	transcripts.missed[key] = missed{at: now, root: root}
	transcripts.Unlock()
	return ""
}

func keepTranscript(key, path string) string {
	transcripts.Lock()
	transcripts.byID[key] = path
	delete(transcripts.missed, key)
	transcripts.Unlock()
	return path
}

// recordDirs index the desktop's record folders (<desktop>/<account>/<org>/):
// the record files in each, listed again only once the folder changed.
var recordDirs = struct {
	sync.Mutex
	byDir map[string]listedDir
}{byDir: map[string]listedDir{}}

type listedDir struct {
	mod, read time.Time
	files     []string
}

// recordPaths are the paths of the desktop records, sorted by folder and
// name; a folder that did not change since its last listing is not listed
// again.
func recordPaths(cfg *config.Config) []string {
	dirs, _ := filepath.Glob(filepath.Join(cfg.Claude.DesktopDir, "*", "*"))
	var out []string
	for _, d := range dirs {
		fi, err := os.Stat(d)
		if err != nil || !fi.IsDir() {
			continue
		}
		recordDirs.Lock()
		l, ok := recordDirs.byDir[d]
		recordDirs.Unlock()
		if !ok || !l.mod.Equal(fi.ModTime()) || !settled(l.mod, l.read) {
			l = listedDir{mod: fi.ModTime(), read: time.Now()}
			names, err := readNames(d)
			if err != nil {
				continue
			}
			for _, n := range names {
				if strings.HasSuffix(n, ".json") {
					l.files = append(l.files, filepath.Join(d, n))
				}
			}
			recordDirs.Lock()
			recordDirs.byDir[d] = l
			recordDirs.Unlock()
		}
		out = append(out, l.files...)
	}
	return out
}

func readNames(dir string) ([]string, error) {
	f, err := os.Open(filepath.Clean(dir))
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	names, err := f.Readdirnames(-1)
	slices.Sort(names)
	return names, err
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// ProjectFolder is the name of the project folder Claude Code keeps the
// transcripts of the sessions run in dir under: every character that is no
// ASCII letter or digit becomes a dash.
func ProjectFolder(dir string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' {
			return r
		}
		return '-'
	}, dir)
}

// contexts are the context sizes read from the transcripts, by path, each
// with the size and modification time it was read at.
var contexts = struct {
	sync.Mutex
	byPath map[string]cachedContext
}{byPath: map[string]cachedContext{}}

type cachedContext struct {
	size      int64
	mod, read time.Time
	context   int64
}

// Context is the size of the last request in the transcript at path (Activity.Context),
// read again only once the transcript changed: an idle session's costs a stat.
func Context(path string) int64 {
	if path == "" {
		return 0
	}
	fi, err := os.Stat(path)
	if err != nil {
		return 0
	}
	contexts.Lock()
	c, ok := contexts.byPath[path]
	contexts.Unlock()
	if ok && c.size == fi.Size() && c.mod.Equal(fi.ModTime()) && settled(c.mod, c.read) {
		return c.context
	}
	read := time.Now()
	_, a := ReadTranscript(path, read)
	contexts.Lock()
	contexts.byPath[path] = cachedContext{size: fi.Size(), mod: fi.ModTime(), read: read, context: a.Context}
	contexts.Unlock()
	return a.Context
}

// TranscriptCache reads the transcripts as ReadTranscript does, for a
// process that reads them again and again (a watch): a transcript unchanged
// since its last read hands back that read while it is younger than fresh,
// so an idle session's costs a stat. Its last hour's figures are as old as
// that read.
type TranscriptCache struct {
	fresh  time.Duration
	mu     sync.Mutex
	byPath map[string]cachedRead
}

type cachedRead struct {
	size      int64
	mod, read time.Time
	work      Work
	activity  Activity
}

// NewTranscriptCache returns a cache whose reads stay fresh for fresh.
func NewTranscriptCache(fresh time.Duration) *TranscriptCache {
	return &TranscriptCache{fresh: fresh, byPath: map[string]cachedRead{}}
}

// Read is ReadTranscript(path, now), from the cache while the transcript is
// unchanged and the last read younger than the cache's fresh.
func (c *TranscriptCache) Read(path string, now time.Time) (Work, Activity) {
	fi, err := os.Stat(path)
	if path == "" || err != nil {
		return ReadTranscript(path, now)
	}
	c.mu.Lock()
	r, ok := c.byPath[path]
	c.mu.Unlock()
	if ok && r.size == fi.Size() && r.mod.Equal(fi.ModTime()) && settled(r.mod, r.read) &&
		!now.Before(r.read) && now.Sub(r.read) < c.fresh {
		return r.work, r.activity
	}
	w, a := ReadTranscript(path, now)
	c.mu.Lock()
	c.byPath[path] = cachedRead{size: fi.Size(), mod: fi.ModTime(), read: now, work: w, activity: a}
	c.mu.Unlock()
	return w, a
}
