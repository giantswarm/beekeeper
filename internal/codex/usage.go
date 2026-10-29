package codex

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

const (
	evTokenCount        = "token_count"
	fiveHourWindowMin   = 300
	sevenDayWindowMin   = 10080
	maxRolloutTailBytes = 4 * 1024 * 1024
	rolloutModSlack     = time.Minute
	epochMilliFloor     = 100_000_000_000
	slotPrimary         = "primary"
	slotSecondary       = "secondary"
	jsonNullLiteral     = "null"
)

// UsageWindow is one subscription rate-limit window reported by Codex CLI in
// a rollout `token_count` event (`primary` or `secondary`).
type UsageWindow struct {
	UsedPercent   float64
	ResetsAt      time.Time
	WindowMinutes int
}

// UsageCredits holds the optional credit balance section of a Codex
// `rate_limits` payload.
type UsageCredits struct {
	HasCredits bool
	Unlimited  bool
	Balance    *float64
}

// UsageReading is the freshest account-level plan usage event read across
// Codex rollout transcripts.
type UsageReading struct {
	At       time.Time
	FiveHour *UsageWindow
	SevenDay *UsageWindow
	PlanType string
	Credits  *UsageCredits
}

// HasRollouts reports whether at least one rollout `.jsonl` file exists under
// the isolated worker CODEX_HOME sessions directory or sessionsDir.
func HasRollouts(stateDir, sessionsDir string, extraDirs ...string) bool {
	found := false
	for _, dir := range sessionDirs(stateDir, sessionsDir, extraDirs...) {
		dir = strings.TrimSpace(dir)
		if dir == "" {
			continue
		}
		_ = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil
			}
			if strings.HasSuffix(d.Name(), ".jsonl") {
				found = true
				return fs.SkipAll
			}
			return nil
		})
		if found {
			return true
		}
	}
	return false
}

// ReadUsage scans Codex rollout transcripts across the isolated worker
// CODEX_HOME (`<stateDir>/codex/home/sessions`) and `sessionsDir`
// (`~/.codex/sessions`), selects the freshest `token_count` event across all
// sessions (since all sessions share one account), and parses its
// `rate_limits` payload.
//
// A rollout transcript that has not yet written any `token_count` event (such
// as a newly started session that has written only `session_meta` or
// `turn_context`) produces no candidate and never shadows a valid reading from
// another session. An explicit invalid, stopped or truncated/corrupt rollout
// event is recorded at its timestamp (or the later of the previous line's
// timestamp and the file's mtime) and fails closed when it is the freshest
// event.
func ReadUsage(stateDir, sessionsDir string, extraDirs ...string) (UsageReading, error) {
	files := rolloutFiles(sessionDirs(stateDir, sessionsDir, extraDirs...))
	if len(files) == 0 {
		return UsageReading{}, errors.New("no codex rollout files found")
	}
	var best *usageCandidate
	for _, rf := range files {
		if best != nil && !best.at.IsZero() && rf.mod.Before(best.at.Add(-rolloutModSlack)) {
			break
		}
		if c := scanRolloutUsage(rf); c != nil {
			if best == nil || candidateNewer(*c, *best) {
				best = c
			}
		}
	}
	if best == nil {
		return UsageReading{}, errors.New("no codex token_count usage event found")
	}
	if best.err != nil {
		return UsageReading{}, best.err
	}
	return best.reading, nil
}

type rolloutRef struct {
	path string
	mod  time.Time
}

type usageCandidate struct {
	at      time.Time
	mod     time.Time
	path    string
	seq     int
	reading UsageReading
	err     error
}

func candidateNewer(a, b usageCandidate) bool {
	if !a.at.Equal(b.at) {
		return a.at.After(b.at)
	}
	if !a.mod.Equal(b.mod) {
		return a.mod.After(b.mod)
	}
	if a.path != b.path {
		return a.path > b.path
	}
	return a.seq > b.seq
}

func rolloutFiles(dirs []string) []rolloutRef {
	seen := map[string]bool{}
	var out []rolloutRef
	for _, dir := range dirs {
		dir = strings.TrimSpace(dir)
		if dir == "" {
			continue
		}
		_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil
			}
			if !strings.HasSuffix(d.Name(), ".jsonl") {
				return nil
			}
			clean := filepath.Clean(path)
			if seen[clean] {
				return nil
			}
			fi, statErr := d.Info()
			if statErr != nil {
				return nil
			}
			seen[clean] = true
			out = append(out, rolloutRef{
				path: clean,
				mod:  fi.ModTime().UTC(),
			})
			return nil
		})
	}
	slices.SortFunc(out, func(a, b rolloutRef) int {
		if c := b.mod.Compare(a.mod); c != 0 {
			return c
		}
		return strings.Compare(b.path, a.path)
	})
	return out
}

func scanRolloutUsage(rf rolloutRef) *usageCandidate {
	f, err := os.Open(rf.path)
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()

	soughtTail := false
	if fi, statErr := f.Stat(); statErr == nil && fi.Size() > maxRolloutTailBytes {
		if _, seekErr := f.Seek(fi.Size()-maxRolloutTailBytes-1, io.SeekStart); seekErr == nil {
			soughtTail = true
		}
	}

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), maxLine)

	if soughtTail {
		// Discard the partial leading line starting at the seek offset.
		_ = sc.Scan()
	}

	var lastLineAt time.Time
	var best *usageCandidate
	seq := 0

	recordErr := func(raw []byte, seqNum int, cerr error) {
		c := usageCandidate{
			at:   corruptLineAt(raw, lastLineAt, rf.mod),
			mod:  rf.mod,
			path: rf.path,
			seq:  seqNum,
			err:  cerr,
		}
		if best == nil || candidateNewer(c, *best) {
			best = &c
		}
	}

	for sc.Scan() {
		seq++
		raw := bytes.TrimSpace(sc.Bytes())
		if len(raw) == 0 {
			continue
		}
		if raw[0] != '{' || !json.Valid(raw) {
			recordErr(raw, seq, errors.New("invalid codex token_count or rollout JSON line"))
			continue
		}
		if ts := extractRawTimestamp(raw); !ts.IsZero() {
			lastLineAt = ts
		}
		if !bytes.Contains(raw, []byte(evTokenCount)) && !bytes.Contains(raw, []byte("rate_limits")) {
			continue
		}
		var line struct {
			Timestamp  time.Time       `json:"timestamp"`
			Type       string          `json:"type"`
			Payload    json.RawMessage `json:"payload"`
			RateLimits json.RawMessage `json:"rate_limits"`
		}
		if uerr := json.Unmarshal(raw, &line); uerr != nil {
			recordErr(raw, seq, fmt.Errorf("invalid codex token_count JSON: %w", uerr))
			continue
		}
		if !line.Timestamp.IsZero() {
			lastLineAt = line.Timestamp.UTC()
		}

		isTokenCount, rawRateLimits, payloadAt, perr := extractTokenCountRateLimits(line.Type, line.Payload, line.RateLimits)
		if !isTokenCount {
			continue
		}
		at := line.Timestamp.UTC()
		if at.IsZero() {
			at = payloadAt
		}
		if at.IsZero() {
			at = lastLineAt
		}
		if at.IsZero() {
			at = rf.mod
		}
		var c usageCandidate
		c.at = at
		c.mod = rf.mod
		c.path = rf.path
		c.seq = seq
		if perr != nil {
			c.err = perr
		} else {
			c.reading, c.err = ParseRateLimits(rawRateLimits, at)
		}
		if best == nil || candidateNewer(c, *best) {
			best = &c
		}
	}

	if scanErr := sc.Err(); scanErr != nil {
		recordErr(nil, seq+1, fmt.Errorf("codex rollout scan error: %w", scanErr))
	} else if soughtTail && seq == 0 {
		recordErr(nil, 1, errors.New("truncated codex rollout tail"))
	}
	return best
}

func corruptLineAt(raw []byte, lastLineAt, mod time.Time) time.Time {
	if ts := extractRawTimestamp(raw); !ts.IsZero() {
		return ts.UTC()
	}
	if lastLineAt.After(mod) {
		return lastLineAt
	}
	return mod
}

func extractRawTimestamp(raw []byte) time.Time {
	const key = `"timestamp"`
	idx := bytes.Index(raw, []byte(key))
	if idx < 0 {
		return time.Time{}
	}
	rest := bytes.TrimSpace(raw[idx+len(key):])
	if len(rest) == 0 || rest[0] != ':' {
		return time.Time{}
	}
	rest = bytes.TrimSpace(rest[1:])
	if len(rest) < 2 || rest[0] != '"' {
		return time.Time{}
	}
	rest = rest[1:]
	end := bytes.IndexByte(rest, '"')
	if end <= 0 {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339Nano, string(rest[:end]))
	if err != nil {
		return time.Time{}
	}
	return t.UTC()
}

func extractTokenCountRateLimits(lineType string, rawPayload, topRateLimits json.RawMessage) (bool, json.RawMessage, time.Time, error) {
	switch lineType {
	case evEventMsg:
		trimmed := bytes.TrimSpace(rawPayload)
		if len(trimmed) == 0 || string(trimmed) == jsonNullLiteral {
			return false, nil, time.Time{}, nil
		}
		var p struct {
			Type       string          `json:"type"`
			Timestamp  time.Time       `json:"timestamp"`
			RateLimits json.RawMessage `json:"rate_limits"`
			Info       *struct {
				RateLimits json.RawMessage `json:"rate_limits"`
			} `json:"info"`
		}
		if err := json.Unmarshal(trimmed, &p); err != nil {
			if bytes.Contains(trimmed, []byte(evTokenCount)) || bytes.Contains(trimmed, []byte("rate_limits")) {
				return true, nil, time.Time{}, fmt.Errorf("invalid codex token_count payload: %w", err)
			}
			return false, nil, time.Time{}, nil
		}
		if p.Type != evTokenCount {
			return false, nil, time.Time{}, nil
		}
		rl := p.RateLimits
		if isNullOrEmptyJSON(rl) && p.Info != nil && !isNullOrEmptyJSON(p.Info.RateLimits) {
			rl = p.Info.RateLimits
		}
		if isNullOrEmptyJSON(rl) && !isNullOrEmptyJSON(topRateLimits) {
			rl = topRateLimits
		}
		return true, rl, p.Timestamp.UTC(), nil

	case evTokenCount:
		rl := topRateLimits
		var payloadAt time.Time
		if trimmed := bytes.TrimSpace(rawPayload); len(trimmed) > 0 && string(trimmed) != jsonNullLiteral {
			var p struct {
				Timestamp  time.Time       `json:"timestamp"`
				RateLimits json.RawMessage `json:"rate_limits"`
			}
			if err := json.Unmarshal(trimmed, &p); err != nil {
				return true, nil, time.Time{}, fmt.Errorf("invalid codex token_count payload: %w", err)
			}
			payloadAt = p.Timestamp.UTC()
			if isNullOrEmptyJSON(rl) {
				rl = p.RateLimits
			}
		}
		return true, rl, payloadAt, nil

	default:
		return false, nil, time.Time{}, nil
	}
}

// ParseRateLimits parses a Codex `rate_limits` JSON object (or an envelope
// containing `rate_limits`) at event timestamp `at`.
func ParseRateLimits(raw json.RawMessage, at time.Time) (UsageReading, error) {
	trimmed := bytes.TrimSpace(raw)
	if isNullOrEmptyJSON(trimmed) {
		return UsageReading{}, errors.New("codex token_count event has null rate_limits")
	}

	var env struct {
		Timestamp  time.Time       `json:"timestamp"`
		RateLimits json.RawMessage `json:"rate_limits"`
		Payload    *struct {
			Timestamp  time.Time       `json:"timestamp"`
			RateLimits json.RawMessage `json:"rate_limits"`
		} `json:"payload"`
	}
	if err := json.Unmarshal(trimmed, &env); err == nil {
		if at.IsZero() && !env.Timestamp.IsZero() {
			at = env.Timestamp.UTC()
		}
		if !isNullOrEmptyJSON(env.RateLimits) {
			trimmed = bytes.TrimSpace(env.RateLimits)
		} else if env.Payload != nil {
			if at.IsZero() && !env.Payload.Timestamp.IsZero() {
				at = env.Payload.Timestamp.UTC()
			}
			if !isNullOrEmptyJSON(env.Payload.RateLimits) {
				trimmed = bytes.TrimSpace(env.Payload.RateLimits)
			}
		}
	}
	if at.IsZero() {
		return UsageReading{}, errors.New("codex token_count event is missing timestamp")
	}
	at = at.UTC()

	var rl struct {
		Primary              json.RawMessage `json:"primary"`
		Secondary            json.RawMessage `json:"secondary"`
		PlanType             string          `json:"plan_type"`
		RateLimitReachedType *string         `json:"rate_limit_reached_type"`
		RateLimitReached     bool            `json:"rate_limit_reached"`
		SpendControlReached  bool            `json:"spend_control_reached"`
		Credits              *struct {
			HasCredits bool     `json:"has_credits"`
			Unlimited  bool     `json:"unlimited"`
			Balance    *float64 `json:"balance"`
		} `json:"credits"`
	}
	if err := json.Unmarshal(trimmed, &rl); err != nil {
		return UsageReading{}, fmt.Errorf("invalid codex rate_limits JSON: %w", err)
	}

	if rl.SpendControlReached {
		return UsageReading{}, errors.New("codex spend control reached")
	}
	if rl.RateLimitReachedType != nil {
		if kind := strings.TrimSpace(*rl.RateLimitReachedType); kind != "" {
			return UsageReading{}, fmt.Errorf("codex rate limit reached (%s)", kind)
		}
	}
	if rl.RateLimitReached {
		return UsageReading{}, errors.New("codex rate limit reached")
	}

	prim, err := parseRateLimitWindow(slotPrimary, rl.Primary, at)
	if err != nil {
		return UsageReading{}, err
	}
	sec, err := parseRateLimitWindow(slotSecondary, rl.Secondary, at)
	if err != nil {
		return UsageReading{}, err
	}
	if prim == nil && sec == nil {
		return UsageReading{}, errors.New("codex rate_limits has neither primary nor secondary window")
	}

	out := UsageReading{
		At:       at,
		PlanType: strings.TrimSpace(rl.PlanType),
	}
	if rl.Credits != nil {
		c := &UsageCredits{
			HasCredits: rl.Credits.HasCredits,
			Unlimited:  rl.Credits.Unlimited,
		}
		if rl.Credits.Balance != nil && !math.IsNaN(*rl.Credits.Balance) && !math.IsInf(*rl.Credits.Balance, 0) {
			b := *rl.Credits.Balance
			c.Balance = &b
		}
		out.Credits = c
	}

	for _, entry := range []struct {
		slot string
		win  *UsageWindow
	}{
		{slotPrimary, prim},
		{slotSecondary, sec},
	} {
		if entry.win == nil {
			continue
		}
		fiveH, sevenD, cerr := classifyRateLimitWindow(entry.slot, entry.win)
		if cerr != nil {
			return UsageReading{}, cerr
		}
		if fiveH {
			if out.FiveHour != nil {
				return UsageReading{}, errors.New("codex rate_limits has duplicate 5-hour windows")
			}
			out.FiveHour = entry.win
		} else if sevenD {
			if out.SevenDay != nil {
				return UsageReading{}, errors.New("codex rate_limits has duplicate 7-day windows")
			}
			out.SevenDay = entry.win
		}
	}

	if out.SevenDay == nil {
		return UsageReading{}, errors.New("codex rate_limits omits 7-day window")
	}
	return out, nil
}

func parseRateLimitWindow(slot string, raw json.RawMessage, ref time.Time) (*UsageWindow, error) {
	trimmed := bytes.TrimSpace(raw)
	if isNullOrEmptyJSON(trimmed) {
		return nil, nil
	}
	var w struct {
		UsedPercent      *float64        `json:"used_percent"`
		UsedPercentAlt   *float64        `json:"usedPercent"`
		WindowMinutes    *int            `json:"window_minutes"`
		WindowMinutesAlt *int            `json:"windowMinutes"`
		ResetsAt         json.RawMessage `json:"resets_at"`
		ResetsAtAlt      json.RawMessage `json:"resetsAt"`
		ResetsInSeconds  *int64          `json:"resets_in_seconds"`
	}
	if err := json.Unmarshal(trimmed, &w); err != nil {
		return nil, fmt.Errorf("codex %s: invalid JSON: %w", slot, err)
	}
	usedPtr := w.UsedPercent
	if usedPtr == nil {
		usedPtr = w.UsedPercentAlt
	}
	if usedPtr == nil {
		return nil, fmt.Errorf("codex %s.used_percent is missing", slot)
	}
	used := *usedPtr
	if math.IsNaN(used) || math.IsInf(used, 0) || used < 0 || used > 100 {
		return nil, fmt.Errorf("codex %s.used_percent %g is invalid", slot, used)
	}

	winPtr := w.WindowMinutes
	if winPtr == nil {
		winPtr = w.WindowMinutesAlt
	}
	if winPtr == nil {
		return nil, fmt.Errorf("codex %s.window_minutes is missing", slot)
	}
	winMin := *winPtr
	if winMin != fiveHourWindowMin && winMin != sevenDayWindowMin {
		return nil, fmt.Errorf("codex %s.window_minutes %d is unsupported (want %d or %d)", slot, winMin, fiveHourWindowMin, sevenDayWindowMin)
	}

	rawReset := w.ResetsAt
	if isNullOrEmptyJSON(rawReset) {
		rawReset = w.ResetsAtAlt
	}
	resetAt, err := parseResetTime(slot, rawReset, w.ResetsInSeconds, ref)
	if err != nil {
		return nil, err
	}
	if resetAt.IsZero() {
		return nil, fmt.Errorf("codex %s.resets_at is missing", slot)
	}
	if !ref.IsZero() {
		refUTC := ref.UTC()
		if !resetAt.After(refUTC) {
			return nil, fmt.Errorf("codex %s.resets_at %s is not after event timestamp %s", slot, resetAt.Format(time.RFC3339), refUTC.Format(time.RFC3339))
		}
		maxWindow := time.Duration(winMin) * time.Minute
		if resetAt.Sub(refUTC) > maxWindow {
			return nil, fmt.Errorf("codex %s.resets_at %s exceeds %dm window from %s", slot, resetAt.Format(time.RFC3339), winMin, refUTC.Format(time.RFC3339))
		}
	}

	return &UsageWindow{
		UsedPercent:   math.Round(used*10) / 10,
		ResetsAt:      resetAt,
		WindowMinutes: winMin,
	}, nil
}

func classifyRateLimitWindow(slot string, w *UsageWindow) (fiveHour, sevenDay bool, err error) {
	switch w.WindowMinutes {
	case fiveHourWindowMin:
		return true, false, nil
	case sevenDayWindowMin:
		return false, true, nil
	default:
		return false, false, fmt.Errorf("codex %s.window_minutes %d is unsupported (want %d or %d)", slot, w.WindowMinutes, fiveHourWindowMin, sevenDayWindowMin)
	}
}

func parseResetTime(slot string, raw json.RawMessage, resetsInSeconds *int64, ref time.Time) (time.Time, error) {
	trimmed := bytes.TrimSpace(raw)
	if !isNullOrEmptyJSON(trimmed) {
		if trimmed[0] == '"' {
			var s string
			if err := json.Unmarshal(trimmed, &s); err != nil {
				return time.Time{}, fmt.Errorf("codex %s.resets_at: %w", slot, err)
			}
			s = strings.TrimSpace(s)
			if s == "" {
				return time.Time{}, nil
			}
			if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
				return t.UTC(), nil
			}
			if sec, err := strconv.ParseInt(s, 10, 64); err == nil {
				return unixEpochToTime(slot, float64(sec))
			}
			return time.Time{}, fmt.Errorf("codex %s.resets_at %q is invalid", slot, s)
		}
		var num float64
		if err := json.Unmarshal(trimmed, &num); err != nil {
			return time.Time{}, fmt.Errorf("codex %s.resets_at: %w", slot, err)
		}
		return unixEpochToTime(slot, num)
	}
	if resetsInSeconds != nil {
		if *resetsInSeconds <= 0 {
			return time.Time{}, fmt.Errorf("codex %s.resets_in_seconds %d is invalid", slot, *resetsInSeconds)
		}
		if !ref.IsZero() {
			return ref.UTC().Add(time.Duration(*resetsInSeconds) * time.Second), nil
		}
	}
	return time.Time{}, nil
}

func unixEpochToTime(slot string, num float64) (time.Time, error) {
	if math.IsNaN(num) || math.IsInf(num, 0) || num <= 0 {
		return time.Time{}, fmt.Errorf("codex %s.resets_at %g is invalid", slot, num)
	}
	if num >= epochMilliFloor {
		ms := int64(math.Round(num))
		return time.UnixMilli(ms).UTC(), nil
	}
	sec, frac := math.Modf(num)
	return time.Unix(int64(sec), int64(math.Round(frac*1e9))).UTC(), nil
}

func isNullOrEmptyJSON(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) == 0 || string(trimmed) == jsonNullLiteral
}
