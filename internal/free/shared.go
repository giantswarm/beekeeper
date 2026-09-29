package free

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gofrs/flock"
)

// Shared runs the --summary scan at most once at a time and shares its rows.
// A front end starts free --summary on every click, and on a starved machine
// each scan takes minutes: without sharing, every impatient click adds a full
// scan to the load that made the first one slow. A caller that finds a scan
// running waits for it and prints its rows; a caller within MaxAge of a
// finished scan prints the cached rows under a header row with their age.
type Shared struct {
	// Dir holds the lock and the cached rows.
	Dir string
	// Key tells scans with other options apart: a cached result made with
	// another key is not used.
	Key string
	// MaxAge is how long a finished scan's rows are reused.
	MaxAge time.Duration
	// Timeout bounds the call, the wait for a running scan included. On
	// timeout the rows collected so far are printed with a partial row.
	Timeout time.Duration
	// Now is the clock, replaced in tests.
	Now func() time.Time
}

// Row kinds Shared adds to the summary, after the scan's own.
const (
	// CachedRow heads rows reused from an earlier scan: cached <age s>.
	CachedRow = "cached"
	// PartialRow ends rows cut short by the timeout: partial <timeout s>.
	PartialRow = "partial"
)

// Print writes the summary rows to out: cached ones when a fresh result
// exists, otherwise those of one scan, run here or by the caller holding the
// lock.
func (s Shared) Print(out io.Writer, scan func(io.Writer) error) error {
	if s.Now == nil {
		s.Now = time.Now
	}
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return err
	}
	if s.printCached(out) {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), s.Timeout)
	defer cancel()
	lock := flock.New(filepath.Join(s.Dir, "summary.lock"))
	ok, err := lock.TryLockContext(ctx, 200*time.Millisecond)
	if !ok {
		if ctx.Err() == nil {
			return err
		}
		// Another caller's scan outlived our timeout: the last finished
		// scan's rows, however old, are better than none.
		if rows, at, ok := s.read(); ok {
			s.write(out, rows, at)
		}
		s.partial(out)
		return nil
	}
	defer func() { _ = lock.Unlock() }()
	// The scan we waited for has just written its rows.
	if s.printCached(out) {
		return nil
	}
	buf := &lineBuffer{}
	done := make(chan error, 1)
	go func() { done <- scan(buf) }()
	select {
	case err := <-done:
		if err != nil {
			return err
		}
		rows := buf.lines()
		s.store(rows)
		_, err = out.Write(rows)
		return err
	case <-ctx.Done():
		// A partial result is not cached: the next caller scans again, one
		// at a time.
		_, err := out.Write(append(buf.lines(), s.partialRow()...))
		return err
	}
}

func (s Shared) path() string { return filepath.Join(s.Dir, "summary.tsv") }

// printCached prints a fresh cached result and says whether there was one.
func (s Shared) printCached(out io.Writer) bool {
	rows, at, ok := s.read()
	if !ok || s.Now().Sub(at) > s.MaxAge {
		return false
	}
	s.write(out, rows, at)
	return true
}

func (s Shared) write(out io.Writer, rows []byte, at time.Time) {
	age := max(s.Now().Sub(at), 0)
	_, _ = fmt.Fprintf(out, "%s\t%d\n", CachedRow, int(age.Seconds()))
	_, _ = out.Write(rows)
}

func (s Shared) partial(out io.Writer) { _, _ = out.Write(s.partialRow()) }

func (s Shared) partialRow() []byte {
	return fmt.Appendf(nil, "%s\t%d\n", PartialRow, int(s.Timeout.Seconds()))
}

// read returns the cached rows made with this key and when they were made.
func (s Shared) read() ([]byte, time.Time, bool) {
	b, err := os.ReadFile(s.path())
	if err != nil {
		return nil, time.Time{}, false
	}
	head, rows, _ := bytes.Cut(b, []byte("\n"))
	stamp, key, _ := strings.Cut(string(head), "\t")
	at, err := time.Parse(time.RFC3339Nano, stamp)
	if err != nil || key != s.Key {
		return nil, time.Time{}, false
	}
	return rows, at, true
}

// store replaces the cached rows; a failure only costs the next caller a
// scan.
func (s Shared) store(rows []byte) {
	tmp, err := os.CreateTemp(s.Dir, "summary-*.tmp")
	if err != nil {
		return
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	_, err = fmt.Fprintf(tmp, "%s\t%s\n%s", s.Now().UTC().Format(time.RFC3339Nano), s.Key, rows)
	if cerr := tmp.Close(); err != nil || cerr != nil {
		return
	}
	_ = os.Rename(tmp.Name(), s.path())
}

// lineBuffer collects a scan's rows while the caller may read them.
type lineBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lineBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

// lines returns a copy of the complete lines written so far.
func (b *lineBuffer) lines() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := b.buf.Bytes()
	if i := bytes.LastIndexByte(out, '\n'); i >= 0 {
		return bytes.Clone(out[:i+1])
	}
	return nil
}
