package state

import (
	"bufio"
	"bytes"
	"context"
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

// cachedEventsContext decodes each appended line once per process, even for
// callers with different predicates. Rotation preserves the inode and its
// cursor; replacement, truncation and same-size rewrites invalidate the old
// projection. An unterminated last line is visible just as in Events, but never
// advances the cursor until completed. Filters and callers receive independent
// notes.
func (s *FileStore) cachedEventsContext(ctx context.Context, n int, keep func(Event) bool) ([]Event, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.eventsMu.Lock()
	defer s.eventsMu.Unlock()
	files, err := s.eventFilesContext(ctx)
	if err != nil {
		return nil, err
	}
	defer closeEventFiles(files)
	infos := make([]os.FileInfo, len(files))
	for i, f := range files {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
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
	type stagedCacheUpdate struct {
		at       *cachedEventFile
		info     os.FileInfo
		offset   int64
		appended []Event
	}
	var staged []stagedCacheUpdate
	var out []Event
	for _, i := range order {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		f := files[i]
		info := infos[i]
		cache := cacheable[i]
		at := next[i]
		if at == nil {
			at = &cachedEventFile{info: info}
		}
		baseOffset := at.offset
		baseEvents := at.events
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
		for _, e := range baseEvents {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			add(e)
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		if _, err := f.Seek(baseOffset, io.SeekStart); err != nil {
			return nil, err
		}
		offset := baseOffset
		var appended []Event
		reader := bufio.NewReader(f)
		for {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			line, readErr := reader.ReadBytes('\n')
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if readErr != nil && !errors.Is(readErr, io.EOF) {
				return nil, readErr
			}
			var e Event
			valid := json.Unmarshal(bytes.TrimLeft(line, "\x00"), &e) == nil
			if valid {
				add(e)
				if err := ctx.Err(); err != nil {
					return nil, err
				}
			}
			if errors.Is(readErr, io.EOF) {
				break
			}
			offset += int64(len(line))
			if valid && cache {
				appended = append(appended, e)
			}
		}
		if cache {
			next[i] = at
			staged = append(staged, stagedCacheUpdate{
				at:       at,
				info:     info,
				offset:   offset,
				appended: appended,
			})
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
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	for _, u := range staged {
		u.at.info = u.info
		u.at.offset = u.offset
		if len(u.appended) > 0 {
			u.at.events = append(u.at.events, u.appended...)
		}
	}
	s.events = slices.DeleteFunc(next, func(c *cachedEventFile) bool { return c == nil })
	return out, nil
}
