package omp

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/giantswarm/beekeeper/internal/claude"
)

// SpendTranscripts reads the requests from from up to to of every session
// file under dir: a session's file in its working directory's folder, its
// subagents' in the session's own folder below that. A file last written
// before from has none and is not read; without dir there are none.
func SpendTranscripts(dir string, from, to time.Time) []claude.SpendTranscript {
	var out []claude.SpendTranscript
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() || filepath.Ext(p) != ".jsonl" {
			return nil //nolint:nilerr // an unreadable entry, or no omp, is passed over
		}
		if fi, err := d.Info(); err != nil || fi.ModTime().Before(from) {
			return nil //nolint:nilerr // as above
		}
		rel, _ := filepath.Rel(dir, p)
		out = append(out, claude.SpendTranscript{
			Subagent: strings.Count(rel, string(filepath.Separator)) > 1,
			Requests: spendRequests(p, from, to),
		})
		return nil
	})
	return out
}

// spendRequests reads the whole session file at p for its requests from
// from up to to: each assistant message with its usage, named
// provider/model as omp selects it. A response counts once by its id and
// time, however many files carry it (a fork copies its history). A file
// that cannot be opened has none.
func spendRequests(p string, from, to time.Time) []claude.SpendRequest {
	f, err := os.Open(filepath.Clean(p))
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()
	r := bufio.NewReaderSize(f, 1<<20)
	var out []claude.SpendRequest
	for {
		line, err := r.ReadBytes('\n')
		if bytes.Contains(line, []byte(`"usage"`)) && bytes.Contains(line, []byte(`"role":"assistant"`)) {
			var e entry
			if json.Unmarshal(line, &e) == nil && e.Type == typeMessage && e.Message != nil &&
				e.Message.Role == roleAssistant && e.Message.Usage != nil {
				m, u := e.Message, e.Message.Usage
				req := claude.SpendRequest{
					At:     e.Timestamp,
					Model:  m.Model,
					Tokens: claude.Tokens{Input: u.Input, Output: u.Output, CacheRead: u.CacheRead, CacheWrite5m: u.CacheWrite},
				}
				if m.Provider != "" {
					req.Model = m.Provider + "/" + m.Model
				}
				if m.ResponseID != "" {
					req.ID = m.ResponseID + "@" + e.Timestamp.Format(time.RFC3339Nano)
				}
				out = append(out, req)
			}
		}
		if err != nil {
			break // io.EOF, or a file cut short: what was read counts
		}
	}
	return claude.InWindow(out, from, to)
}
