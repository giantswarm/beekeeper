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

	"github.com/giantswarm/beekeeper/internal/agy"
	"github.com/giantswarm/beekeeper/internal/claude"
	clk "github.com/giantswarm/beekeeper/internal/clock"
	"github.com/giantswarm/beekeeper/internal/codex"
	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/loop"
	"github.com/giantswarm/beekeeper/internal/state"
)

const (
	// usageFile is the state's side file holding the latest subscription
	// usage reading, shared by watch, capacity, snapshot and agents start/resume.
	usageFile = "usage.json"
	// codexAccountUsageFile shares the last account probe across provider aliases.
	codexAccountUsageFile = "codex-account-usage.json"
	// usageHighFile records the busy workers already told to park in the
	// current USAGE HIGH episode, so watch restarts never tell a worker twice.
	usageHighFile = "usage-high.json"
	// usageUnderusedFile records whether a P1 finding was already filed for
	// the current 7-day subscription window's heavy underuse.
	usageUnderusedFile = "usage-underused.json"
	// verbUsageHigh logs each busy worker told to park on USAGE HIGH.
	verbUsageHigh = "usage.high"
	// episodeGap separates consecutive 5-hour windows if watch slept across
	// a reset boundary while tolerating small resetsAt jitter within one window.
	episodeGap = 15 * time.Minute
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

func usageUnderusedTargetFile(target string) string {
	if strings.TrimSpace(target) == "" {
		return usageUnderusedFile
	}
	return "usage-underused-" + unsafeKey.ReplaceAllString(target, "_") + ".json"
}

func usageQuotaFile(name string) string {
	if strings.TrimSpace(name) == "" {
		return "usage-quota.json"
	}
	return "usage-quota-" + unsafeKey.ReplaceAllString(name, "_") + ".json"
}

func quotaFileForUsageFile(file string) string {
	if file == usageFile {
		return "usage-quota.json"
	}
	if rest, ok := strings.CutPrefix(file, "usage-"); ok {
		if !strings.HasPrefix(rest, "high-") && !strings.HasPrefix(rest, "quota-") && !strings.HasPrefix(rest, "underused-") {
			return "usage-quota-" + rest
		}
	}
	return ""
}

type usageQuotaEntry struct {
	At         time.Time `json:"at"`
	ResetsAt   time.Time `json:"resetsAt"`
	ShortError string    `json:"shortError,omitempty"`
}

type usageQuotaLedger struct {
	Buckets   map[string]usageQuotaEntry `json:"buckets,omitempty"`
	Processed map[string]time.Time       `json:"processed,omitempty"`
}

func applyQuotaForUsageFile(store state.Store, file string, r *UsageReading, now time.Time) {
	if store == nil || r == nil {
		return
	}
	qf := quotaFileForUsageFile(file)
	if qf == "" {
		return
	}
	if now.IsZero() {
		now = clk.Now()
	}
	var ledger usageQuotaLedger
	found, err := store.ReadFile(qf, &ledger)
	if err != nil || !found || len(ledger.Buckets) == 0 {
		return
	}
	for _, bucket := range slices.Sorted(maps.Keys(ledger.Buckets)) {
		entry := ledger.Buckets[bucket]
		if entry.ResetsAt.IsZero() || !now.Before(entry.ResetsAt) {
			continue
		}
		if bucket == "" {
			r.FiveHour = &UsageWindow{UsedPercent: 100, ResetsAt: entry.ResetsAt}
			r.SevenDay = &UsageWindow{UsedPercent: 100, ResetsAt: entry.ResetsAt}
			continue
		}
		if r.Buckets == nil {
			r.Buckets = map[string]UsageBucket{}
		}
		b, exists := r.Buckets[bucket]
		b.FiveHour = &UsageWindow{UsedPercent: 100, ResetsAt: entry.ResetsAt}
		b.SevenDay = &UsageWindow{UsedPercent: 100, ResetsAt: entry.ResetsAt}
		if !exists {
			r.BucketOrder = append(r.BucketOrder, bucket)
		}
		r.Buckets[bucket] = b
	}
}

func (a *app) recordAgyQuotaExhausted(model string, qerr *agy.QuotaError, now time.Time) {
	a.recordAgyQuotaExhaustedForID("", model, qerr, now)
}

func (a *app) recordAgyQuotaExhaustedForID(id, model string, qerr *agy.QuotaError, now time.Time) {
	if a == nil || a.store == nil || qerr == nil {
		return
	}
	if now.IsZero() {
		now = a.now
	}
	if now.IsZero() {
		now = clk.Now()
	}
	m := cmp.Or(strings.TrimSpace(model), strings.TrimSpace(qerr.Model))
	k := a.liveCapacity()
	prov, bucket := ResolveAgentProviderBucket(k, agy.Harness, m)
	if prov == "" {
		prov = agy.Harness
	}
	if bucket == "" {
		bucket = k.Providers[prov].BucketFor(m)
	}
	qf := usageQuotaFile(prov)
	var ledger usageQuotaLedger
	_, _ = a.store.ReadFile(qf, &ledger)
	if id != "" && !qerr.At.IsZero() && ledger.Processed[id].Equal(qerr.At) {
		return
	}
	base := now
	if !qerr.At.IsZero() {
		if !qerr.At.After(now.Add(10 * time.Minute)) {
			base = qerr.At
		}
	}
	var resetsAt time.Time
	if qerr.ResetsIn > 0 {
		resetsAt = base.Add(qerr.ResetsIn).UTC()
	} else {
		var cached UsageReading
		if found, err := a.store.ReadFile(usageProviderFile(prov), &cached); err == nil && found {
			if b, ok := cached.Buckets[bucket]; ok {
				for _, w := range []*UsageWindow{b.FiveHour, b.SevenDay} {
					if w != nil && w.ResetsAt.After(base) && (resetsAt.IsZero() || w.ResetsAt.Before(resetsAt)) {
						resetsAt = w.ResetsAt.UTC()
					}
				}
			}
		}
		if resetsAt.IsZero() {
			resetsAt = base.Add(5 * time.Hour).UTC()
		}
	}
	if ledger.Buckets == nil {
		ledger.Buckets = map[string]usageQuotaEntry{}
	}
	for bName, existing := range ledger.Buckets {
		if existing.ResetsAt.IsZero() || !base.Before(existing.ResetsAt) {
			delete(ledger.Buckets, bName)
		}
	}
	if existing, ok := ledger.Buckets[bucket]; !ok || resetsAt.After(existing.ResetsAt) {
		ledger.Buckets[bucket] = usageQuotaEntry{
			At:         base.UTC(),
			ResetsAt:   resetsAt,
			ShortError: qerr.ShortError,
		}
	}
	if id != "" && !qerr.At.IsZero() {
		if ledger.Processed == nil {
			ledger.Processed = map[string]time.Time{}
		}
		ledger.Processed[id] = qerr.At
	}
	_ = a.store.WriteFile(qf, ledger)
}

func (a *app) syncAgyQuotaForID(id, model string) {
	if a == nil || a.cfg == nil || strings.TrimSpace(id) == "" {
		return
	}
	qerr := agy.ReadQuotaError(a.cfg.StateDir, id)
	if qerr == nil {
		return
	}
	a.recordAgyQuotaExhaustedForID(id, model, qerr, a.now)
}

func (a *app) syncAgyQuotaFromState(st *state.State, now time.Time) {
	if a == nil || a.cfg == nil || a.cfg.StateDir == "" {
		return
	}
	if st == nil && a.store != nil {
		st, _ = a.store.Peek()
	}
	if st == nil {
		return
	}
	seen := map[string]bool{}
	for _, ag := range st.Agents {
		id := agyAgentStartID(ag)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		if qerr := agy.ReadQuotaError(a.cfg.StateDir, id); qerr != nil {
			_, model := AgentHarnessModel(st, nil, ag)
			a.recordAgyQuotaExhaustedForID(id, model, qerr, now)
		}
	}
	for _, s := range st.Starts {
		id, ok := strings.CutPrefix(s.HostSession, agy.HostPrefix)
		if !ok || id == "" || seen[id] {
			continue
		}
		seen[id] = true
		if qerr := agy.ReadQuotaError(a.cfg.StateDir, id); qerr != nil {
			a.recordAgyQuotaExhaustedForID(id, s.Model, qerr, now)
		}
	}
}

// usageHighLedger tracks the current USAGE HIGH episode and who was told.
type usageHighLedger struct {
	Episode  string            `json:"episode,omitempty"`
	ResetsAt time.Time         `json:"resetsAt,omitzero"`
	Told     map[string]string `json:"told,omitempty"`
}

// usageUnderusedLedger tracks whether a P1 finding was already filed for the
// current 7-day subscription window.
type usageUnderusedLedger struct {
	ResetsAt   time.Time `json:"resetsAt,omitzero"`
	ReportedAt time.Time `json:"reportedAt,omitzero"`
}

func runUsageCommand(ctx context.Context, command []string, timeout time.Duration) (UsageReading, error) {
	return runUsageCommandInDir(ctx, command, timeout, "")
}

func runUsageCommandInDir(ctx context.Context, command []string, timeout time.Duration, dir string) (UsageReading, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	c := exec.CommandContext(ctx, command[0], command[1:]...) //nolint:gosec // the configured usage command
	c.Dir = dir
	c.Env = usageProbeEnv(command)
	var stdout, stderr bytes.Buffer
	c.Stdout, c.Stderr = &stdout, &stderr
	if err := c.Run(); err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return UsageReading{}, fmt.Errorf("%s timed out after %s", filepath.Base(command[0]), timeout)
		}
		if msg := oneLine(stderr.String()); msg != "" {
			return UsageReading{}, fmt.Errorf("%s: %s", err, truncate(msg, 120))
		}
		return UsageReading{}, err
	}
	if slices.Equal(command, config.DefaultClaudeUsageCommand) {
		return parseClaudeUsage(stdout.Bytes(), clk.Now())
	}
	if slices.Equal(command, config.DefaultAgyUsageCommand) {
		return parseAgyBuiltinUsage(stdout.Bytes())
	}
	return parseUsageOutput(stdout.Bytes())
}

// usageProbeEnv is the environment of a built-in usage command: a local
// probe is not a turn of the worker that requested it, so it carries none of
// the worker's session identity. A configured command (nil) inherits all.
func usageProbeEnv(command []string) []string {
	var drop func(key string) bool
	switch {
	case slices.Equal(command, config.DefaultClaudeUsageCommand):
		drop = func(key string) bool {
			return key == claudeCodeEnv || key == "CLAUDE_CODE_ENTRYPOINT" || key == "CLAUDE_CODE_PARENT_SESSION_ID" ||
				strings.HasPrefix(key, "CLAUDE_CODE_SESSION_") || strings.HasPrefix(key, "CLAUDE_CODE_HOST_SESSION_")
		}
	case slices.Equal(command, config.DefaultAgyUsageCommand):
		// Its PreInvocation hook must not take the worker's inbox.
		drop = func(key string) bool { return key == agy.EnvAgent || key == agy.EnvName }
	default:
		return nil
	}
	env := make([]string, 0)
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if !drop(key) {
			env = append(env, entry)
		}
	}
	return env
}

func parseUsageOutput(raw []byte) (UsageReading, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return UsageReading{}, errors.New("empty usage output")
	}
	if trimmed[0] == '{' {
		return parseUsageJSON(trimmed)
	}
	return parseAgyUsage(trimmed)
}

func parseUsageJSON(raw []byte) (UsageReading, error) {
	type rawWindow struct {
		UsedPercent json.RawMessage `json:"usedPercent"`
		ResetsAt    json.RawMessage `json:"resetsAt"`
		Pace        *float64        `json:"pace"`
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
		return UsageReading{}, fmt.Errorf("invalid JSON: %w", err)
	}
	conv := func(name string, w *rawWindow) (*UsageWindow, error) {
		if w == nil {
			return nil, nil
		}
		// Only an explicit pair of nulls denotes an unopened 5-hour window.
		// Omitted fields and unknown weekly usage still fail closed.
		idle := (name == "fiveHour" || strings.HasSuffix(name, ".fiveHour")) &&
			bytes.Equal(bytes.TrimSpace(w.UsedPercent), []byte("null")) &&
			bytes.Equal(bytes.TrimSpace(w.ResetsAt), []byte("null"))
		var used *float64
		if len(w.UsedPercent) > 0 {
			if err := json.Unmarshal(w.UsedPercent, &used); err != nil {
				return nil, fmt.Errorf("%s.usedPercent: %w", name, err)
			}
		}
		if used == nil && !idle {
			return nil, fmt.Errorf("%s.usedPercent is missing", name)
		}
		var v float64
		if used != nil {
			v = *used
		}
		if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
			return nil, fmt.Errorf("%s.usedPercent %g is invalid", name, v)
		}
		var reset time.Time
		var resetsAt string
		if len(w.ResetsAt) > 0 {
			if err := json.Unmarshal(w.ResetsAt, &resetsAt); err != nil {
				return nil, fmt.Errorf("%s.resetsAt: %w", name, err)
			}
		}
		if s := strings.TrimSpace(resetsAt); s != "" {
			var err error
			reset, err = time.Parse(time.RFC3339Nano, s)
			if err != nil {
				return nil, fmt.Errorf("%s.resetsAt: %w", name, err)
			}
			reset = reset.UTC()
		}
		out := &UsageWindow{UsedPercent: v, ResetsAt: reset}
		if w.Pace != nil && !math.IsNaN(*w.Pace) && !math.IsInf(*w.Pace, 0) {
			p := *w.Pace
			out.Pace = &p
		}
		return out, nil
	}
	var r UsageReading
	r.Stale = doc.Stale
	if doc.AgeMinutes != nil && !math.IsNaN(*doc.AgeMinutes) && !math.IsInf(*doc.AgeMinutes, 0) {
		age := *doc.AgeMinutes
		r.AgeMinutes = &age
	}
	if s := strings.TrimSpace(doc.At); s != "" {
		t, err := time.Parse(time.RFC3339Nano, s)
		if err != nil {
			return UsageReading{}, fmt.Errorf("at: %w", err)
		}
		r.SourceAt = t.UTC()
	}
	var err error
	if r.FiveHour, err = conv("fiveHour", doc.FiveHour); err != nil {
		return UsageReading{}, err
	}
	if r.SevenDay, err = conv("sevenDay", doc.SevenDay); err != nil {
		return UsageReading{}, err
	}
	if r.SevenDay != nil && r.SevenDay.Pace == nil && doc.Pace != nil && !math.IsNaN(*doc.Pace) && !math.IsInf(*doc.Pace, 0) {
		p := *doc.Pace
		r.SevenDay.Pace = &p
	}
	if len(doc.Buckets) > 0 {
		r.Buckets = make(map[string]UsageBucket, len(doc.Buckets))
		for _, bName := range slices.Sorted(maps.Keys(doc.Buckets)) {
			rb := doc.Buckets[bName]
			fh, err := conv(bName+".fiveHour", rb.FiveHour)
			if err != nil {
				return UsageReading{}, err
			}
			sd, err := conv(bName+".sevenDay", rb.SevenDay)
			if err != nil {
				return UsageReading{}, err
			}
			if fh == nil && sd == nil {
				return UsageReading{}, fmt.Errorf("bucket %q has neither fiveHour nor sevenDay", bName)
			}
			r.Buckets[bName] = UsageBucket{FiveHour: fh, SevenDay: sd}
			r.BucketOrder = append(r.BucketOrder, bName)
		}
	}
	if r.FiveHour == nil && r.SevenDay == nil && len(r.Buckets) == 0 {
		if cr, ok, cerr := parseCodexRateLimitsJSON(raw); ok {
			return cr, cerr
		}
		return UsageReading{}, errors.New("usage output has neither fiveHour nor sevenDay")
	}
	return r, nil
}

func parseCodexRateLimitsJSON(raw []byte) (UsageReading, bool, error) {
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
		return UsageReading{}, false, nil
	}
	isCodex := len(probe.RateLimits) > 0 || len(probe.Primary) > 0 || len(probe.Secondary) > 0 ||
		probe.RateLimitReached != nil || probe.SpendControlReached != nil ||
		probe.Type == "token_count" ||
		(probe.Payload != nil && (probe.Payload.Type == "token_count" || len(probe.Payload.RateLimits) > 0))
	if !isCodex {
		return UsageReading{}, false, nil
	}
	cr, err := codex.ParseRateLimits(raw, time.Time{})
	if err != nil {
		return UsageReading{}, true, err
	}
	return codexUsageToReading(cr), true, nil
}

// parseAgyUsage parses the tab-separated lines printed by `agy -p /usage`:
//
//	Gemini Models\tWeekly Limit Remaining\t78%\t2026-11-03T09:23:51Z
//	Gemini Models\tFive Hour Limit Remaining\t94%\t2026-10-28T05:23:51Z
func parseAgyUsage(raw []byte) (UsageReading, error) {
	r := UsageReading{Buckets: map[string]UsageBucket{}}
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
			return UsageReading{}, fmt.Errorf("invalid percentage %q in %q", fields[2], line)
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
					return UsageReading{}, fmt.Errorf("invalid reset time %q in %q: %w", s, line, err)
				}
				reset = reset.UTC()
			}
		}
		w := &UsageWindow{UsedPercent: used, ResetsAt: reset}
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
		return UsageReading{}, fmt.Errorf("invalid usage output: %s", truncate(oneLine(string(raw)), 120))
	}
	return r, nil
}

// parseAgyBuiltinUsage reads the built-in `agy -p /usage` report. Each model
// bucket maps to its 5-hour and 7-day windows; a bucket that prints only one
// of them is left out, so a choice on it reads as not in the output and gets
// no start, while the complete buckets still count.
func parseAgyBuiltinUsage(raw []byte) (UsageReading, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return UsageReading{}, errors.New("agy /usage printed nothing")
	}
	r, err := parseAgyUsage(trimmed)
	if err != nil {
		return UsageReading{}, fmt.Errorf("agy /usage: %w", err)
	}
	var partial []string
	order := r.BucketOrder[:0]
	for _, name := range r.BucketOrder {
		if b := r.Buckets[name]; b.FiveHour == nil || b.SevenDay == nil {
			partial = append(partial, name)
			delete(r.Buckets, name)
			continue
		}
		order = append(order, name)
	}
	r.BucketOrder = order
	if len(r.Buckets) == 0 {
		return UsageReading{}, fmt.Errorf("agy /usage has no bucket with both a five-hour and a weekly window (%s)", strings.Join(partial, ", "))
	}
	return r, nil
}

func (a *app) execUsage(ctx context.Context, u config.Usage) (UsageReading, error) {
	if a.readUsage != nil {
		return a.readUsage(ctx, u.Command, u.EffectiveTimeout())
	}
	if slices.Equal(u.Command, config.DefaultCodexUsageCommand) {
		return a.readCodexUsage(ctx, u)
	}
	// A built-in local probe runs in a directory of its own under the state,
	// never in a worktree whose project settings it could pick up.
	for _, builtin := range [][]string{config.DefaultClaudeUsageCommand, config.DefaultAgyUsageCommand} {
		if !slices.Equal(u.Command, builtin) {
			continue
		}
		dir := filepath.Join(a.cfg.StateDir, "usage", builtin[0])
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return UsageReading{}, err
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
		now = clk.Now()
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

// codexAccountSnapshot retains explicit account stops as well as healthy usage.
// A transport failure cannot establish that an earlier stop has cleared.
type codexAccountSnapshot struct {
	UsageReading
	Stopped bool `json:"stopped,omitempty"`
}

func (a *app) readCodexUsage(ctx context.Context, u config.Usage) (UsageReading, error) {
	var stateDir, sessionsDir string
	if a.cfg != nil {
		stateDir = a.cfg.StateDir
		sessionsDir = a.cfg.Codex.SessionsDir
	}
	cr, err := codex.ReadUsage(stateDir, sessionsDir)
	now := a.now
	if now.IsZero() {
		now = clk.Now()
	}
	// Fresh explicit local errors remain authoritative. Once old, probe the
	// account so an idle desk can recover after its quota resets.
	if err != nil && !errors.Is(err, codex.ErrNoUsage) && (cr.At.IsZero() || now.Sub(cr.At) <= u.EffectiveEvery()) {
		return UsageReading{}, err
	}
	// Prefer a fresh account snapshot over an older rollout. Still read local
	// events first so a new explicit stop cannot hide behind the snapshot.
	var snapshot codexAccountSnapshot
	haveCached := false
	if a.store != nil {
		found, e := a.store.ReadFile(codexAccountUsageFile, &snapshot)
		haveCached = e == nil && found && !snapshot.SourceAt.IsZero()
	}
	cached := snapshot.UsageReading
	stopped := haveCached && snapshot.Stopped && !cached.SourceAt.Before(cr.At)
	if stopped && now.Sub(cached.SourceAt) <= u.EffectiveEvery() {
		return UsageReading{}, codex.ErrUsageStopped
	}
	if !stopped && err == nil && now.Sub(cr.At) <= u.EffectiveEvery() {
		return codexUsageToReading(cr), nil
	}
	if haveCached && !snapshot.Stopped && !cached.SourceAt.Before(cr.At) && now.Sub(cached.SourceAt) <= u.EffectiveEvery() && UsageFresh(cached, u, now) {
		return cached, nil
	}
	dir := filepath.Join(stateDir, "usage", "codex")
	if mkdirErr := os.MkdirAll(dir, 0o700); mkdirErr != nil {
		return UsageReading{}, mkdirErr
	}
	probeCtx, cancel := context.WithTimeout(ctx, u.EffectiveTimeout())
	defer cancel()
	home := ""
	if _, statErr := os.Stat(filepath.Join(codex.HomeDir(stateDir), "auth.json")); statErr == nil {
		home = codex.HomeDir(stateDir)
	}
	fresh, probeErr := codex.ProbeUsage(probeCtx, dir, home, now)
	if probeErr == nil {
		r := codexUsageToReading(fresh)
		r.At = now.UTC()
		if a.store != nil {
			_ = a.store.WriteFile(codexAccountUsageFile, r)
		}
		return r, nil
	}
	if errors.Is(probeErr, codex.ErrUsageStopped) {
		if a.store != nil {
			snapshot = codexAccountSnapshot{UsageReading: UsageReading{At: now.UTC(), SourceAt: now.UTC()}, Stopped: true}
			if writeErr := a.store.WriteFile(codexAccountUsageFile, snapshot); writeErr != nil {
				return UsageReading{}, fmt.Errorf("%w: persist account stop: %v", probeErr, writeErr)
			}
		}
		return UsageReading{}, probeErr
	}
	if stopped {
		return UsageReading{}, codex.ErrUsageStopped
	}
	if codex.QuotaStop(err) {
		return UsageReading{}, err
	}
	if haveCached && !snapshot.Stopped && (err == nil || errors.Is(err, codex.ErrNoUsage)) && cached.SourceAt.After(cr.At) {
		cached.At = now.UTC()
		if ValidateReadingAt(cached, u, now) == nil {
			cached.Fallback = "account refresh failed; using last Codex reading: " + probeErr.Error()
			return cached, nil
		}
	}
	if err != nil {
		return UsageReading{}, probeErr
	}
	r := codexUsageToReading(cr)
	r.Fallback = "account refresh failed; using last Codex reading: " + probeErr.Error()
	return r, nil
}

func (a *app) useCodexRolloutUsage(u config.Usage, cached UsageReading) bool {
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

func codexUsageToReading(cr codex.UsageReading) UsageReading {
	r := UsageReading{
		SourceAt:     cr.At,
		SingleWindow: cr.FiveHour == nil && cr.SevenDay != nil,
	}
	if cr.FiveHour != nil {
		r.FiveHour = &UsageWindow{
			UsedPercent: cr.FiveHour.UsedPercent,
			ResetsAt:    cr.FiveHour.ResetsAt,
		}
	}
	if cr.SevenDay != nil {
		r.SevenDay = &UsageWindow{
			UsedPercent: cr.SevenDay.UsedPercent,
			ResetsAt:    cr.SevenDay.ResetsAt,
		}
	}
	return r
}

// cachedUsageCapacity derives Capacity from a fresh usage.json (or cached
// provider usage files) if one exists, without running any subprocess.
func (a *app) cachedUsageCapacity(k config.Capacity) (config.Capacity, *UsageView) {
	if len(k.Providers) > 0 {
		return a.cachedProvidersCapacityAt(k, a.now, nil)
	}
	if !k.Usage.Enabled() || a.store == nil {
		return k, nil
	}
	var r UsageReading
	if found, err := a.store.ReadFile(usageFile, &r); err == nil && found &&
		UsageFresh(r, k.Usage, a.now) && ValidateReadingAt(r, k.Usage, a.now) == nil {
		return DeriveUsageView(r, k, a.now)
	}
	return k, nil
}

func (a *app) cachedProvidersCapacityAt(k config.Capacity, now time.Time, providerErrs map[string]string) (config.Capacity, *UsageView) {
	derived := k
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
			selfHostedCeiling += p.Ceiling
			continue
		}
		if !p.Enabled() {
			anySubscription = true
			subDerivedCeiling += p.Ceiling
			continue
		}
		var cause error
		if providerErrs[name] != "" {
			cause = errors.New(providerErrs[name])
		}
		r, err := a.cachedUsageReading(usageProviderFile(name), p.Usage, now, cause)
		if err != nil {
			errs = append(errs, name+": "+err.Error())
			continue
		}
		pv := BuildSingleProviderView(name, p, ProviderAgents{}, ProviderReadingResult{Reading: r, OK: true}, now)
		anySubscription = true
		subDerivedCeiling += pv.Ceiling
	}
	derived.Ceiling = selfHostedCeiling + subDerivedCeiling
	if anySubscription {
		// capacity.ceiling caps the subscription providers, not the
		// self-hosted ones: a desk that lowers it to 0 suppresses CAPACITY
		// FULL (k.Ceiling stays 0 for the watch) and holds the subscription
		// side to 0, while the self-hosted ceiling stands beside it.
		derived.Ceiling = min(k.Ceiling, subDerivedCeiling) + selfHostedCeiling
	}
	var u *UsageView
	if len(errs) > 0 {
		u = &UsageView{Ceiling: derived.Ceiling, Error: strings.Join(errs, "; ")}
	}
	return derived, u
}

// resolveUsage returns the derived Capacity and usageView for k, reusing a
// fresh usage.json or running capacity.usage.command and saving usage.json.
// In the agent sandbox (where home credentials and external domains are
// closed), a recent usage.json written by the host's watch within 2 × every
// (before resetsAt) is kept when the command cannot run inside the sandbox.
func (a *app) resolveUsage(ctx context.Context, k config.Capacity) (config.Capacity, *UsageView) {
	return a.resolveUsageForStart(ctx, k, ForWorker)
}

func (a *app) resolveUsageForStart(ctx context.Context, k config.Capacity, kind StartFor) (config.Capacity, *UsageView) {
	if !k.Usage.Enabled() {
		return k, nil
	}
	r, err := a.resolveReading(ctx, usageFile, k.Usage)
	if err != nil {
		return ErrorUsageView(k, err)
	}
	return DeriveUsageViewForStart(r, k, kind, a.now)
}

func (a *app) resolveReading(ctx context.Context, file string, u config.Usage) (UsageReading, error) {
	return a.resolveReadingAt(ctx, file, u, nil)
}

func (a *app) resolveReadingAt(ctx context.Context, file string, u config.Usage, completed func() time.Time) (UsageReading, error) {
	if err := ctx.Err(); err != nil {
		return UsageReading{}, err
	}
	started := clk.Now()
	now := a.now
	if now.IsZero() {
		now = started
	}
	var cached UsageReading
	var haveCached bool
	if a.store != nil {
		if found, err := a.store.ReadFile(file, &cached); err == nil && found && !cached.At.IsZero() && ValidateReadingAt(cached, u, now) == nil {
			if !a.useCodexRolloutUsage(u, cached) && UsageFresh(cached, u, now) {
				applyQuotaForUsageFile(a.store, file, &cached, now)
				return cached, nil
			}
			haveCached = true
		}
	}
	unlock, err := a.usageRefreshLock(file)
	if err != nil {
		now := a.timeNow()
		if now.IsZero() {
			now = clk.Now()
		}
		return a.cachedUsageReading(file, u, now, err)
	}
	defer unlock()
	// Recheck under the provider lock: another process may have published
	// the reading between our caller deciding to refresh and admission.
	if a.store != nil {
		cached = UsageReading{}
		haveCached = false
		if found, err := a.store.ReadFile(file, &cached); err == nil && found && !cached.At.IsZero() && ValidateReadingAt(cached, u, now) == nil {
			if !a.useCodexRolloutUsage(u, cached) && UsageFresh(cached, u, now) {
				applyQuotaForUsageFile(a.store, file, &cached, now)
				return cached, nil
			}
			haveCached = true
		}
	}
	r, err := a.execUsage(ctx, u)
	now = now.Add(max(0, clk.Since(started)))
	if completed != nil {
		now = completed()
	}
	if !a.now.IsZero() {
		a.now = now
	}
	if err == nil {
		r.At = now.UTC()
		err = ValidateReadingAt(r, u, now)
	}
	if err != nil {
		if slices.Equal(u.Command, config.DefaultCodexUsageCommand) {
			a.retainUsageStop(file, err)
			return a.cachedUsageReading(file, u, now, err)
		}
		if haveCached && ValidateReadingAt(cached, u, now) == nil {
			cached.Fallback = err.Error()
			applyQuotaForUsageFile(a.store, file, &cached, now)
			return cached, nil
		}
		return UsageReading{}, err
	}
	if a.store != nil {
		_ = a.store.WriteFile(file, r)
	}
	applyQuotaForUsageFile(a.store, file, &r, now)
	return r, nil
}

// retainUsageStop replaces an older good reading with an explicit stop. An
// ordinary failure cannot reopen capacity until a successful read clears it.
func (a *app) retainUsageStop(file string, err error) {
	if a.store != nil && codex.QuotaStop(err) {
		_ = a.store.WriteFile(file, UsageReading{Stop: err.Error()})
	}
}

// cachedUsageReading preserves a valid last good reading, including its limits.
// Built-in Codex read/validation failures without one use the configured ceiling;
// this in-memory fallback is never saved as a successful observation.
func (a *app) cachedUsageReading(file string, u config.Usage, now time.Time, cause error) (UsageReading, error) {
	var r UsageReading
	found := false
	if a.store != nil {
		var err error
		found, err = a.store.ReadFile(file, &r)
		if err != nil && cause == nil {
			cause = err
		}
	}
	if found && r.Stop != "" {
		return UsageReading{}, codex.UsageStoppedError(r.Stop)
	}
	if codex.QuotaStop(cause) {
		return UsageReading{}, cause
	}
	if found {
		err := ValidateReadingAt(r, u, now)
		// A known window still constrains starts until its reset even if the
		// observation is old. Keep its original timestamp for the warning.
		if err != nil && slices.Equal(u.Command, config.DefaultCodexUsageCommand) && !r.At.After(now) {
			check := r
			check.At = now
			if ValidateReadingAt(check, u, now) == nil {
				if cause == nil {
					cause = err
				}
				err = nil
			}
		}
		if err == nil {
			if cause != nil {
				r.Fallback = keptReadingFallback(u, cause.Error())
			}
			applyQuotaForUsageFile(a.store, file, &r, now)
			return r, nil
		} else if cause == nil || !slices.Equal(u.Command, config.DefaultCodexUsageCommand) {
			cause = err
		}
	}
	if cause == nil {
		cause = errors.New("usage not read")
	}
	if !slices.Equal(u.Command, config.DefaultCodexUsageCommand) {
		return UsageReading{}, cause
	}
	r = UsageReading{Fallback: configuredCeilingFallback(cause.Error())}
	applyQuotaForUsageFile(a.store, file, &r, now)
	return r, nil
}

func keptReadingFallback(u config.Usage, msg string) string {
	if slices.Equal(u.Command, config.DefaultCodexUsageCommand) && !codex.QuotaStop(errors.New(msg)) {
		return UsageUnreadable + "kept last good reading: " + msg
	}
	return msg
}

func configuredCeilingFallback(msg string) string {
	return UsageUnreadable + "using configured ceiling: " + msg
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
		return u.HighBlock(a.now)
	}
	noReading := u.Error != "" || (u.At.IsZero() && strings.HasPrefix(u.Fallback, UsageUnreadable))
	if noReading && a.store != nil {
		var l usageHighLedger
		if found, err := a.store.ReadFile(usageHighFile, &l); err == nil && found &&
			l.Episode != "" && !l.ResetsAt.IsZero() && a.now.Before(l.ResetsAt) {
			return fmt.Sprintf("usage high until %s (in %s); usage unreadable (%s)",
				clock(a.now.Local(), l.ResetsAt), dur(l.ResetsAt.Sub(a.now)), truncate(cmp.Or(u.Error, u.Fallback), 80))
		}
	}
	return ""
}

func (a *app) providersUsageHighBlockForAgent(ctx context.Context, k config.Capacity, st *state.State, sessions []*claude.Session, ag *state.Agent) string {
	if ag != nil {
		if st == nil && a.store != nil {
			st, _ = a.store.Peek()
		}
		if st == nil {
			st = &state.State{}
		}
		if sessions == nil {
			sessions, _, _ = a.sessions()
		}
		harness, model := AgentHarnessModel(st, sessions, *ag)
		prov, bucket := ResolveAgentProviderBucket(k, harness, model)
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
	pv := BuildSingleProviderView(prov, p, ProviderAgents{}, ProviderReadingResult{Reading: r, OK: true}, a.now)
	if len(pv.Buckets) > 0 && bucket != "" && !pv.SupplementalBuckets {
		if bv := pv.FindBucket(bucket); bv != nil {
			if bv.High {
				return fmt.Sprintf("provider %s (%s): %s", prov, bv.Name, HighBlockText(bv.FiveHour, pv.HighPercent, a.now))
			}
			return ""
		}
	}
	if !pv.High {
		return ""
	}
	return fmt.Sprintf("provider %s: %s", prov, HighBlockText(pv.FiveHour, pv.HighPercent, a.now))
}

func sameUnderuseWindow(prev, cur time.Time) bool {
	if prev.IsZero() && cur.IsZero() {
		return true
	}
	if prev.IsZero() || cur.IsZero() {
		return false
	}
	diff := cur.Sub(prev)
	if diff < 0 {
		diff = -diff
	}
	return diff <= episodeGap
}

func (w *watcher) checkUsageUnderused(condKey, file, target string, sd *UsageWindow, u config.Usage) {
	underused := WindowUnderused(sd, u, w.now)
	if !underused {
		w.check(condKey, false, "")
		if w.chores && w.store != nil && sd != nil && !sd.ResetsAt.IsZero() {
			var l usageUnderusedLedger
			if found, err := w.store.ReadFile(file, &l); err == nil && found && !l.ReportedAt.IsZero() && !sameUnderuseWindow(l.ResetsAt, sd.ResetsAt) {
				_ = w.store.WriteFile(file, usageUnderusedLedger{})
			}
		}
		return
	}
	fig := UnderusedFigureText(sd, u, w.now)
	line := "USAGE UNDERUSED: " + fig + "; P1 finding filed"
	if target != "" {
		line = fmt.Sprintf("USAGE UNDERUSED (%s): %s; P1 finding filed", target, fig)
	}
	w.check(condKey, true, "%s", line)
	if !w.chores || w.store == nil {
		return
	}
	var l usageUnderusedLedger
	_, _ = w.store.ReadFile(file, &l)
	if !l.ReportedAt.IsZero() && sameUnderuseWindow(l.ResetsAt, sd.ResetsAt) {
		return
	}
	l = usageUnderusedLedger{ResetsAt: sd.ResetsAt, ReportedAt: w.now.UTC()}
	_ = w.store.WriteFile(file, l)
	subject := cmp.Or(target, "subscription")
	winKey := w.now.UTC().Format(time.RFC3339)
	if !sd.ResetsAt.IsZero() {
		winKey = sd.ResetsAt.UTC().Format(time.RFC3339)
	}
	report := fmt.Sprintf("P1: subscription budget for %s is heavily underused: %s. File a P1 issue and ensure starts use this subscription budget before reset.",
		subject, fig)
	detail := fmt.Sprintf("[%s %s] P1 subscription budget underused (%s)%s%s",
		condKey, winKey, subject, loop.FileReportSeparator, report)
	if err := w.store.Record(state.Event{At: w.now, By: watchParty, Verb: loop.LoopFileVerb, Detail: detail}); err != nil {
		w.fail(condKey+"-guide", "cannot retain usage underuse finding for guide: %v", err)
	}
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
	var cached UsageReading
	if found, err := w.store.ReadFile(usageFile, &cached); err == nil && found &&
		UsageFresh(cached, k.Usage, now) && ValidateReadingAt(cached, k.Usage, now) == nil {
		w.mu.Lock()
		w.usageErr = ""
		w.mu.Unlock()
		return false
	}
	w.mu.Lock()
	last := w.lastUsage
	w.mu.Unlock()
	retry := min(k.Usage.EffectiveEvery(), w.usageRetryFloor())
	return last.IsZero() || now.Sub(last) >= retry
}

func (w *watcher) providerUsageNeedsRefresh(name string, u config.Usage, now time.Time) bool {
	var cached UsageReading
	if found, err := w.store.ReadFile(usageProviderFile(name), &cached); err == nil && found &&
		!w.useCodexRolloutUsage(u, cached) &&
		UsageFresh(cached, u, now) && ValidateReadingAt(cached, u, now) == nil {
		w.mu.Lock()
		delete(w.providerUsageErr, name)
		w.mu.Unlock()
		return false
	}
	w.mu.Lock()
	last := w.providerLastUsage[name]
	w.mu.Unlock()
	retry := min(u.EffectiveEvery(), w.usageRetryFloor())
	return last.IsZero() || now.Sub(last) >= retry
}

func (w *watcher) usageRetryFloor() time.Duration {
	if w.cfg != nil {
		return max(w.cfg.Watch.Interval.Duration, time.Second)
	}
	return time.Second
}

// Providers are read sequentially; allow each its configured command deadline.
func usageRefreshTimeout(k config.Capacity) time.Duration {
	var timeout time.Duration
	for _, p := range k.Providers {
		if p.Enabled() {
			timeout += p.EffectiveTimeout()
		}
	}
	return max(timeout, k.Usage.EffectiveTimeout())
}

func (w *watcher) usageNow() time.Time {
	if w.clock != nil || w.polls.Load() > 0 || w.now.IsZero() {
		return w.clk().Now()
	}
	return w.now
}

// execUsageNow gives each provider its own observation time, then validates at
// completion. A refresh can spend minutes on earlier providers; its scheduling
// time must not make a newly written Codex event appear to be in the future.
func (w *watcher) execUsageNow(ctx context.Context, file string, u config.Usage) (UsageReading, error) {
	started := w.clk().Now()
	obs := w.usageNow()
	reader := w.snapshotAtNow()
	reader.now = obs
	return reader.resolveReadingAt(ctx, file, u, func() time.Time {
		if w.clock != nil || w.polls.Load() > 0 || w.now.IsZero() {
			return w.clk().Now()
		}
		return obs.Add(max(0, w.clk().Now().Sub(started)))
	})
}

// refreshUsage stamps every provider read with its own observation time: a
// slow earlier provider must not age the schedule of the next one.
func (w *watcher) refreshUsage(ctx context.Context, k config.Capacity) {
	if len(k.Providers) > 0 {
		for _, name := range k.ProviderNames() {
			now := w.usageNow()
			p := k.Providers[name]
			if !p.Enabled() || !w.providerUsageNeedsRefresh(name, p.Usage, now) {
				continue
			}
			r, err := w.execUsageNow(ctx, usageProviderFile(name), p.Usage)
			if err != nil && isProbeInProgress(err.Error()) || isProbeInProgress(r.Fallback) {
				continue
			}
			w.mu.Lock()
			if w.providerLastUsage == nil {
				w.providerLastUsage = map[string]time.Time{}
			}
			w.providerLastUsage[name] = now
			if w.providerUsageErr == nil {
				w.providerUsageErr = map[string]string{}
			}
			if err != nil {
				w.providerUsageErr[name] = err.Error()
				w.mu.Unlock()
				w.retainUsageStop(usageProviderFile(name), err)
				continue
			}
			w.providerUsageErr[name] = r.Fallback
			w.mu.Unlock()
		}
		return
	}
	now := w.usageNow()
	r, err := w.execUsageNow(ctx, usageFile, k.Usage)
	if err != nil && isProbeInProgress(err.Error()) || isProbeInProgress(r.Fallback) {
		return
	}
	w.mu.Lock()
	w.lastUsage = now
	if err != nil {
		w.usageErr = err.Error()
		w.mu.Unlock()
		w.retainUsageStop(usageFile, err)
		return
	}
	w.usageErr = r.Fallback
	w.mu.Unlock()
}

func (w *watcher) currentUsage(k config.Capacity, now time.Time) (config.Capacity, *UsageView) {
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
	var cause error
	if uerr != "" {
		cause = errors.New(uerr)
	}
	r, err := w.cachedUsageReading(usageFile, k.Usage, now, cause)
	if err != nil {
		if uerr == "" && !slices.Equal(k.Usage.Command, config.DefaultCodexUsageCommand) {
			return k, nil
		}
		return ErrorUsageView(k, err)
	}
	return DeriveUsageView(r, k, now)
}

// reportUsageFallback keeps unreadability visible even when starts can continue.
// The keyed report feeds the guide's existing durable problem queue.
func (w *watcher) reportUsageFallback(key, provider, failure string) {
	if failure == "" || isProbeInProgress(failure) {
		w.clear(key)
		return
	}
	label := "USAGE"
	if provider != "" {
		label += " " + provider
	}
	line := w.fail(key, "%s unreadable: %s", label, failure)
	if line != "" && w.chores && w.store != nil {
		detail := fmt.Sprintf("[%s %s] usage reader failure%s%s", key, w.now.UTC().Format(time.RFC3339Nano), loop.FileReportSeparator, line)
		if err := w.store.Record(state.Event{At: w.now, By: watchParty, Verb: loop.LoopFileVerb, Detail: detail}); err != nil {
			w.fail(key+"-guide", "cannot retain usage failure for guide: %v", err)
		}
	}
}

// watchUsage refreshes subscription usage when due, prints USAGE lines, tracks
// USAGE HIGH / ENDED USAGE HIGH and tells live busy workers once per USAGE HIGH
// episode to finish their step and park.
func (w *watcher) watchUsage(ctx context.Context, st *state.State, sessions []*claude.Session) *UsageView {
	k := w.liveCapacity()
	if len(k.Providers) > 0 {
		return w.watchProvidersUsage(ctx, k, st, sessions)
	}
	if !k.Usage.Enabled() {
		w.clear("usage-error")
		w.clear("usage-high")
		w.clear("usage-underused")
		w.clearUsageHighEpisode(usageHighFile)
		return nil
	}
	if !w.chores {
		_, u := w.resolveUsage(ctx, k)
		if u == nil {
			return nil
		}
		if u.Error != "" {
			w.reportUsageFallback("usage-error", "", u.Error)
			return u
		}
		w.reportUsageFallback("usage-error", "", u.Fallback)
		w.emitNow("usage", "USAGE: %s", u.WatchLine(w.now))
		w.check("usage-high", u.High, "%s", u.HighWatchLine(w.now))
		w.checkUsageUnderused("usage-underused", usageUnderusedFile, "", u.SevenDay, k.Usage)
		return u
	}
	now := w.usageNow()
	w.now = now
	if w.usageNeedsRefresh(k, now) {
		w.inFlight(ctx, k.Usage.EffectiveTimeout(), &w.usageBusy, func(ctx context.Context) {
			w.refreshUsage(ctx, k)
		})
		now = w.usageNow()
		w.now = now
	}
	_, u := w.currentUsage(k, w.now)
	if u == nil {
		return nil
	}
	if u.Error != "" {
		w.reportUsageFallback("usage-error", "", u.Error)
		return u
	}
	w.reportUsageFallback("usage-error", "", u.Fallback)
	if rep := k.Usage.ReportEvery.Duration; rep > 0 {
		w.mu.Lock()
		due := w.usageReported.IsZero() || w.now.Sub(w.usageReported) >= rep
		if due {
			w.usageReported = w.now
			w.dirty = true
		}
		w.mu.Unlock()
		if due {
			w.emitNow("usage", "USAGE: %s", u.WatchLine(w.now))
		}
	}
	w.check("usage-high", u.High, "%s", u.HighWatchLine(w.now))
	w.checkUsageUnderused("usage-underused", usageUnderusedFile, "", u.SevenDay, k.Usage)
	if !u.High {
		w.clearUsageHighEpisode(usageHighFile)
		return u
	}
	if st.Stop == nil {
		w.tellBusyWorkersUsageHigh(ctx, st, sessions, u, usageHighFile, nil)
	}
	return u
}

func (w *watcher) watchProvidersUsage(ctx context.Context, k config.Capacity, st *state.State, sessions []*claude.Session) *UsageView {
	w.syncAgyQuotaFromState(st, w.now)
	readings := map[string]ProviderReadingResult{}
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
				readings[name] = ProviderReadingResult{Err: err}
				continue
			}
			readings[name] = ProviderReadingResult{Reading: r, OK: true}
		}
	} else {
		now := w.usageNow()
		w.now = now
		if w.usageNeedsRefresh(k, now) {
			w.inFlight(ctx, usageRefreshTimeout(k), &w.usageBusy, func(ctx context.Context) {
				w.refreshUsage(ctx, k)
			})
			now = w.usageNow()
			w.now = now
		}
		for _, name := range k.ProviderNames() {
			p := k.Providers[name]
			if !p.Enabled() {
				continue
			}
			w.mu.Lock()
			uerr := w.providerUsageErr[name]
			w.mu.Unlock()
			var cause error
			if uerr != "" {
				cause = errors.New(uerr)
			}
			r, err := w.cachedUsageReading(usageProviderFile(name), p.Usage, w.now, cause)
			readings[name] = ProviderReadingResult{Reading: r, Err: err, OK: err == nil}
		}
	}

	c := countAgents(st, sessions, w.now, w.cfg)
	pa := countProviderAgents(st, sessions, k, w.now, w.cfg)
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
	var h *Headroom
	if w.readHeadroom != nil {
		h = w.readHeadroom(ctx, swap)
	} else {
		h = w.app.readHeadroom(ctx, k, swap)
	}
	v := capacityStopped(BuildProvidersCapacity(c, pa, k, readings, h, false, nil, ForWorker, w.now), w.app, st)

	var parts []string
	allHigh := len(k.Providers) > 0
	anyUnderused := false
	for _, name := range k.ProviderNames() {
		p := k.Providers[name]
		if !p.Enabled() {
			allHigh = false
		}
		pv := v.Providers[name]
		if pv == nil {
			continue
		}
		anyUnderused = anyUnderused || pv.Underused
		if pv.Kind == config.KindSelfHosted {
			// No subscription behind it: no USAGE HIGH, nothing to tell its
			// workers to park for; its own ceiling is the only guard.
			allHigh = false
			if pv.ServingModeError != "" {
				w.fail("serve-mode-error-"+name, "SERVE MODE %s unreadable: %s", name, pv.ServingModeError)
			} else {
				w.clear("serve-mode-error-" + name)
			}
			parts = append(parts, name+": "+pv.WatchSummary(w.now))
			continue
		}
		if pv.Error != "" {
			w.reportUsageFallback("usage-error-"+name, name, pv.Error)
		} else {
			w.reportUsageFallback("usage-error-"+name, name, pv.Fallback)
		}
		parts = append(parts, name+": "+pv.WatchSummary(w.now))
		if len(pv.Buckets) > 0 && !pv.SupplementalBuckets {
			for _, bName := range pv.BucketOrder {
				bv := pv.Buckets[bName]
				if bv == nil {
					continue
				}
				target := name + "/" + bName
				highLine := fmt.Sprintf("USAGE HIGH (%s): %s; new starts and resumes on %s wait until reset, telling busy workers to finish their step and park",
					target, HighFigureText(bv.FiveHour, pv.HighPercent, w.now), target)
				w.check("usage-high-"+name+"-"+bName, bv.High, "%s", highLine)
				w.checkUsageUnderused("usage-underused-"+name+"-"+bName, usageUnderusedTargetFile(target), target, bv.SevenDay, p.Usage)
				ledgerFile := usageHighBucketFile(name, bName)
				if !bv.High {
					allHigh = false
					w.clearUsageHighEpisode(ledgerFile)
				} else if st.Stop == nil {
					u := &UsageView{FiveHour: bv.FiveHour, High: true, HighPercent: pv.HighPercent}
					w.tellBusyWorkersUsageHigh(ctx, st, sessions, u, ledgerFile, func(ag state.Agent) bool {
						return slices.Contains(bv.Busy, ag.Name)
					})
				}
			}
			continue
		}
		highLine := fmt.Sprintf("USAGE HIGH (%s): %s; new starts and resumes on %s wait until reset, telling busy workers to finish their step and park",
			name, HighFigureText(pv.FiveHour, pv.HighPercent, w.now), name)
		w.check("usage-high-"+name, pv.High, "%s", highLine)
		w.checkUsageUnderused("usage-underused-"+name, usageUnderusedTargetFile(name), name, pv.SevenDay, p.Usage)
		if !pv.High {
			allHigh = false
			w.clearUsageHighEpisode(usageHighProviderFile(name))
		} else if st.Stop == nil {
			u := &UsageView{FiveHour: pv.FiveHour, High: true, HighPercent: pv.HighPercent}
			w.tellBusyWorkersUsageHigh(ctx, st, sessions, u, usageHighProviderFile(name), func(ag state.Agent) bool {
				return slices.Contains(pv.Busy, ag.Name)
			})
		}
	}
	if v.Choice != nil {
		parts = append(parts, "choice: "+v.Choice.Summary())
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
	return &UsageView{Ceiling: v.Ceiling, High: allHigh, Underused: anyUnderused}
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

func (w *watcher) tellBusyWorkersUsageHigh(ctx context.Context, st *state.State, sessions []*claude.Session, u *UsageView, ledgerFile string, match func(state.Agent) bool) {
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
		u.HighFigure(w.now))
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
		_ = w.store.Log(event(watchParty, verbUsageHigh, "%s: told to finish its step and park (%s)", ag.Name, u.HighFigure(w.now)))
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
	guide := st.GuideRole()
	for _, ag := range st.Agents {
		p := ag.Party
		if roleRun(st, p) || guide.Relay.Open(now) && guide.Relay.To.Is(p) {
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
