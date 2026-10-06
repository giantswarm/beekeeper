package guard

import (
	"bufio"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// A Leak is one reference or rule found in the transcripts: how often, in
// how many files and which. It never carries the value or where in a file
// it was.
type Leak struct {
	Finding
	Files int      `json:"files"`
	Paths []string `json:"paths"`
}

// SweepReport is what SweepTranscripts read and found.
type SweepReport struct {
	Files int    `json:"files"`
	Bytes int64  `json:"bytes"`
	Leaks []Leak `json:"leaks"`
}

// SweepTranscripts scans every transcript under dir (Claude Code's projects
// directory): the sessions' and subagents' .jsonl files, each line's
// strings decoded, and the spilled tool results (.txt) as they are. It
// counts what ix.Redact finds; it never writes.
func SweepTranscripts(dir string, ix *Index) (SweepReport, error) {
	var rep SweepReport
	byName := map[string]*Leak{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if p == dir {
				return err
			}
			return nil //nolint:nilerr // an unreadable entry is passed over
		}
		if !d.Type().IsRegular() {
			return nil
		}
		ext := filepath.Ext(p)
		if ext != ".jsonl" && (ext != ".txt" || filepath.Base(filepath.Dir(p)) != "tool-results") {
			return nil
		}
		found, n, err := sweepFile(p, ext == ".jsonl", ix)
		if err != nil {
			return nil //nolint:nilerr // a file that vanished or cannot be read is passed over
		}
		rep.Files++
		rep.Bytes += n
		for _, f := range found {
			l := byName[f.Name()]
			if l == nil {
				l = &Leak{Finding: Finding{Ref: f.Ref, Rule: f.Rule}}
				byName[f.Name()] = l
			}
			l.Count += f.Count
			l.Files++
			l.Paths = append(l.Paths, p)
		}
		return nil
	})
	for _, l := range byName {
		rep.Leaks = append(rep.Leaks, *l)
	}
	sort.Slice(rep.Leaks, func(i, j int) bool {
		a, b := rep.Leaks[i], rep.Leaks[j]
		if (a.Ref != "") != (b.Ref != "") {
			return a.Ref != ""
		}
		if a.Count != b.Count {
			return a.Count > b.Count
		}
		return a.Name() < b.Name()
	})
	return rep, err
}

// sweepFile counts what one transcript file carries and returns its size.
func sweepFile(p string, jsonl bool, ix *Index) ([]Finding, int64, error) {
	f, err := os.Open(p) //nolint:gosec // the user's own transcripts
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = f.Close() }()
	var all []Finding
	scan := func(s string) string {
		_, fs := ix.Redact(s)
		all = mergeFindings(all, fs)
		return s
	}
	r := bufio.NewReaderSize(f, 1<<20)
	var n int64
	if !jsonl {
		b, err := io.ReadAll(r)
		if err != nil {
			return nil, 0, err
		}
		scan(string(b))
		return all, int64(len(b)), nil
	}
	for {
		line, err := r.ReadBytes('\n')
		n += int64(len(line))
		if l := strings.TrimSpace(string(line)); l != "" {
			if v, derr := decodeJSON([]byte(l)); derr == nil {
				walkStrings(v, scan)
			} else {
				scan(l)
			}
		}
		if errors.Is(err, io.EOF) {
			return all, n, nil
		}
		if err != nil {
			return nil, n, err
		}
	}
}
