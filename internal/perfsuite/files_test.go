//go:build perf && linux

package perfsuite

import (
	"cmp"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// fileReads attributes a running process's reads to the files it reads: it
// samples /proc/<pid>/fd and fdinfo every 10ms and counts, for each file
// opened read-only, how far its offset moved while it was open. A file
// opened, read and closed between two samples is missed, and a positional
// read (pread, mmap) moves no offset, so the totals are a lower bound; a
// whole-log read of MiBs takes long enough to be seen. The report names
// the files behind a read budget.
type fileReads struct {
	pid   int
	strip string
	mu    sync.Mutex
	open  map[string]*openFile // by fd number
	total map[string]int64     // by normalized path
	done  chan struct{}
	ended chan struct{}
}

type openFile struct {
	path     string
	min, max int64
}

func watchFileReads(pid int, strip string) *fileReads {
	r := &fileReads{pid: pid, strip: strip, open: map[string]*openFile{}, total: map[string]int64{}, done: make(chan struct{}), ended: make(chan struct{})}
	go func() {
		defer close(r.ended)
		tick := time.NewTicker(10 * time.Millisecond)
		defer tick.Stop()
		for {
			r.sample()
			select {
			case <-r.done:
				return
			case <-tick.C:
			}
		}
	}()
	return r
}

func (r *fileReads) sample() {
	dir := "/proc/" + strconv.Itoa(r.pid)
	entries, err := os.ReadDir(dir + "/fd")
	if err != nil {
		return
	}
	seen := map[string]bool{}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, e := range entries {
		fd := e.Name()
		target, err := os.Readlink(dir + "/fd/" + fd)
		if err != nil || !strings.HasPrefix(target, "/") {
			continue // sockets, pipes, anonymous inodes
		}
		pos, flags, ok := fdInfo(dir + "/fdinfo/" + fd)
		if !ok || flags&0o3 != 0 {
			continue // written, not read
		}
		seen[fd] = true
		path := r.normalize(target)
		o := r.open[fd]
		if o != nil && (o.path != path || pos < o.max) {
			// Closed and the number reused, or seeked back: what moved is read.
			r.total[o.path] += o.max - o.min
			o = nil
		}
		if o == nil {
			r.open[fd] = &openFile{path: path, min: pos, max: pos}
			continue
		}
		o.max = max(o.max, pos)
	}
	for fd, o := range r.open {
		if !seen[fd] {
			r.total[o.path] += o.max - o.min
			delete(r.open, fd)
		}
	}
}

func fdInfo(path string) (pos, flags int64, ok bool) {
	raw, err := os.ReadFile(path) //nolint:gosec // /proc fdinfo of the process under measurement
	if err != nil {
		return 0, 0, false
	}
	var havePos, haveFlags bool
	for line := range strings.SplitSeq(string(raw), "\n") {
		k, v, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		v = strings.TrimSpace(v)
		switch k {
		case "pos":
			pos, err = strconv.ParseInt(v, 10, 64)
			havePos = err == nil
		case "flags":
			flags, err = strconv.ParseInt(v, 8, 64)
			haveFlags = err == nil
		}
	}
	return pos, flags, havePos && haveFlags
}

// digits folds the timestamps and serials in checkpoint anchors, temporary
// files and event segments, so one file kind is one row.
var digits = regexp.MustCompile(`[0-9]+`)

func (r *fileReads) normalize(target string) string {
	target = strings.TrimSuffix(target, " (deleted)")
	if r.strip != "" {
		target = strings.ReplaceAll(target, r.strip, "<fixture>")
	}
	return digits.ReplaceAllString(target, "N")
}

// snapshot is what was read so far, per file, the open files included.
func (r *fileReads) snapshot() map[string]int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string]int64, len(r.total))
	for p, n := range r.total {
		out[p] = n
	}
	for _, o := range r.open {
		out[o.path] += o.max - o.min
	}
	return out
}

func (r *fileReads) stop() {
	select {
	case <-r.done:
	default:
		close(r.done)
	}
	<-r.ended
}

// readsBetween formats the files read between two snapshots, per unit
// (a pass, a second), largest first: the files behind a read budget.
func readsBetween(before, after map[string]int64, per float64, unit string) string {
	type file struct {
		path string
		mib  float64
	}
	var files []file
	for p, n := range after {
		if d := float64(n-before[p]) / mib / per; d >= 0.01 {
			files = append(files, file{p, d})
		}
	}
	if len(files) == 0 {
		return "no file read seen"
	}
	slices.SortFunc(files, func(a, b file) int { return cmp.Compare(b.mib, a.mib) })
	parts := make([]string, 0, 8)
	for i, f := range files {
		if i == 8 {
			parts = append(parts, fmt.Sprintf("%d more", len(files)-i))
			break
		}
		parts = append(parts, fmt.Sprintf("%s %.2f MiB", f.path, f.mib))
	}
	return "files read per " + unit + ": " + strings.Join(parts, ", ")
}
