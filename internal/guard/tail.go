package guard

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

// A failed capped run keeps the tail of its output — the `--- FAIL` lines a
// truncated screen lost — so the failing test can be named without rerunning
// the suite (one full `make test` rerun to read a failure name cost 13
// minutes). The tail of run <name> is <stateDir>/<TailsDirName>/<name>.log,
// masked like every output that leaves the run, and `beekeeper run tail`
// shows it.

// TailsDirName is the directory under the state directory that holds one
// tail per failed run.
const TailsDirName = "run-tails"

// MaxTailBytes bounds the kept tail whatever the configuration asks for:
// the tail names the failing test, it is not a log file.
const MaxTailBytes = 1 << 20

// tailsKeep is how many tails the directory holds: the keep-on-write trim
// removes the oldest beyond it, so the directory stays bounded without a
// prune of its own.
const tailsKeep = 100

// tailHeader starts a kept tail and names how the run ended and what it
// ran; `beekeeper run tail` reads it back for its listing and its exit
// line.
const tailHeader = "# beekeeper run tail: exit %d at %s: %s\n"

// Tail is the last maxBytes of one run's output, kept in a fixed ring: the
// capture costs no allocation past its own size and no unbounded growth —
// a 13-minute `make test` writes its megabytes through it while the memory
// stays the cap. stdout and stderr of a run share one Tail: go test writes
// the FAIL lines to stdout and the build errors to stderr, and the order
// between the two stays the order they happened.
type Tail struct {
	max  int
	buf  []byte
	pos  int
	full bool
	mu   sync.Mutex
}

// NewTail is a Tail of the last maxBytes bytes, clamped to MaxTailBytes;
// nil for maxBytes < 1, which keeps nothing.
func NewTail(maxBytes int) *Tail {
	if maxBytes < 1 {
		return nil
	}
	if maxBytes > MaxTailBytes {
		maxBytes = MaxTailBytes
	}
	return &Tail{max: maxBytes, buf: make([]byte, maxBytes)}
}

// write records p, dropping from the front what no longer fits the ring.
func (t *Tail) write(p []byte) {
	if len(p) == 0 {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(p) >= t.max {
		copy(t.buf, p[len(p)-t.max:])
		t.pos, t.full = 0, true
		return
	}
	if t.pos+len(p) > t.max {
		split := t.max - t.pos
		copy(t.buf[t.pos:], p[:split])
		copy(t.buf, p[split:])
		t.pos = len(p) - split
		t.full = true
		return
	}
	copy(t.buf[t.pos:], p)
	t.pos += len(p)
	if t.pos == t.max {
		t.pos, t.full = 0, true
	}
}

// Bytes is the kept tail, oldest first, and whether the ring cut it: a cut
// tail's first line is incomplete and names nothing.
func (t *Tail) Bytes() ([]byte, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.full {
		return append([]byte(nil), t.buf[:t.pos]...), false
	}
	out := make([]byte, 0, t.max)
	out = append(out, t.buf[t.pos:]...)
	return append(out, t.buf[:t.pos]...), true
}

// teeWriter is a stream's writer: every byte goes to the terminal unchanged
// and into the run's Tail.
type teeWriter struct {
	dst  io.Writer
	tail *Tail
}

func (w teeWriter) Write(p []byte) (int, error) {
	n, err := w.dst.Write(p)
	if w.tail != nil {
		w.tail.write(p[:n])
	}
	return n, err
}

// Write lets the remote builder's stream tee into the tail as the local
// scope's does (io.Writer); a nil tail swallows what it is given.
func (t *Tail) Write(p []byte) (int, error) {
	if t == nil {
		return len(p), nil
	}
	t.write(p)
	return len(p), nil
}

// KeepTail masks the tail and writes it under dir as the run name's tail,
// replacing an older one of the same name, and trims the directory back to
// its bound. A cut tail loses its first partial line: half a line names
// nothing. redact masks the text; nil falls back to the token patterns
// alone, so a caller that has no index still never stores a token. It
// answers the path it wrote and the masked body, which is what a failure
// prints: no caller writes or shows an unmasked tail.
func KeepTail(dir, name, head string, exit int, t *Tail, redact func(string) string) (path, masked string, err error) {
	raw, cut := t.Bytes()
	s := strings.TrimRight(string(raw), "\n")
	if i := strings.IndexByte(s, '\n'); cut && i >= 0 {
		s = s[i+1:]
	}
	if redact == nil {
		redact = func(s string) string { out, _ := (&Index{}).Redact(s); return out }
	}
	masked = redact(s)
	if err = os.MkdirAll(dir, 0o700); err != nil {
		return "", "", err
	}
	path = filepath.Join(dir, name+".log") //nolint:gosec // name is the run's own scope unit or run id, both plain characters
	body := fmt.Sprintf(tailHeader, exit, time.Now().UTC().Format(time.RFC3339), redact(head)) + masked + "\n"
	if err = os.WriteFile(path, []byte(body), 0o600); err != nil {
		return "", "", err
	}
	trimTails(dir)
	return path, masked, nil
}

// trimTails keeps the tailsKeep newest tails of dir and removes the rest:
// the keep-on-write trim bounds the directory without a prune of its own.
func trimTails(dir string) {
	es, err := os.ReadDir(dir)
	if err != nil || len(es) <= tailsKeep {
		return
	}
	type tail struct {
		name string
		mod  time.Time
	}
	var ts []tail
	for _, e := range es {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".log") {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		ts = append(ts, tail{e.Name(), fi.ModTime()})
	}
	if len(ts) <= tailsKeep {
		return
	}
	slices.SortFunc(ts, func(a, b tail) int { return b.mod.Compare(a.mod) })
	for _, t := range ts[tailsKeep:] {
		_ = os.Remove(filepath.Join(dir, t.name))
	}
}
