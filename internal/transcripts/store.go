// Package transcripts collects, redacts, indexes and searches session
// transcripts across harnesses (claude, agy, codex, omp) and execution hosts.
package transcripts

import (
	"bufio"
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/giantswarm/beekeeper/internal/guard"
)

// Roles of stored transcript turns.
const (
	RoleUser      = "user"
	RoleAssistant = "assistant"
	RoleTool      = "tool"
)

// Turn is one conversational or tool turn in a stored transcript.
type Turn struct {
	At     time.Time `json:"at,omitzero"`
	Role   string    `json:"role"`
	Tool   string    `json:"tool,omitempty"`
	Text   string    `json:"text"`
	Result string    `json:"result,omitempty"`
	// Answered says a result arrived, however empty.
	Answered bool `json:"answered,omitempty"`
	// Phase is a Codex assistant message's phase ("commentary",
	// "final_answer"), empty where the harness names none.
	Phase string `json:"phase,omitempty"`
}

// Meta is the persisted metadata of one session's transcript in the store.
// It survives the session ending or leaving the roster.
type Meta struct {
	Key          string    `json:"key"`
	ID           string    `json:"id,omitempty"`
	HostSession  string    `json:"hostSession,omitempty"`
	Execution    string    `json:"execution,omitempty"`
	Name         string    `json:"name,omitempty"`
	RosterName   string    `json:"rosterName,omitempty"`
	Role         string    `json:"role,omitempty"`
	Repo         string    `json:"repo,omitempty"`
	Ref          string    `json:"ref,omitempty"`
	Refs         []string  `json:"refs,omitempty"`
	Harness      string    `json:"harness"`
	Host         string    `json:"host"`
	Cwd          string    `json:"cwd,omitempty"`
	Model        string    `json:"model,omitempty"`
	Running      bool      `json:"running,omitempty"`
	StartedAt    time.Time `json:"startedAt,omitzero"`
	LastActive   time.Time `json:"lastActive,omitzero"`
	EndedAt      time.Time `json:"endedAt,omitzero"`
	Turns        int       `json:"turns"`
	Bytes        int64     `json:"bytes"`
	SourcePath   string    `json:"sourcePath,omitempty"`
	SourceMod    time.Time `json:"sourceMod,omitzero"`
	SourceSize   int64     `json:"sourceSize,omitempty"`
	RemoteCursor string    `json:"remoteCursor,omitempty"`
	// Parser is the parser version that indexed the turns (parserVersions).
	Parser int `json:"parser,omitempty"`
}

// DisplayName is the most descriptive human-readable name for the session.
func (m Meta) DisplayName() string {
	return cmp.Or(m.RosterName, m.Name, m.ID, m.HostSession, m.Execution, m.Key)
}

// Matches reports whether q (a session name, roster name, id, execution id,
// key or issue/pull-request reference) identifies m.
func (m Meta) Matches(q string) bool {
	q = strings.TrimSpace(q)
	if q == "" {
		return false
	}
	if m.ExactMatch(q) {
		return true
	}
	if ref := NormalizeRef(q); ref != "" {
		if strings.EqualFold(m.Ref, ref) {
			return true
		}
		for _, r := range m.Refs {
			if strings.EqualFold(r, ref) {
				return true
			}
		}
	}
	lq := strings.ToLower(q)
	for _, cand := range []string{m.RosterName, m.Name, m.ID, m.HostSession, m.Execution, m.Key} {
		if cand != "" && strings.Contains(strings.ToLower(cand), lq) {
			return true
		}
	}
	return false
}

// ExactMatch reports whether q equals one of m's identifiers or names.
func (m Meta) ExactMatch(q string) bool {
	q = strings.TrimSpace(q)
	if q == "" {
		return false
	}
	for _, id := range []string{m.Key, m.ID, m.HostSession, m.Execution} {
		if id != "" && strings.EqualFold(id, q) {
			return true
		}
	}
	for _, name := range []string{m.RosterName, m.Name} {
		if name != "" && strings.EqualFold(name, q) {
			return true
		}
	}
	return false
}

// Store manages the coordinator's transcript directory (<stateDir>/transcripts).
type Store struct {
	Root   string
	Redact func(string) string
}

func (s Store) redact(text string) string {
	if text == "" {
		return ""
	}
	if s.Redact != nil {
		return s.Redact(text)
	}
	out, _ := (&guard.Index{}).Redact(text)
	return out
}

var unsafeKeyChars = regexp.MustCompile(`[^a-zA-Z0-9._-]+`)

// SafeKey builds a flat filename-safe key for a session transcript.
func SafeKey(harness, host, id string) string {
	h := sanitizePart(cmp.Or(harness, "claude"))
	ho := sanitizePart(cmp.Or(host, "local"))
	i := sanitizePart(id)
	if i == "" {
		i = "unknown"
	}
	return h + "-" + ho + "-" + i
}

func sanitizePart(s string) string {
	s = strings.TrimSpace(s)
	s = unsafeKeyChars.ReplaceAllString(s, "_")
	s = strings.Trim(s, "._-")
	if len(s) > 96 {
		s = s[:96]
	}
	return s
}

func (s Store) metaPath(key string) string {
	return filepath.Join(s.Root, sanitizePart(key)+".json")
}

func (s Store) turnsPath(key string) string {
	return filepath.Join(s.Root, sanitizePart(key)+".jsonl")
}

func (s Store) rawPath(key string) string {
	return filepath.Join(s.Root, sanitizePart(key)+".raw")
}

// ReadMeta reads the stored Meta for key; found is false when absent.
func (s Store) ReadMeta(key string) (Meta, bool, error) {
	b, err := os.ReadFile(s.metaPath(key))
	if errors.Is(err, os.ErrNotExist) {
		return Meta{}, false, nil
	}
	if err != nil {
		return Meta{}, false, err
	}
	var m Meta
	if err := json.Unmarshal(b, &m); err != nil {
		return Meta{}, false, err
	}
	return m, true, nil
}

// mergeMeta combines fresh metadata with previously persisted metadata so
// fields known while a session ran (roster name, repository, refs, start
// time) survive after the session ends and leaves the roster.
func mergeMeta(prev, next Meta) Meta {
	out := prev
	out.Key = cmp.Or(next.Key, prev.Key)
	out.ID = cmp.Or(next.ID, prev.ID)
	out.HostSession = cmp.Or(next.HostSession, prev.HostSession)
	out.Execution = cmp.Or(next.Execution, prev.Execution)
	out.Name = cmp.Or(next.Name, prev.Name)
	out.RosterName = cmp.Or(next.RosterName, prev.RosterName)
	out.Role = cmp.Or(next.Role, prev.Role)
	out.Repo = cmp.Or(next.Repo, prev.Repo)
	out.Ref = cmp.Or(next.Ref, prev.Ref)
	out.Refs = mergeRefs(prev.Refs, next.Refs, out.Ref)
	if out.Ref == "" && len(out.Refs) > 0 {
		out.Ref = out.Refs[0]
	}
	if out.Repo == "" && out.Ref != "" {
		out.Repo = RepoOfRef(out.Ref)
	}
	out.Harness = cmp.Or(next.Harness, prev.Harness, "claude")
	out.Host = cmp.Or(next.Host, prev.Host, "local")
	out.Cwd = cmp.Or(next.Cwd, prev.Cwd)
	out.Model = cmp.Or(next.Model, prev.Model)
	out.Running = next.Running
	if !next.StartedAt.IsZero() && (out.StartedAt.IsZero() || next.StartedAt.Before(out.StartedAt)) {
		out.StartedAt = next.StartedAt.UTC()
	}
	if !next.LastActive.IsZero() && next.LastActive.After(out.LastActive) {
		out.LastActive = next.LastActive.UTC()
	}
	if !next.EndedAt.IsZero() {
		out.EndedAt = next.EndedAt.UTC()
	} else if !next.Running && out.EndedAt.IsZero() && !out.LastActive.IsZero() {
		out.EndedAt = out.LastActive.UTC()
	}
	if next.Running {
		out.EndedAt = time.Time{}
	}
	if next.SourcePath != "" {
		out.SourcePath = next.SourcePath
		out.SourceMod = next.SourceMod.UTC()
		out.SourceSize = next.SourceSize
	}
	if next.RemoteCursor != "" {
		out.RemoteCursor = next.RemoteCursor
	}
	out.Parser = max(prev.Parser, next.Parser)
	if next.Turns > 0 || prev.Turns == 0 {
		out.Turns = next.Turns
	}
	if next.Bytes > 0 || prev.Bytes == 0 {
		out.Bytes = next.Bytes
	}
	return out
}

func mergeRefs(a, b []string, primary string) []string {
	var out []string
	add := func(r string) {
		r = NormalizeRef(r)
		if r == "" {
			return
		}
		if !slices.ContainsFunc(out, func(x string) bool { return strings.EqualFold(x, r) }) {
			out = append(out, r)
		}
	}
	add(primary)
	for _, r := range a {
		add(r)
	}
	for _, r := range b {
		add(r)
	}
	return out
}

// Save redacts turns, merges metadata with any prior record for meta.Key, and
// atomically writes both <key>.jsonl and <key>.json.
func (s Store) Save(meta Meta, turns []Turn) (Meta, error) {
	if s.Root == "" {
		return Meta{}, errors.New("transcript store root is empty")
	}
	if err := os.MkdirAll(s.Root, 0o700); err != nil {
		return Meta{}, err
	}
	if meta.Key == "" {
		meta.Key = SafeKey(meta.Harness, meta.Host, cmp.Or(meta.Execution, meta.HostSession, meta.ID, meta.RosterName, meta.Name))
	} else {
		meta.Key = sanitizePart(meta.Key)
	}
	prev, hadPrev, err := s.ReadMeta(meta.Key)
	if err != nil {
		return Meta{}, err
	}

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	var firstAt, lastAt time.Time
	var refs []string
	kept := 0
	for _, t := range turns {
		t.At = t.At.UTC()
		t.Text = strings.TrimSpace(s.redact(t.Text))
		t.Tool = strings.TrimSpace(s.redact(t.Tool))
		t.Result = strings.TrimSpace(s.redact(t.Result))
		if t.Text == "" && t.Tool == "" && t.Result == "" {
			continue
		}
		if !t.At.IsZero() {
			if firstAt.IsZero() || t.At.Before(firstAt) {
				firstAt = t.At
			}
			if t.At.After(lastAt) {
				lastAt = t.At
			}
		}
		refs = append(refs, ExtractRefs(t.Text)...)
		if err := enc.Encode(t); err != nil {
			return Meta{}, err
		}
		kept++
	}
	if !firstAt.IsZero() && (meta.StartedAt.IsZero() || firstAt.Before(meta.StartedAt)) {
		meta.StartedAt = firstAt
	}
	if !lastAt.IsZero() && lastAt.After(meta.LastActive) {
		meta.LastActive = lastAt
	}
	meta.Refs = mergeRefs(meta.Refs, refs, meta.Ref)
	if kept > 0 || !hadPrev {
		meta.Turns = kept
		meta.Bytes = int64(buf.Len())
		if err := writeAtomic(s.turnsPath(meta.Key), buf.Bytes()); err != nil {
			return Meta{}, err
		}
	}
	merged := mergeMeta(prev, meta)
	rawMeta, err := json.MarshalIndent(merged, "", "  ")
	if err != nil {
		return Meta{}, err
	}
	if err := writeAtomic(s.metaPath(meta.Key), append(rawMeta, '\n')); err != nil {
		return Meta{}, err
	}
	return merged, nil
}

// UpdateMeta updates stored metadata for meta.Key without rewriting its turns.
func (s Store) UpdateMeta(meta Meta) (Meta, error) {
	if s.Root == "" {
		return Meta{}, errors.New("transcript store root is empty")
	}
	if err := os.MkdirAll(s.Root, 0o700); err != nil {
		return Meta{}, err
	}
	if meta.Key == "" {
		meta.Key = SafeKey(meta.Harness, meta.Host, cmp.Or(meta.Execution, meta.HostSession, meta.ID, meta.RosterName, meta.Name))
	} else {
		meta.Key = sanitizePart(meta.Key)
	}
	prev, _, err := s.ReadMeta(meta.Key)
	if err != nil {
		return Meta{}, err
	}
	merged := mergeMeta(prev, meta)
	rawMeta, err := json.MarshalIndent(merged, "", "  ")
	if err != nil {
		return Meta{}, err
	}
	if err := writeAtomic(s.metaPath(meta.Key), append(rawMeta, '\n')); err != nil {
		return Meta{}, err
	}
	return merged, nil
}

// IngestFile parses a local harness transcript at path and saves it into the
// store. If path's size and modification time match what is already stored for
// meta.Key, only changed metadata fields are updated.
func (s Store) IngestFile(meta Meta, path string) (Meta, bool, error) {
	if path == "" {
		return Meta{}, false, nil
	}
	fi, err := os.Stat(filepath.Clean(path))
	if errors.Is(err, os.ErrNotExist) || (err == nil && fi.IsDir()) {
		return Meta{}, false, nil
	}
	if err != nil {
		return Meta{}, false, err
	}
	if meta.Key == "" {
		meta.Key = SafeKey(meta.Harness, meta.Host, cmp.Or(meta.Execution, meta.HostSession, meta.ID, filepath.Base(path)))
	} else {
		meta.Key = sanitizePart(meta.Key)
	}
	meta.SourcePath = filepath.Clean(path)
	meta.SourceMod = fi.ModTime().UTC()
	meta.SourceSize = fi.Size()
	if meta.LastActive.IsZero() {
		meta.LastActive = fi.ModTime().UTC()
	}

	if prev, ok, err := s.ReadMeta(meta.Key); err == nil && ok {
		if prev.SourcePath == meta.SourcePath && prev.SourceSize == meta.SourceSize && prev.SourceMod.Equal(meta.SourceMod) && prev.Turns > 0 && prev.Parser >= parserVersions[meta.Harness] {
			updated, err := s.UpdateMeta(meta)
			return updated, false, err
		}
	}
	meta.Parser = parserVersions[meta.Harness]

	parsed, err := ParseFile(meta.Harness, path)
	if err != nil {
		return Meta{}, false, err
	}
	if meta.ID == "" {
		meta.ID = parsed.SessionID
	}
	if meta.Name == "" {
		meta.Name = parsed.Title
	}
	if meta.Cwd == "" {
		meta.Cwd = parsed.Cwd
	}
	if meta.Model == "" {
		meta.Model = parsed.Model
	}
	if meta.Repo == "" {
		meta.Repo = parsed.Repo
	}
	meta.Refs = mergeRefs(meta.Refs, parsed.Refs, meta.Ref)
	if meta.StartedAt.IsZero() && !parsed.StartedAt.IsZero() {
		meta.StartedAt = parsed.StartedAt
	}
	if !parsed.LastActive.IsZero() && parsed.LastActive.After(meta.LastActive) {
		meta.LastActive = parsed.LastActive
	}
	saved, err := s.Save(meta, parsed.Turns)
	return saved, true, err
}

// AppendRemoteLog appends a page of an SSH execution's stream log (after
// redacting secret values), reparses the accumulated stream with any prompt
// turns from the execution record, and updates the stored transcript. The
// chunk ends at a record boundary: redaction matches whole records only, so
// a secret split across two chunks would reach the raw stream unmasked.
func (s Store) AppendRemoteLog(meta Meta, chunk []byte, nextCursor string, prompts []Turn) (Meta, error) {
	if s.Root == "" {
		return Meta{}, errors.New("transcript store root is empty")
	}
	if err := os.MkdirAll(s.Root, 0o700); err != nil {
		return Meta{}, err
	}
	if meta.Key == "" {
		meta.Key = SafeKey(meta.Harness, meta.Host, cmp.Or(meta.Execution, meta.ID, meta.RosterName, meta.Name))
	} else {
		meta.Key = sanitizePart(meta.Key)
	}
	rawFile := s.rawPath(meta.Key)
	if len(chunk) > 0 {
		redacted := s.redact(string(chunk))
		// #nosec G304 -- rawPath confines the sanitized transcript key to the store root.
		f, err := os.OpenFile(rawFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			return Meta{}, err
		}
		_, writeErr := f.WriteString(redacted)
		closeErr := f.Close()
		if writeErr != nil {
			return Meta{}, writeErr
		}
		if closeErr != nil {
			return Meta{}, closeErr
		}
	}
	// #nosec G304 -- rawPath confines the sanitized transcript key to the store root.
	raw, err := os.ReadFile(rawFile)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return Meta{}, err
	}
	parsed := ParseRemoteStream(meta.Harness, raw, prompts)
	if meta.ID == "" {
		meta.ID = parsed.SessionID
	}
	if meta.Cwd == "" {
		meta.Cwd = parsed.Cwd
	}
	if meta.Model == "" {
		meta.Model = parsed.Model
	}
	if meta.Repo == "" {
		meta.Repo = parsed.Repo
	}
	meta.Refs = mergeRefs(meta.Refs, parsed.Refs, meta.Ref)
	if nextCursor != "" {
		meta.RemoteCursor = nextCursor
	}
	return s.Save(meta, parsed.Turns)
}

// List returns all stored transcript metadata entries, most recently active first.
func (s Store) List() ([]Meta, error) {
	entries, err := os.ReadDir(s.Root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Meta
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(s.Root, e.Name()))
		if err != nil {
			continue
		}
		var m Meta
		if json.Unmarshal(b, &m) == nil && m.Key != "" {
			out = append(out, m)
		}
	}
	slices.SortFunc(out, func(a, b Meta) int {
		if c := b.LastActive.Compare(a.LastActive); c != 0 {
			return c
		}
		if c := b.StartedAt.Compare(a.StartedAt); c != 0 {
			return c
		}
		return strings.Compare(a.Key, b.Key)
	})
	return out, nil
}

// Resolve finds the transcript matching query among metas (which should
// already be filtered for caller access and sorted most-recent first).
func Resolve(metas []Meta, query string) (Meta, error) {
	q := strings.TrimSpace(query)
	if q == "" {
		return Meta{}, errors.New("session name, id or issue/pull-request reference is required")
	}
	// 1. Exact ID / Key / HostSession / Execution match.
	for _, m := range metas {
		for _, id := range []string{m.Key, m.ID, m.HostSession, m.Execution} {
			if id != "" && strings.EqualFold(id, q) {
				return m, nil
			}
		}
	}
	// 2. Exact RosterName or Name match (metas are sorted most-recent first).
	for _, m := range metas {
		if (m.RosterName != "" && strings.EqualFold(m.RosterName, q)) || (m.Name != "" && strings.EqualFold(m.Name, q)) {
			return m, nil
		}
	}
	// 3. Issue or pull request reference match (owner/repo#n or full GitHub URL).
	if ref := NormalizeRef(q); ref != "" {
		for _, m := range metas {
			if strings.EqualFold(m.Ref, ref) {
				return m, nil
			}
		}
		for _, m := range metas {
			if slices.ContainsFunc(m.Refs, func(r string) bool { return strings.EqualFold(r, ref) }) {
				return m, nil
			}
		}
	}
	// 4. Substring match on RosterName, Name, ID, HostSession, Execution, or Key.
	var matches []Meta
	for _, m := range metas {
		if m.Matches(q) {
			matches = append(matches, m)
		}
	}
	if len(matches) == 1 {
		return matches[0], nil
	}
	if len(matches) > 1 {
		// If all matches share the same RosterName, return the most recent one.
		sameRoster := matches[0].RosterName != ""
		for _, m := range matches[1:] {
			if !strings.EqualFold(m.RosterName, matches[0].RosterName) {
				sameRoster = false
				break
			}
		}
		if sameRoster {
			return matches[0], nil
		}
		var names []string
		for _, m := range matches[:min(5, len(matches))] {
			names = append(names, fmt.Sprintf("%s (%s)", m.DisplayName(), m.Key))
		}
		return Meta{}, fmt.Errorf("%q matches %d transcripts: %s", query, len(matches), strings.Join(names, ", "))
	}
	return Meta{}, fmt.Errorf("no accessible transcript found for %q", query)
}

// ReadOptions controls which turns ReadTurns returns.
type ReadOptions struct {
	Last    int
	Tools   bool
	Results bool
}

// ReadTurns reads the stored turns of key according to opts.
func (s Store) ReadTurns(key string, opts ReadOptions) ([]Turn, error) {
	f, err := os.Open(s.turnsPath(key))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	includeTools := opts.Tools || opts.Results
	var turns []Turn
	r := bufio.NewReaderSize(f, 256<<10)
	for {
		line, readErr := r.ReadBytes('\n')
		line = bytes.TrimSpace(line)
		if len(line) > 0 {
			var t Turn
			if json.Unmarshal(line, &t) == nil {
				if t.Role == RoleTool && !includeTools {
					goto next
				}
				if !opts.Results {
					t.Result = ""
				}
				turns = append(turns, t)
			}
		}
	next:
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return nil, readErr
		}
	}
	if opts.Last > 0 && len(turns) > opts.Last {
		turns = turns[len(turns)-opts.Last:]
	}
	return turns, nil
}

// SearchOptions filters and bounds a transcript search.
type SearchOptions struct {
	Repo    string
	Harness string
	Host    string
	Ref     string
	Session string
	Since   time.Time
	Tools   bool
	Results bool
	Limit   int
}

// SearchHit is one matching turn from Search.
type SearchHit struct {
	Meta      Meta   `json:"meta"`
	TurnIndex int    `json:"turnIndex"`
	Turn      Turn   `json:"turn"`
	Snippet   string `json:"snippet"`
}

// Search searches accessible transcripts (metas) for query.
func (s Store) Search(metas []Meta, query string, opts SearchOptions) ([]SearchHit, error) {
	q := strings.TrimSpace(query)
	if q == "" {
		return nil, errors.New("search query is required")
	}
	lq := strings.ToLower(q)
	terms := strings.Fields(lq)
	refFilter := NormalizeRef(opts.Ref)
	limit := opts.Limit
	if limit <= 0 {
		limit = 50
	}

	var hits []SearchHit
	for _, m := range metas {
		if opts.Repo != "" && !strings.EqualFold(m.Repo, opts.Repo) && !strings.EqualFold(RepoOfRef(m.Ref), opts.Repo) {
			continue
		}
		if opts.Harness != "" && !strings.EqualFold(m.Harness, opts.Harness) {
			continue
		}
		if opts.Host != "" && !strings.EqualFold(m.Host, opts.Host) {
			continue
		}
		if refFilter != "" && !strings.EqualFold(m.Ref, refFilter) && !slices.ContainsFunc(m.Refs, func(r string) bool { return strings.EqualFold(r, refFilter) }) {
			continue
		}
		if opts.Session != "" && !m.Matches(opts.Session) {
			continue
		}
		if !opts.Since.IsZero() && !m.LastActive.IsZero() && m.LastActive.Before(opts.Since) {
			continue
		}

		turns, err := s.ReadTurns(m.Key, ReadOptions{Tools: opts.Tools, Results: opts.Results})
		if err != nil {
			continue
		}
		for idx, t := range turns {
			if !opts.Since.IsZero() && !t.At.IsZero() && t.At.Before(opts.Since) {
				continue
			}
			hay := t.Text
			if t.Tool != "" && !strings.Contains(hay, t.Tool) {
				hay = t.Tool + "\n" + hay
			}
			if opts.Results && t.Result != "" {
				hay = hay + "\n" + t.Result
			}
			if !matchesQuery(hay, lq, terms) {
				continue
			}
			hits = append(hits, SearchHit{
				Meta:      m,
				TurnIndex: idx + 1,
				Turn:      t,
				Snippet:   snippetAround(hay, lq, terms, 200),
			})
			if len(hits) >= limit {
				return hits, nil
			}
		}
	}
	return hits, nil
}

func matchesQuery(text, lq string, terms []string) bool {
	lt := strings.ToLower(text)
	if strings.Contains(lt, lq) {
		return true
	}
	if len(terms) <= 1 {
		return false
	}
	for _, term := range terms {
		if !strings.Contains(lt, term) {
			return false
		}
	}
	return true
}

func snippetAround(text, lq string, terms []string, maxLen int) string {
	flat := strings.Join(strings.Fields(text), " ")
	if len(flat) <= maxLen {
		return flat
	}
	lf := strings.ToLower(flat)
	pos := strings.Index(lf, lq)
	if pos < 0 && len(terms) > 0 {
		pos = strings.Index(lf, terms[0])
	}
	if pos < 0 {
		return flat[:maxLen-1] + "…"
	}
	start := max(0, pos-maxLen/3)
	end := min(len(flat), start+maxLen)
	if end-start < maxLen && start > 0 {
		start = max(0, end-maxLen)
	}
	out := flat[start:end]
	if start > 0 {
		out = "…" + out
	}
	if end < len(flat) {
		out = out + "…"
	}
	return out
}

func writeAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), path)
}
