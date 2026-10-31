package cmd

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/codex"
	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/state"
)

const (
	// usageFile is the state's side file holding the latest subscription
	// usage reading, shared by watch, capacity, snapshot and agents start/resume.
	usageFile = "usage.json"
	// usageHighFile records the busy workers already told to park in the
	// current USAGE HIGH episode, so watch restarts never tell a worker twice.
	usageHighFile = "usage-high.json"
	// verbUsageHigh logs each busy worker told to park on USAGE HIGH.
	verbUsageHigh = "usage.high"
	// episodeGap separates consecutive 5-hour windows if watch slept across
	// a reset boundary while tolerating small resetsAt jitter within one window.
	episodeGap = 15 * time.Minute
	// sevenDaySpan is the duration of a 7-day subscription usage window.
	sevenDaySpan = 7 * 24 * time.Hour
)

func usageProviderFile(name string) string {
	return "usage-" + unsafeKey.ReplaceAllString(name, "_") + ".json"
}

func usageHighProviderFile(name string) string {
	return "usage-high-" + unsafeKey.ReplaceAllString(name, "_") + ".json"
}

func usageHighBucketFile(name, bucket string) string {
	if bucket == "" {
		return usageHighProviderFile(name)
	}
	return "usage-high-" + unsafeKey.ReplaceAllString(name, "_") + "-" + unsafeKey.ReplaceAllString(bucket, "_") + ".json"
}

// usageWindow is one subscription usage window (5-hour or 7-day).
type usageWindow struct {
	UsedPercent float64   `json:"usedPercent"`
	ResetsAt    time.Time `json:"resetsAt,omitzero"`
	Pace        *float64  `json:"pace,omitempty"`
}

// usageBucket is one named bucket (such as "Gemini Models" or
// "Claude and GPT models") in a multi-bucket usage reading.
type usageBucket struct {
	FiveHour *usageWindow `json:"fiveHour,omitempty"`
	SevenDay *usageWindow `json:"sevenDay,omitempty"`
}

// usageReading is the timestamped output of a usage command stored in
// usageFile or usageProviderFile.
type usageReading struct {
	At            time.Time              `json:"at"`
	SourceAt      time.Time              `json:"sourceAt,omitzero"`
	AgeMinutes    *float64               `json:"ageMinutes,omitempty"`
	Stale         bool                   `json:"stale,omitempty"`
	NoPlanWindows bool                   `json:"noPlanWindows,omitempty"`
	SingleWindow  bool                   `json:"singleWindow,omitempty"`
	FiveHour      *usageWindow           `json:"fiveHour,omitempty"`
	SevenDay      *usageWindow           `json:"sevenDay,omitempty"`
	Buckets       map[string]usageBucket `json:"buckets,omitempty"`
	BucketOrder   []string               `json:"bucketOrder,omitempty"`
}

// usageView is how capacity, watch and snapshot present subscription usage and
// the target derived from it.
type usageView struct {
	At          time.Time    `json:"at,omitzero"`
	FiveHour    *usageWindow `json:"fiveHour,omitempty"`
	SevenDay    *usageWindow `json:"sevenDay,omitempty"`
	Floor       int          `json:"floor"`
	Ceiling     int          `json:"ceiling"`
	High        bool         `json:"high,omitempty"`
	HighPercent float64      `json:"highPercent,omitempty"`
	Error       string       `json:"error,omitempty"`
}

// usageHighLedger tracks the current USAGE HIGH episode and who was told.
type usageHighLedger struct {
	Episode  string            `json:"episode,omitempty"`
	ResetsAt time.Time         `json:"resetsAt,omitzero"`
	Told     map[string]string `json:"told,omitempty"`
}

func runUsageCommand(ctx context.Context, command []string, timeout time.Duration) (usageReading, error) {
	return runUsageCommandInDir(ctx, command, timeout, "")
}

func runUsageCommandInDir(ctx context.Context, command []string, timeout time.Duration, dir string) (usageReading, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	c := exec.CommandContext(ctx, command[0], command[1:]...) //nolint:gosec // the configured usage command
	c.Dir = dir
	if slices.Equal(command, config.DefaultClaudeUsageCommand) {
		// The local probe is not a turn of the worker that requested it.
		c.Env = make([]string, 0)
		for _, entry := range os.Environ() {
			key, _, _ := strings.Cut(entry, "=")
			if key != claudeCodeEnv && !strings.HasPrefix(key, "CLAUDE_CODE_") {
				c.Env = append(c.Env, entry)
			}
		}
	}
	var stdout, stderr bytes.Buffer
	c.Stdout, c.Stderr = &stdout, &stderr
	if err := c.Run(); err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return usageReading{}, fmt.Errorf("%s timed out after %s", filepath.Base(command[0]), timeout)
		}
		if msg := oneLine(stderr.String()); msg != "" {
			return usageReading{}, fmt.Errorf("%s: %s", err, truncate(msg, 120))
		}
		return usageReading{}, err
	}
	if slices.Equal(command, config.DefaultClaudeUsageCommand) {
		return parseClaudeUsage(stdout.Bytes(), time.Now())
	}
	return parseUsageOutput(stdout.Bytes())
}

func parseUsageOutput(raw []byte) (usageReading, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return usageReading{}, errors.New("empty usage output")
	}
	if trimmed[0] == '{' {
		return parseUsageJSON(trimmed)
	}
	return parseAgyUsage(trimmed)
}

func parseUsageJSON(raw []byte) (usageReading, error) {
	type rawWindow struct {
		UsedPercent *float64 `json:"usedPercent"`
		ResetsAt    string   `json:"resetsAt"`
		Pace        *float64 `json:"pace"`
	}
	type rawBucket struct {
		FiveHour *rawWindow `json:"fiveHour"`
		SevenDay *rawWindow `json:"sevenDay"`
	}
	var doc struct {
		At         string               `json:"at"`
		AgeMinutes *float64             `json:"ageMinutes"`
		Stale      bool                 `json:"stale"`
		Pace       *float64             `json:"pace"`
		FiveHour   *rawWindow           `json:"fiveHour"`
		SevenDay   *rawWindow           `json:"sevenDay"`
		Buckets    map[string]rawBucket `json:"buckets"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return usageReading{}, fmt.Errorf("invalid JSON: %w", err)
	}
	conv := func(name string, w *rawWindow) (*usageWindow, error) {
		if w == nil {
			return nil, nil
		}
		if w.UsedPercent == nil {
			return nil, fmt.Errorf("%s.usedPercent is missing", name)
		}
		v := *w.UsedPercent
		if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
			return nil, fmt.Errorf("%s.usedPercent %g is invalid", name, v)
		}
		var reset time.Time
		if s := strings.TrimSpace(w.ResetsAt); s != "" {
			var err error
			reset, err = time.Parse(time.RFC3339Nano, s)
			if err != nil {
				return nil, fmt.Errorf("%s.resetsAt: %w", name, err)
			}
			reset = reset.UTC()
		}
		out := &usageWindow{UsedPercent: v, ResetsAt: reset}
		if w.Pace != nil && !math.IsNaN(*w.Pace) && !math.IsInf(*w.Pace, 0) {
			p := *w.Pace
			out.Pace = &p
		}
		return out, nil
	}
	var r usageReading
	r.Stale = doc.Stale
	if doc.AgeMinutes != nil && !math.IsNaN(*doc.AgeMinutes) && !math.IsInf(*doc.AgeMinutes, 0) {
		age := *doc.AgeMinutes
		r.AgeMinutes = &age
	}
	if s := strings.TrimSpace(doc.At); s != "" {
		t, err := time.Parse(time.RFC3339Nano, s)
		if err != nil {
			return usageReading{}, fmt.Errorf("at: %w", err)
		}
		r.SourceAt = t.UTC()
	}
	var err error
	if r.FiveHour, err = conv("fiveHour", doc.FiveHour); err != nil {
		return usageReading{}, err
	}
	if r.SevenDay, err = conv("sevenDay", doc.SevenDay); err != nil {
		return usageReading{}, err
	}
	if r.SevenDay != nil && r.SevenDay.Pace == nil && doc.Pace != nil && !math.IsNaN(*doc.Pace) && !math.IsInf(*doc.Pace, 0) {
		p := *doc.Pace
		r.SevenDay.Pace = &p
	}
	if len(doc.Buckets) > 0 {
		r.Buckets = make(map[string]usageBucket, len(doc.Buckets))
		for _, bName := range slices.Sorted(maps.Keys(doc.Buckets)) {
			rb := doc.Buckets[bName]
			fh, err := conv(bName+".fiveHour", rb.FiveHour)
			if err != nil {
				return usageReading{}, err
			}
			sd, err := conv(bName+".sevenDay", rb.SevenDay)
			if err != nil {
				return usageReading{}, err
			}
			if fh == nil && sd == nil {
				return usageReading{}, fmt.Errorf("bucket %q has neither fiveHour nor sevenDay", bName)
			}
			r.Buckets[bName] = usageBucket{FiveHour: fh, SevenDay: sd}
			r.BucketOrder = append(r.BucketOrder, bName)
		}
	}
	if r.FiveHour == nil && r.SevenDay == nil && len(r.Buckets) == 0 {
		if cr, ok, cerr := parseCodexRateLimitsJSON(raw); ok {
			return cr, cerr
		}
		return usageReading{}, errors.New("usage output has neither fiveHour nor sevenDay")
	}
	return r, nil
}

func parseCodexRateLimitsJSON(raw []byte) (usageReading, bool, error) {
	var probe struct {
		Type                string          `json:"type"`
		RateLimits          json.RawMessage `json:"rate_limits"`
		Primary             json.RawMessage `json:"primary"`
		Secondary           json.RawMessage `json:"secondary"`
		RateLimitReached    *string         `json:"rate_limit_reached_type"`
		SpendControlReached *bool           `json:"spend_control_reached"`
		Payload             *struct {
			Type       string          `json:"type"`
			RateLimits json.RawMessage `json:"rate_limits"`
		} `json:"payload"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return usageReading{}, false, nil
	}
	isCodex := len(probe.RateLimits) > 0 || len(probe.Primary) > 0 || len(probe.Secondary) > 0 ||
		probe.RateLimitReached != nil || probe.SpendControlReached != nil ||
		probe.Type == "token_count" ||
		(probe.Payload != nil && (probe.Payload.Type == "token_count" || len(probe.Payload.RateLimits) > 0))
	if !isCodex {
		return usageReading{}, false, nil
	}
	cr, err := codex.ParseRateLimits(raw, time.Time{})
	if err != nil {
		return usageReading{}, true, err
	}
	return codexUsageToReading(cr), true, nil
}

// parseAgyUsage parses the tab-separated lines printed by `agy -p /usage`:
//
//	Gemini Models\tWeekly Limit Remaining\t78%\t2026-11-03T09:23:51Z
//	Gemini Models\tFive Hour Limit Remaining\t94%\t2026-10-28T05:23:51Z
func parseAgyUsage(raw []byte) (usageReading, error) {
	r := usageReading{Buckets: map[string]usageBucket{}}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Split(line, "\t")
		if len(fields) < 3 {
			continue
		}
		bucket := strings.TrimSpace(fields[0])
		label := strings.ToLower(strings.TrimSpace(fields[1]))
		pctStr := strings.TrimSuffix(strings.TrimSpace(fields[2]), "%")
		if bucket == "" || pctStr == "" {
			continue
		}
		val, err := strconv.ParseFloat(pctStr, 64)
		if err != nil || math.IsNaN(val) || math.IsInf(val, 0) || val < 0 || val > 100 {
			return usageReading{}, fmt.Errorf("invalid percentage %q in %q", fields[2], line)
		}
		var used float64
		if strings.Contains(label, "remaining") {
			used = math.Round(max(0, 100-val)*10) / 10
		} else {
			used = math.Round(val*10) / 10
		}
		var reset time.Time
		if len(fields) >= 4 {
			if s := strings.TrimSpace(fields[3]); s != "" {
				reset, err = time.Parse(time.RFC3339Nano, s)
				if err != nil {
					return usageReading{}, fmt.Errorf("invalid reset time %q in %q: %w", s, line, err)
				}
				reset = reset.UTC()
			}
		}
		w := &usageWindow{UsedPercent: used, ResetsAt: reset}
		b, exists := r.Buckets[bucket]
		switch {
		case strings.Contains(label, "five hour") || strings.Contains(label, "5h"):
			b.FiveHour = w
		case strings.Contains(label, "weekly") || strings.Contains(label, "seven day") || strings.Contains(label, "7d"):
			b.SevenDay = w
		default:
			continue
		}
		if !exists {
			r.BucketOrder = append(r.BucketOrder, bucket)
		}
		r.Buckets[bucket] = b
	}
	if len(r.Buckets) == 0 {
		return usageReading{}, fmt.Errorf("invalid usage output: %s", truncate(oneLine(string(raw)), 120))
	}
	return r, nil
}

func validateReadingAt(r usageReading, u config.Usage, now time.Time) error {
	maxAge := u.EffectiveStaleAfter()
	if r.Stale {
		return errors.New("usage reading is stale")
	}
	if r.AgeMinutes != nil {
		age := time.Duration(*r.AgeMinutes * float64(time.Minute))
		if age > maxAge {
			return fmt.Errorf("usage reading is stale (%s old, max %s)", dur(age), dur(maxAge))
		}
	}
	if !r.SourceAt.IsZero() && !now.IsZero() {
		if r.SourceAt.After(now.Add(time.Minute)) {
			return fmt.Errorf("usage reading timestamp %s is in the future", r.SourceAt.Format(time.RFC3339))
		}
		if now.Sub(r.SourceAt) > maxAge {
			return fmt.Errorf("usage reading is stale (%s old, max %s)", dur(now.Sub(r.SourceAt)), dur(maxAge))
		}
	}
	if !r.At.IsZero() && !now.IsZero() {
		if r.At.After(now.Add(time.Minute)) {
			return fmt.Errorf("usage reading timestamp %s is in the future", r.At.Format(time.RFC3339))
		}
		if now.Sub(r.At) > maxAge {
			return fmt.Errorf("usage reading is stale (%s old, max %s)", dur(now.Sub(r.At)), dur(maxAge))
		}
	}
	if r.NoPlanWindows {
		return nil
	}
	if len(r.Buckets) > 0 && r.FiveHour == nil && r.SevenDay == nil {
		for _, bName := range bucketNames(r) {
			b := r.Buckets[bName]
			switch {
			case b.FiveHour == nil && b.SevenDay == nil:
				return fmt.Errorf("bucket %q has neither fiveHour nor sevenDay", bName)
			case len(u.FiveHour) > 0 && b.FiveHour == nil:
				return fmt.Errorf("bucket %q omits fiveHour (fiveHour is configured)", bName)
			case u.HighPercentSet() && b.FiveHour == nil:
				return fmt.Errorf("bucket %q omits fiveHour (highPercent is configured)", bName)
			case len(u.SevenDay) > 0 && b.SevenDay == nil:
				return fmt.Errorf("bucket %q omits sevenDay (sevenDay is configured)", bName)
			}
		}
		return nil
	}
	switch {
	case r.FiveHour == nil && r.SevenDay == nil:
		return errors.New("usage output has neither fiveHour nor sevenDay")
	case len(u.FiveHour) > 0 && r.FiveHour == nil && !r.SingleWindow:
		return errors.New("usage output omits fiveHour (capacity.usage.fiveHour is configured)")
	case u.HighPercentSet() && r.FiveHour == nil && !r.SingleWindow:
		return errors.New("usage output omits fiveHour (capacity.usage.highPercent is configured)")
	case len(u.SevenDay) > 0 && r.SevenDay == nil:
		return errors.New("usage output omits sevenDay (capacity.usage.sevenDay is configured)")
	}
	return nil
}

func bucketNames(r usageReading) []string {
	if len(r.Buckets) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(r.Buckets))
	var out []string
	for _, b := range r.BucketOrder {
		if _, ok := r.Buckets[b]; ok && !seen[b] {
			seen[b] = true
			out = append(out, b)
		}
	}
	for _, b := range slices.Sorted(maps.Keys(r.Buckets)) {
		if !seen[b] {
			seen[b] = true
			out = append(out, b)
		}
	}
	return out
}

// usageFresh reports whether r is fresh at now: within u.EffectiveEvery(), not
// stale, and no window crossed its ResetsAt since r.At.
func usageFresh(r usageReading, u config.Usage, now time.Time) bool {
	if r.At.IsZero() || now.Sub(r.At) > u.EffectiveEvery() || r.At.After(now.Add(time.Minute)) {
		return false
	}
	if validateReadingAt(r, u, now) != nil {
		return false
	}
	crossed := func(w *usageWindow) bool {
		return w != nil && !w.ResetsAt.IsZero() && r.At.Before(w.ResetsAt) && !now.Before(w.ResetsAt)
	}
	if crossed(r.FiveHour) || crossed(r.SevenDay) {
		return false
	}
	for _, b := range r.Buckets {
		if crossed(b.FiveHour) || crossed(b.SevenDay) {
			return false
		}
	}
	return true
}

// effectiveWindow returns w at now: a window whose ResetsAt has passed has
// reset to 0% in the new window.
func effectiveWindow(w *usageWindow, now time.Time) *usageWindow {
	if w == nil {
		return nil
	}
	if !w.ResetsAt.IsZero() && !now.Before(w.ResetsAt) {
		return &usageWindow{UsedPercent: 0}
	}
	return &usageWindow{UsedPercent: w.UsedPercent, ResetsAt: w.ResetsAt, Pace: w.Pace}
}

// effectiveSevenDayWindow returns the 7-day window at now with its elapsed
// weekly pace share in percent attached.
func effectiveSevenDayWindow(w *usageWindow, now time.Time) *usageWindow {
	ew := effectiveWindow(w, now)
	if ew == nil {
		return nil
	}
	ew.Pace = windowPace(ew, now)
	return ew
}

// windowPace returns the 7-day window's elapsed share in percent [0, 100] at now,
// or w.Pace when explicitly supplied in JSON, or nil when resetsAt is unset.
func windowPace(w *usageWindow, now time.Time) *float64 {
	if w == nil {
		return nil
	}
	if w.Pace != nil {
		v := math.Round(max(0, min(100, *w.Pace))*10) / 10
		return &v
	}
	if w.ResetsAt.IsZero() || !now.Before(w.ResetsAt) {
		return nil
	}
	rem := w.ResetsAt.Sub(now)
	elapsed := sevenDaySpan - rem
	v := max(0, min(100, float64(elapsed)/float64(sevenDaySpan)*100))
	v = math.Round(v*10) / 10
	return &v
}

func sdPace(w *usageWindow) *float64 {
	if w == nil {
		return nil
	}
	return w.Pace
}

func pctPtr(w *usageWindow) *float64 {
	if w == nil {
		return nil
	}
	v := w.UsedPercent
	return &v
}

func deriveUsageView(r usageReading, k config.Capacity, now time.Time) (config.Capacity, *usageView) {
	fh := effectiveWindow(r.FiveHour, now)
	sd := effectiveSevenDayWindow(r.SevenDay, now)
	derived, high := k.DeriveWithPace(pctPtr(fh), pctPtr(sd), sdPace(sd))
	return derived, &usageView{
		At:          r.At,
		FiveHour:    fh,
		SevenDay:    sd,
		Floor:       derived.Floor,
		Ceiling:     derived.Ceiling,
		High:        high,
		HighPercent: k.Usage.EffectiveHighPercent(),
	}
}

func errorUsageView(k config.Capacity, err error) (config.Capacity, *usageView) {
	derived := k
	derived.Floor, derived.Ceiling = 0, 0
	return derived, &usageView{
		Floor:       0,
		Ceiling:     0,
		HighPercent: k.Usage.EffectiveHighPercent(),
		Error:       err.Error(),
	}
}

func (a *app) execUsage(ctx context.Context, u config.Usage) (usageReading, error) {
	if a.readUsage != nil {
		return a.readUsage(ctx, u.Command, u.EffectiveTimeout())
	}
	if slices.Equal(u.Command, config.DefaultCodexUsageCommand) {
		return a.readCodexUsage()
	}
	if slices.Equal(u.Command, config.DefaultClaudeUsageCommand) {
		dir := filepath.Join(a.cfg.StateDir, "usage", claudeComm)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return usageReading{}, err
		}
		return runUsageCommandInDir(ctx, u.Command, u.EffectiveTimeout(), dir)
	}
	return runUsageCommand(ctx, u.Command, u.EffectiveTimeout())
}

// serveModeFile is the state's side file holding a self-hosted provider's
// latest serving-mode reading.
func serveModeFile(name string) string {
	return "serve-mode-" + unsafeKey.ReplaceAllString(name, "_") + ".json"
}

// serveMode is the provider's serving mode now: the cached reading while it is
// fresh, else one read by running serveModeCommand. A failed or stale read is
// returned as an error: the provider keeps its configured ceiling, and the
// line says the mode is unknown rather than naming one it did not read.
func (a *app) serveMode(ctx context.Context, name string, p config.Provider) config.ServeModeReading {
	if !p.HasServeMode() {
		return config.ServeModeReading{}
	}
	now := a.now
	if now.IsZero() {
		now = time.Now()
	}
	fresh := func(r config.ServeModeReading) bool {
		return !r.At.IsZero() && !r.At.After(now.Add(time.Minute)) && now.Sub(r.At) <= p.EffectiveServeModeStale()
	}
	var cached config.ServeModeReading
	var haveCached bool
	if a.store != nil {
		if found, err := a.store.ReadFile(serveModeFile(name), &cached); err == nil && found {
			if fresh(cached) {
				return cached
			}
			haveCached = true
		}
	}
	r, err := a.runServeMode(ctx, p)
	if err != nil {
		// In the agent sandbox, where the command cannot run, a reading the
		// host's watch wrote within twice the freshness window stands.
		if haveCached && inSandbox() && now.Sub(cached.At) <= 2*p.EffectiveServeModeStale() {
			return cached
		}
		return config.ServeModeReading{Err: err.Error()}
	}
	r.At = now.UTC()
	if a.store != nil {
		_ = a.store.WriteFile(serveModeFile(name), r)
	}
	return r
}

// runServeMode runs the provider's serving-mode command and reads the mode off
// its stdout: the first word it prints.
func (a *app) runServeMode(ctx context.Context, p config.Provider) (config.ServeModeReading, error) {
	if a.readServeMode != nil {
		return a.readServeMode(ctx, p.ServeModeCommand, p.EffectiveServeModeStale())
	}
	ctx, cancel := context.WithTimeout(ctx, config.DefaultUsageTimeout)
	defer cancel()
	c := exec.CommandContext(ctx, p.ServeModeCommand[0], p.ServeModeCommand[1:]...) //nolint:gosec // the configured serving-mode command
	var stdout, stderr bytes.Buffer
	c.Stdout, c.Stderr = &stdout, &stderr
	if len(a.serveModeEnv) > 0 {
		c.Env = append(os.Environ(), a.serveModeEnv...)
	}
	if err := c.Run(); err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return config.ServeModeReading{}, fmt.Errorf("serving mode command timed out: %s", strings.Join(p.ServeModeCommand, " "))
		}
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return config.ServeModeReading{}, fmt.Errorf("serving mode command: %s", msg)
	}
	fields := strings.Fields(stdout.String())
	if len(fields) == 0 {
		return config.ServeModeReading{}, errors.New("serving mode command printed no mode")
	}
	return config.ServeModeReading{Mode: fields[0]}, nil
}

func (a *app) readCodexUsage() (usageReading, error) {
	var stateDir, sessionsDir string
	if a.cfg != nil {
		stateDir = a.cfg.StateDir
		sessionsDir = a.cfg.Codex.SessionsDir
	}
	cr, err := codex.ReadUsage(stateDir, sessionsDir)
	if err != nil {
		return usageReading{}, err
	}
	return codexUsageToReading(cr), nil
}

func (a *app) useCodexRolloutUsage(u config.Usage, cached usageReading) bool {
	if a.readUsage != nil || !slices.Equal(u.Command, config.DefaultCodexUsageCommand) {
		return false
	}
	var stateDir, sessionsDir string
	if a.cfg != nil {
		stateDir = a.cfg.StateDir
		sessionsDir = a.cfg.Codex.SessionsDir
	}
	return codex.HasRollouts(stateDir, sessionsDir) || !cached.SourceAt.IsZero()
}

func codexUsageToReading(cr codex.UsageReading) usageReading {
	r := usageReading{
		SourceAt:     cr.At,
		SingleWindow: cr.FiveHour == nil && cr.SevenDay != nil,
	}
	if cr.FiveHour != nil {
		r.FiveHour = &usageWindow{
			UsedPercent: cr.FiveHour.UsedPercent,
			ResetsAt:    cr.FiveHour.ResetsAt,
		}
	}
	if cr.SevenDay != nil {
		r.SevenDay = &usageWindow{
			UsedPercent: cr.SevenDay.UsedPercent,
			ResetsAt:    cr.SevenDay.ResetsAt,
		}
	}
	return r
}

// cachedUsageCapacity derives Capacity from a fresh usage.json (or cached
// provider usage files) if one exists, without running any subprocess.
func (a *app) cachedUsageCapacity(k config.Capacity) (config.Capacity, *usageView) {
	if len(k.Providers) > 0 {
		return a.cachedProvidersCapacityAt(k, a.now, nil)
	}
	if !k.Usage.Enabled() || a.store == nil {
		return k, nil
	}
	var r usageReading
	if found, err := a.store.ReadFile(usageFile, &r); err == nil && found &&
		usageFresh(r, k.Usage, a.now) && validateReadingAt(r, k.Usage, a.now) == nil {
		return deriveUsageView(r, k, a.now)
	}
	return k, nil
}

func (a *app) cachedProvidersCapacityAt(k config.Capacity, now time.Time, providerErrs map[string]string) (config.Capacity, *usageView) {
	derived := k
	sumFloor, sumCeiling := 0, 0
	subFloor, subCeiling := 0, 0
	// subDerivedCeiling is the subscription providers' own ceiling as the
	// cached readings derive it; selfHostedCeiling the self-hosted providers'
	// own, which capacity.ceiling never caps.
	subDerivedCeiling, selfHostedCeiling := 0, 0
	anySubscription := false
	var errs []string
	for _, name := range k.ProviderNames() {
		p := k.Providers[name]
		if p.SelfHosted() {
			// The self-hosted provider's ceiling is its own; capacity.ceiling
			// holds back only the subscriptions. The cached serving-mode
			// reading, while fresh, sets it.
			if p.HasServeMode() && a.store != nil {
				var r config.ServeModeReading
				if found, err := a.store.ReadFile(serveModeFile(name), &r); err == nil && found &&
					r.At.After(now.Add(-2*p.EffectiveServeModeStale())) && !r.At.After(now) {
					p = p.WithServeMode(r)
				}
			}
			sumFloor += p.Floor
			sumCeiling += p.Ceiling
			selfHostedCeiling += p.Ceiling
			continue
		}
		if !p.Enabled() {
			sumFloor += p.Floor
			sumCeiling += p.Ceiling
			anySubscription = true
			subFloor += p.Floor
			subCeiling += p.Ceiling
			subDerivedCeiling += p.Ceiling
			continue
		}
		if uerr := providerErrs[name]; uerr != "" {
			errs = append(errs, name+": "+uerr)
			continue
		}
		if a.store == nil {
			errs = append(errs, name+": usage not read")
			continue
		}
		var r usageReading
		found, err := a.store.ReadFile(usageProviderFile(name), &r)
		if err != nil || !found {
			errs = append(errs, name+": usage not read")
			continue
		}
		if verr := validateReadingAt(r, p.Usage, now); verr != nil {
			errs = append(errs, name+": "+verr.Error())
			continue
		}
		if !usageFresh(r, p.Usage, now) {
			errs = append(errs, name+": usage reading is stale")
			continue
		}
		pv := buildSingleProviderView(name, p, providerAgents{}, providerReadingResult{reading: r, ok: true}, now)
		sumFloor += pv.Floor
		sumCeiling += pv.Ceiling
		anySubscription = true
		subFloor += pv.Floor
		subCeiling += pv.Ceiling
		subDerivedCeiling += pv.Ceiling
	}
	derived.Ceiling = selfHostedCeiling + subDerivedCeiling
	if anySubscription {
		// capacity.ceiling caps the subscription providers, not the
		// self-hosted ones: a desk that lowers it to 0 suppresses CAPACITY
		// LOW and CAPACITY FULL (k.Ceiling stays 0 for the watch) and holds
		// the subscription side to 0, while the self-hosted ceiling stands
		// beside it.
		derived.Ceiling = min(k.Ceiling, subDerivedCeiling) + selfHostedCeiling
	}
	derived.Floor = min(k.Floor, min(sumFloor, derived.Ceiling))
	var u *usageView
	if len(errs) > 0 {
		u = &usageView{Floor: derived.Floor, Ceiling: derived.Ceiling, Error: strings.Join(errs, "; ")}
	}
	return derived, u
}

// resolveUsage returns the derived Capacity and usageView for k, reusing a
// fresh usage.json or running capacity.usage.command and saving usage.json.
// In the agent sandbox (where home credentials and external domains are
// closed), a recent usage.json written by the host's watch within 2 × every
// (before resetsAt) is kept when the command cannot run inside the sandbox.
func (a *app) resolveUsage(ctx context.Context, k config.Capacity) (config.Capacity, *usageView) {
	if !k.Usage.Enabled() {
		return k, nil
	}
	r, err := a.resolveReading(ctx, usageFile, k.Usage)
	if err != nil {
		return errorUsageView(k, err)
	}
	return deriveUsageView(r, k, a.now)
}

func (a *app) resolveReading(ctx context.Context, file string, u config.Usage) (usageReading, error) {
	now := a.now
	if now.IsZero() {
		now = time.Now()
	}
	var cached usageReading
	var haveCached bool
	if a.store != nil {
		if found, err := a.store.ReadFile(file, &cached); err == nil && found && validateReadingAt(cached, u, now) == nil {
			if !a.useCodexRolloutUsage(u, cached) && usageFresh(cached, u, now) {
				return cached, nil
			}
			haveCached = true
		}
	}
	r, err := a.execUsage(ctx, u)
	if err == nil {
		r.At = now.UTC()
		err = validateReadingAt(r, u, now)
	}
	if err != nil {
		if haveCached && inSandbox() {
			grace := u
			grace.Every = config.Duration{Duration: 2 * u.EffectiveEvery()}
			if usageFresh(cached, grace, now) {
				return cached, nil
			}
		}
		return usageReading{}, err
	}
	if a.store != nil {
		_ = a.store.WriteFile(file, r)
	}
	return r, nil
}

// usageHighBlockForAgent returns the USAGE HIGH block reason for ag if USAGE
// HIGH holds now on ag's provider/bucket (or globally when capacity.providers
// is not configured), "" otherwise.
func (a *app) usageHighBlockForAgent(ctx context.Context, st *state.State, sessions []*claude.Session, ag *state.Agent) string {
	k := a.liveCapacity()
	if len(k.Providers) > 0 {
		return a.providersUsageHighBlockForAgent(ctx, k, st, sessions, ag)
	}
	if !k.Usage.Enabled() {
		return ""
	}
	_, u := a.resolveUsage(ctx, k)
	if u == nil {
		return ""
	}
	if u.High {
		return u.highBlock(a.now)
	}
	if u.Error != "" && a.store != nil {
		var l usageHighLedger
		if found, err := a.store.ReadFile(usageHighFile, &l); err == nil && found &&
			l.Episode != "" && !l.ResetsAt.IsZero() && a.now.Before(l.ResetsAt) {
			return fmt.Sprintf("usage high until %s (in %s); usage unreadable (%s)",
				clock(a.now.Local(), l.ResetsAt), dur(l.ResetsAt.Sub(a.now)), truncate(u.Error, 80))
		}
	}
	return ""
}

func (a *app) providersUsageHighBlockForAgent(ctx context.Context, k config.Capacity, st *state.State, sessions []*claude.Session, ag *state.Agent) string {
	if ag != nil {
		if st == nil && a.store != nil {
			st, _ = a.store.Read()
		}
		if st == nil {
			st = &state.State{}
		}
		if sessions == nil {
			sessions, _, _ = a.sessions()
		}
		harness, model := agentHarnessModel(st, sessions, *ag)
		prov, bucket := resolveAgentProviderBucket(k, harness, model)
		return a.singleProviderUsageHighBlock(ctx, prov, bucket, k.Providers[prov])
	}
	var highReasons []string
	enabledCount := 0
	for _, name := range k.ProviderNames() {
		p := k.Providers[name]
		if !p.Enabled() {
			return ""
		}
		enabledCount++
		reason := a.singleProviderUsageHighBlock(ctx, name, "", p)
		if reason == "" {
			return ""
		}
		highReasons = append(highReasons, reason)
	}
	if enabledCount == 0 || len(highReasons) == 0 {
		return ""
	}
	if len(highReasons) == 1 {
		_, rest, ok := strings.Cut(highReasons[0], ": ")
		if ok {
			return rest
		}
		return highReasons[0]
	}
	return strings.Join(highReasons, "; ")
}

func (a *app) singleProviderUsageHighBlock(ctx context.Context, prov, bucket string, p config.Provider) string {
	if !p.Enabled() {
		return ""
	}
	r, err := a.resolveReading(ctx, usageProviderFile(prov), p.Usage)
	if err != nil {
		if a.store != nil {
			files := []string{usageHighProviderFile(prov)}
			if bucket != "" {
				files = []string{usageHighBucketFile(prov, bucket), usageHighProviderFile(prov)}
			}
			for _, lf := range files {
				var l usageHighLedger
				if found, rerr := a.store.ReadFile(lf, &l); rerr == nil && found &&
					l.Episode != "" && !l.ResetsAt.IsZero() && a.now.Before(l.ResetsAt) {
					label := prov
					if bucket != "" {
						label = prov + " (" + bucket + ")"
					}
					return fmt.Sprintf("provider %s: usage high until %s (in %s); usage unreadable (%s)",
						label, clock(a.now.Local(), l.ResetsAt), dur(l.ResetsAt.Sub(a.now)), truncate(err.Error(), 80))
				}
			}
		}
		return ""
	}
	pv := buildSingleProviderView(prov, p, providerAgents{}, providerReadingResult{reading: r, ok: true}, a.now)
	if len(pv.Buckets) > 0 && bucket != "" && !pv.supplementalBuckets {
		if bv := pv.findBucket(bucket); bv != nil {
			if bv.High {
				return fmt.Sprintf("provider %s (%s): %s", prov, bv.Name, highBlockText(bv.FiveHour, pv.HighPercent, a.now))
			}
			return ""
		}
	}
	if !pv.High {
		return ""
	}
	return fmt.Sprintf("provider %s: %s", prov, highBlockText(pv.FiveHour, pv.HighPercent, a.now))
}

func windowFigure(label string, w *usageWindow, now time.Time) string {
	s := fmt.Sprintf("%s %s", label, pct(w.UsedPercent))
	if !w.ResetsAt.IsZero() && now.Before(w.ResetsAt) {
		if w.Pace != nil {
			s += fmt.Sprintf(" (resets %s, in %s, pace %s)", clock(now.Local(), w.ResetsAt), dur(w.ResetsAt.Sub(now)), pct(*w.Pace))
		} else {
			s += fmt.Sprintf(" (resets %s, in %s)", clock(now.Local(), w.ResetsAt), dur(w.ResetsAt.Sub(now)))
		}
	} else if w.Pace != nil {
		s += fmt.Sprintf(" (pace %s)", pct(*w.Pace))
	}
	return s
}

func windowsPairLine(fh, sd *usageWindow, now time.Time) string {
	var parts []string
	if fh != nil {
		parts = append(parts, windowFigure("5h", fh, now))
	}
	if sd != nil {
		parts = append(parts, windowFigure("7d", sd, now))
	}
	return strings.Join(parts, ", ")
}

func (u *usageView) windowsLine(now time.Time) string {
	return windowsPairLine(u.FiveHour, u.SevenDay, now)
}

func (u *usageView) capacitySuffix(now time.Time) string {
	if u.Error != "" {
		return "usage unknown (" + truncate(u.Error, 120) + ")"
	}
	return "usage " + u.windowsLine(now)
}

func (u *usageView) watchLine(now time.Time) string {
	s := fmt.Sprintf("%s; target floor %d, ceiling %d", u.windowsLine(now), u.Floor, u.Ceiling)
	if u.High {
		s += " (USAGE HIGH)"
	}
	return s
}

func (u *usageView) snapshotLine(now time.Time) string {
	if u.Error != "" {
		return "usage: unknown (" + truncate(u.Error, 120) + ")"
	}
	return "usage: " + u.watchLine(now)
}

func highFigureText(fh *usageWindow, highPct float64, now time.Time) string {
	used := highPct
	var reset time.Time
	if fh != nil {
		used, reset = fh.UsedPercent, fh.ResetsAt
	}
	s := fmt.Sprintf("5h %s at or past %s", pct(used), pct(highPct))
	if !reset.IsZero() && now.Before(reset) {
		s += fmt.Sprintf(" (resets %s, in %s)", clock(now.Local(), reset), dur(reset.Sub(now)))
	}
	return s
}

func highBlockText(fh *usageWindow, highPct float64, now time.Time) string {
	return "usage high: " + highFigureText(fh, highPct, now)
}

func (u *usageView) highFigure(now time.Time) string {
	return highFigureText(u.FiveHour, u.HighPercent, now)
}

func (u *usageView) highBlock(now time.Time) string {
	return "usage high: " + u.highFigure(now)
}

func (u *usageView) highWatchLine(now time.Time) string {
	return "USAGE HIGH: " + u.highFigure(now) + "; new starts and resumes wait until reset, telling busy workers to finish their step and park"
}

func (w *watcher) usageNeedsRefresh(k config.Capacity, now time.Time) bool {
	if len(k.Providers) > 0 {
		for _, name := range k.ProviderNames() {
			p := k.Providers[name]
			if p.Enabled() && w.providerUsageNeedsRefresh(name, p.Usage, now) {
				return true
			}
		}
		return false
	}
	var cached usageReading
	if found, err := w.store.ReadFile(usageFile, &cached); err == nil && found &&
		usageFresh(cached, k.Usage, now) && validateReadingAt(cached, k.Usage, now) == nil {
		return false
	}
	w.mu.Lock()
	last := w.lastUsage
	w.mu.Unlock()
	retry := min(k.Usage.EffectiveEvery(), max(w.cfg.Watch.Interval.Duration, time.Second))
	return last.IsZero() || now.Sub(last) >= retry
}

func (w *watcher) providerUsageNeedsRefresh(name string, u config.Usage, now time.Time) bool {
	var cached usageReading
	if found, err := w.store.ReadFile(usageProviderFile(name), &cached); err == nil && found &&
		!w.useCodexRolloutUsage(u, cached) &&
		usageFresh(cached, u, now) && validateReadingAt(cached, u, now) == nil {
		return false
	}
	w.mu.Lock()
	last := w.providerLastUsage[name]
	w.mu.Unlock()
	retry := min(u.EffectiveEvery(), max(w.cfg.Watch.Interval.Duration, time.Second))
	return last.IsZero() || now.Sub(last) >= retry
}

func (w *watcher) refreshUsage(ctx context.Context, k config.Capacity, now time.Time) {
	if now.IsZero() {
		now = w.clk().Now()
	}
	if len(k.Providers) > 0 {
		for _, name := range k.ProviderNames() {
			p := k.Providers[name]
			if !p.Enabled() || !w.providerUsageNeedsRefresh(name, p.Usage, now) {
				continue
			}
			w.mu.Lock()
			if w.providerLastUsage == nil {
				w.providerLastUsage = map[string]time.Time{}
			}
			w.providerLastUsage[name] = now
			w.mu.Unlock()
			r, err := w.execUsage(ctx, p.Usage)
			if err == nil {
				r.At = now.UTC()
				err = validateReadingAt(r, p.Usage, now)
			}
			w.mu.Lock()
			if w.providerUsageErr == nil {
				w.providerUsageErr = map[string]string{}
			}
			if err != nil {
				w.providerUsageErr[name] = err.Error()
				w.mu.Unlock()
				continue
			}
			w.providerUsageErr[name] = ""
			w.mu.Unlock()
			_ = w.store.WriteFile(usageProviderFile(name), r)
		}
		return
	}
	w.mu.Lock()
	w.lastUsage = now
	w.mu.Unlock()
	r, err := w.execUsage(ctx, k.Usage)
	if err == nil {
		r.At = now.UTC()
		err = validateReadingAt(r, k.Usage, now)
	}
	if err != nil {
		w.mu.Lock()
		w.usageErr = err.Error()
		w.mu.Unlock()
		return
	}
	_ = w.store.WriteFile(usageFile, r)
	w.mu.Lock()
	w.usageErr = ""
	w.mu.Unlock()
}

func (w *watcher) currentUsage(k config.Capacity, now time.Time) (config.Capacity, *usageView) {
	if len(k.Providers) > 0 {
		w.mu.Lock()
		errs := maps.Clone(w.providerUsageErr)
		w.mu.Unlock()
		return w.cachedProvidersCapacityAt(k, now, errs)
	}
	if !k.Usage.Enabled() {
		return k, nil
	}
	w.mu.Lock()
	uerr := w.usageErr
	w.mu.Unlock()
	if uerr != "" {
		return errorUsageView(k, errors.New(uerr))
	}
	var r usageReading
	if found, err := w.store.ReadFile(usageFile, &r); err == nil && found && validateReadingAt(r, k.Usage, now) == nil {
		return deriveUsageView(r, k, now)
	}
	return k, nil
}

// watchUsage refreshes subscription usage when due, prints USAGE lines, tracks
// USAGE HIGH / ENDED USAGE HIGH and tells live busy workers once per USAGE HIGH
// episode to finish their step and park.
func (w *watcher) watchUsage(ctx context.Context, st *state.State, sessions []*claude.Session) *usageView {
	k := w.liveCapacity()
	if len(k.Providers) > 0 {
		return w.watchProvidersUsage(ctx, k, st, sessions)
	}
	if !k.Usage.Enabled() {
		w.clear("usage-error")
		w.clear("usage-high")
		w.clearUsageHighEpisode(usageHighFile)
		return nil
	}
	if !w.chores {
		_, u := w.resolveUsage(ctx, k)
		if u == nil {
			return nil
		}
		if u.Error != "" {
			w.fail("usage-error", "USAGE unreadable: %s", u.Error)
			return u
		}
		w.clear("usage-error")
		w.emitNow("usage", "USAGE: %s", u.watchLine(w.now))
		w.check("usage-high", u.High, "%s", u.highWatchLine(w.now))
		return u
	}
	now := w.now
	if w.usageNeedsRefresh(k, now) {
		w.inFlight(ctx, cmp.Or(w.cfg.Watch.Interval.Duration, k.Usage.EffectiveTimeout()), &w.usageBusy, func(ctx context.Context) {
			w.refreshUsage(ctx, k, now)
		})
	}
	_, u := w.currentUsage(k, w.now)
	if u == nil {
		return nil
	}
	if u.Error != "" {
		w.fail("usage-error", "USAGE unreadable: %s", u.Error)
		return u
	}
	w.clear("usage-error")
	if rep := k.Usage.ReportEvery.Duration; rep > 0 {
		w.mu.Lock()
		due := w.usageReported.IsZero() || w.now.Sub(w.usageReported) >= rep
		if due {
			w.usageReported = w.now
			w.dirty = true
		}
		w.mu.Unlock()
		if due {
			w.emitNow("usage", "USAGE: %s", u.watchLine(w.now))
		}
	}
	w.check("usage-high", u.High, "%s", u.highWatchLine(w.now))
	if !u.High {
		w.clearUsageHighEpisode(usageHighFile)
		return u
	}
	if st.Stop == nil {
		w.tellBusyWorkersUsageHigh(ctx, st, sessions, u, usageHighFile, nil)
	}
	return u
}

func (w *watcher) watchProvidersUsage(ctx context.Context, k config.Capacity, st *state.State, sessions []*claude.Session) *usageView {
	readings := map[string]providerReadingResult{}
	if !w.chores {
		for _, name := range k.ProviderNames() {
			p := k.Providers[name]
			if !p.Enabled() {
				continue
			}
			r, err := w.resolveReading(ctx, usageProviderFile(name), p.Usage)
			if err != nil {
				w.mu.Lock()
				if w.providerUsageErr == nil {
					w.providerUsageErr = map[string]string{}
				}
				w.providerUsageErr[name] = err.Error()
				w.mu.Unlock()
				readings[name] = providerReadingResult{err: err}
				continue
			}
			readings[name] = providerReadingResult{reading: r, ok: true}
		}
	} else {
		now := w.now
		if w.usageNeedsRefresh(k, now) {
			w.inFlight(ctx, cmp.Or(w.cfg.Watch.Interval.Duration, config.DefaultUsageTimeout), &w.usageBusy, func(ctx context.Context) {
				w.refreshUsage(ctx, k, now)
			})
		}
		for _, name := range k.ProviderNames() {
			p := k.Providers[name]
			if !p.Enabled() {
				continue
			}
			w.mu.Lock()
			uerr := w.providerUsageErr[name]
			w.mu.Unlock()
			if uerr != "" {
				readings[name] = providerReadingResult{err: errors.New(uerr)}
				continue
			}
			if w.store != nil {
				var r usageReading
				if found, err := w.store.ReadFile(usageProviderFile(name), &r); err == nil && found {
					if verr := validateReadingAt(r, p.Usage, w.now); verr != nil {
						readings[name] = providerReadingResult{err: verr}
					} else {
						readings[name] = providerReadingResult{reading: r, ok: true}
					}
				}
			}
		}
	}

	c := countAgents(st, sessions, w.now)
	pa := countProviderAgents(st, sessions, k, w.now)
	for _, name := range k.ProviderNames() {
		p := k.Providers[name]
		if !p.SelfHosted() {
			continue
		}
		// A self-hosted provider reads no subscription usage; its ceiling may
		// follow the serving mode of the model it serves, and the modes a
		// node constraint holds back lower it before any switch ran.
		readings[name] = w.serveModeReadings(ctx, name, p)
	}
	w.mu.Lock()
	swap := w.swap
	w.mu.Unlock()
	var h *headroom
	if w.readHeadroom != nil {
		h = w.readHeadroom(ctx, swap)
	} else {
		h = w.app.readHeadroom(ctx, k, swap)
	}
	v := buildProvidersCapacity(c, pa, k, readings, h, false, nil, forWorker, w.now).stopped(w.app, st.Stop)

	var parts []string
	allHigh := len(k.Providers) > 0
	for _, name := range k.ProviderNames() {
		p := k.Providers[name]
		if !p.Enabled() {
			allHigh = false
		}
		pv := v.Providers[name]
		if pv == nil {
			continue
		}
		if pv.Kind == config.KindSelfHosted {
			// No subscription behind it: no USAGE HIGH, nothing to tell its
			// workers to park for; its own ceiling is the only guard.
			allHigh = false
			if pv.ServingModeError != "" {
				w.fail("serve-mode-error-"+name, "SERVE MODE %s unreadable: %s", name, pv.ServingModeError)
			} else {
				w.clear("serve-mode-error-" + name)
			}
			parts = append(parts, name+": "+pv.watchSummary(w.now))
			continue
		}
		if pv.Error != "" {
			w.fail("usage-error-"+name, "USAGE %s unreadable: %s", name, pv.Error)
		} else {
			w.clear("usage-error-" + name)
		}
		parts = append(parts, name+": "+pv.watchSummary(w.now))
		if len(pv.Buckets) > 0 && !pv.supplementalBuckets {
			for _, bName := range pv.bucketOrder {
				bv := pv.Buckets[bName]
				if bv == nil {
					continue
				}
				target := name + "/" + bName
				highLine := fmt.Sprintf("USAGE HIGH (%s): %s; new starts and resumes on %s wait until reset, telling busy workers to finish their step and park",
					target, highFigureText(bv.FiveHour, pv.HighPercent, w.now), target)
				w.check("usage-high-"+name+"-"+bName, bv.High, "%s", highLine)
				ledgerFile := usageHighBucketFile(name, bName)
				if !bv.High {
					allHigh = false
					w.clearUsageHighEpisode(ledgerFile)
				} else if st.Stop == nil {
					u := &usageView{FiveHour: bv.FiveHour, High: true, HighPercent: pv.HighPercent}
					w.tellBusyWorkersUsageHigh(ctx, st, sessions, u, ledgerFile, func(ag state.Agent) bool {
						return slices.Contains(bv.Busy, ag.Name)
					})
				}
			}
			continue
		}
		highLine := fmt.Sprintf("USAGE HIGH (%s): %s; new starts and resumes on %s wait until reset, telling busy workers to finish their step and park",
			name, highFigureText(pv.FiveHour, pv.HighPercent, w.now), name)
		w.check("usage-high-"+name, pv.High, "%s", highLine)
		if !pv.High {
			allHigh = false
			w.clearUsageHighEpisode(usageHighProviderFile(name))
		} else if st.Stop == nil {
			u := &usageView{FiveHour: pv.FiveHour, High: true, HighPercent: pv.HighPercent}
			w.tellBusyWorkersUsageHigh(ctx, st, sessions, u, usageHighProviderFile(name), func(ag state.Agent) bool {
				return slices.Contains(pv.Busy, ag.Name)
			})
		}
	}
	if v.Choice != nil {
		parts = append(parts, "choice: "+v.Choice.summary())
	}
	shouldEmit := !w.chores
	if w.chores {
		switch {
		case !k.Usage.ReportEverySet():
			shouldEmit = true
		case k.Usage.ReportEvery.Duration > 0:
			w.mu.Lock()
			due := w.usageReported.IsZero() || w.now.Sub(w.usageReported) >= k.Usage.ReportEvery.Duration
			if due {
				w.usageReported = w.now
				w.dirty = true
			}
			w.mu.Unlock()
			shouldEmit = due
		}
	}
	if shouldEmit && len(parts) > 0 {
		w.emitNow("usage", "USAGE: %s", strings.Join(parts, "; "))
	}
	return &usageView{Floor: v.Floor, Ceiling: v.Ceiling, High: allHigh}
}

func (w *watcher) clearUsageHighEpisode(file string) {
	if w.store == nil {
		return
	}
	var l usageHighLedger
	if found, err := w.store.ReadFile(file, &l); err == nil && found && l.Episode != "" {
		_ = w.store.WriteFile(file, usageHighLedger{})
	}
}

func (w *watcher) tellBusyWorkersUsageHigh(ctx context.Context, st *state.State, sessions []*claude.Session, u *usageView, ledgerFile string, match func(state.Agent) bool) {
	var l usageHighLedger
	if w.store != nil {
		_, _ = w.store.ReadFile(ledgerFile, &l)
	}
	var reset time.Time
	if u.FiveHour != nil {
		reset = u.FiveHour.ResetsAt
	}
	newEpisode := l.Episode == "" ||
		(!l.ResetsAt.IsZero() && !reset.IsZero() && reset.Sub(l.ResetsAt) > episodeGap)
	changed := false
	if newEpisode {
		l = usageHighLedger{
			Episode:  w.now.UTC().Format(time.RFC3339),
			ResetsAt: reset,
			Told:     map[string]string{},
		}
		changed = true
	} else if !reset.IsZero() && !reset.Equal(l.ResetsAt) {
		l.ResetsAt = reset
		changed = true
	}
	if l.Told == nil {
		l.Told = map[string]string{}
	}
	msg := fmt.Sprintf("USAGE HIGH (%s): finish your current step, commit or save any work in flight, and park with `beekeeper agents park \"5-hour usage window reset\"` (or `--on` your open PR or note) so the 5-hour window can reset.",
		u.highFigure(w.now))
	for _, ag := range liveBusyWorkers(st, sessions, w.now) {
		if match != nil && !match(ag) {
			continue
		}
		key := cmp.Or(ag.Session, ag.Name)
		if l.Told[key] != "" {
			continue
		}
		quiet := *w.app
		quiet.out = io.Discard
		if err := wakeOwner(&quiet, ctx, watchParty, ag.Name, msg, ""); err != nil {
			w.emit("usage-wake-"+key, "USAGE HIGH WAKE FAILED %q: %v", ag.Name, err)
			continue
		}
		w.clear("usage-wake-" + key)
		l.Told[key] = w.now.UTC().Format(time.RFC3339)
		changed = true
		_ = w.store.Log(event(watchParty, verbUsageHigh, "%s: told to finish its step and park (%s)", ag.Name, u.highFigure(w.now)))
	}
	if changed && w.store != nil {
		_ = w.store.WriteFile(ledgerFile, l)
	}
}

// liveBusyWorkers are the busy workers whose CLI is running now: role holders,
// open relay successors, the one-off reporter, parked/kept/waiting agents and
// stopped CLIs are skipped so USAGE HIGH never launches a headless turn.
func liveBusyWorkers(st *state.State, sessions []*claude.Session, now time.Time) []state.Agent {
	var out []state.Agent
	sup, guide := st.SupervisorRole(), st.GuideRole()
	for _, ag := range st.Agents {
		p := ag.Party
		if roleRun(st, p) || sup.Relay.Open(now) && sup.Relay.To.Is(p) || guide.Relay.Open(now) && guide.Relay.To.Is(p) {
			continue
		}
		if st.Report.Running() && ag.Is(st.Report.Party) {
			continue
		}
		if ag.Task == "" || ag.Done || keptBy(st, ag, now) != "" {
			continue
		}
		s, live := claude.Live(sessions, p)
		if !live || s.Waiting != nil || s.OwnWait(now, waitQuiet) != "" {
			continue
		}
		out = append(out, ag)
	}
	return out
}
