package state

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"reflect"
	"slices"
)

type cachedEventFile struct {
	info   os.FileInfo
	offset int64
	events []Event
}

// cachedEvents decodes each appended line once per process, even for callers
// with different predicates. Rotation preserves the inode and its cursor;
// replacement, truncation and same-size rewrites invalidate the old projection.
// An unterminated last line is visible just as in Events, but never advances
// the cursor until completed. Filters and callers receive independent notes.
func (s *FileStore) cachedEvents(n int, keep func(Event) bool) ([]Event, error) {
	s.eventsMu.Lock()
	defer s.eventsMu.Unlock()
	files, err := s.eventFiles()
	if err != nil {
		return nil, err
	}
	defer closeEventFiles(files)
	infos := make([]os.FileInfo, len(files))
	for i, f := range files {
		info, err := f.Stat()
		if err != nil {
			return nil, err
		}
		infos[i] = info
	}
	// Bounded queries start at the newest segment and stop as soon as
	// satisfied. Cache at most 32 MiB of the newest segments, never a whole
	// large archive, and keep already-cached older segments across bounded
	// queries that stop at a newer segment.
	const cacheBytes = 32 << 20
	var retained int64
	cacheable := make([]bool, len(files))
	next := make([]*cachedEventFile, len(files))
	for i := len(files) - 1; i >= 0; i-- {
		info := infos[i]
		if retained+info.Size() > cacheBytes {
			continue
		}
		retained += info.Size()
		cacheable[i] = true
		for _, old := range s.events {
			if os.SameFile(old.info, info) && info.Size() >= old.info.Size() && info.Size() >= old.offset &&
				(info.Size() != old.info.Size() || info.ModTime().Equal(old.info.ModTime())) {
				next[i] = old
				break
			}
		}
	}
	order := make([]int, len(files))
	for i := range order {
		if n > 0 {
			order[i] = len(files) - 1 - i
		} else {
			order[i] = i
		}
	}
	var out []Event
	for _, i := range order {
		f := files[i]
		info := infos[i]
		cache := cacheable[i]
		at := next[i]
		if at == nil {
			at = &cachedEventFile{info: info}
		}
		var hits []Event
		add := func(e Event) {
			if e.NoteSnapshot != nil {
				e.NoteSnapshot = cloneSnapshot(reflect.ValueOf(e.NoteSnapshot)).Interface().(*Note)
			}
			if keep == nil || keep(e) {
				hits = append(hits, e)
				if n > 0 && len(hits) > n {
					hits = hits[len(hits)-n:]
				}
			}
		}
		for _, e := range at.events {
			add(e)
		}
		if _, err := f.Seek(at.offset, io.SeekStart); err != nil {
			return nil, err
		}
		reader := bufio.NewReader(f)
		for {
			line, readErr := reader.ReadBytes('\n')
			if readErr != nil && !errors.Is(readErr, io.EOF) {
				return nil, readErr
			}
			var e Event
			valid := json.Unmarshal(bytes.TrimLeft(line, "\x00"), &e) == nil
			if valid {
				add(e)
			}
			if errors.Is(readErr, io.EOF) {
				break
			}
			at.offset += int64(len(line))
			if valid && cache {
				at.events = append(at.events, e)
			}
		}
		at.info = info
		if cache {
			next[i] = at
		}
		if n > 0 {
			out = append(hits, out...)
			if len(out) >= n {
				out = out[len(out)-n:]
				break
			}
		} else {
			out = append(out, hits...)
		}
	}
	s.events = slices.DeleteFunc(next, func(c *cachedEventFile) bool { return c == nil })
	return out, nil
}
