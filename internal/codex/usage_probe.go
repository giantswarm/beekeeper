package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"time"
)

// ProbeUsage fetches account usage without starting a model turn. Codex owns
// authentication; no credential values pass through beekeeper.
func ProbeUsage(ctx context.Context, dir, home string, at time.Time) (UsageReading, error) {
	c := exec.CommandContext(ctx, Comm, "app-server")
	c.Dir = dir
	if home != "" {
		c.Env = append(os.Environ(), "CODEX_HOME="+home)
	}
	c.WaitDelay = time.Second
	return probeUsage(c, at)
}

func probeUsage(c *exec.Cmd, at time.Time) (UsageReading, error) {
	in, err := c.StdinPipe()
	if err != nil {
		return UsageReading{}, err
	}
	defer func() { _ = in.Close() }()
	out, err := c.StdoutPipe()
	if err != nil {
		return UsageReading{}, err
	}
	// stderr can contain account metadata: discard it rather than returning it.
	c.Stderr = io.Discard
	if err = c.Start(); err != nil {
		return UsageReading{}, err
	}
	defer func() { _ = c.Process.Kill(); _ = c.Wait() }()
	sc := bufio.NewScanner(out)
	sc.Buffer(make([]byte, 4096), 1024*1024)
	send := func(s string) error { _, e := io.WriteString(in, s+"\n"); return e }
	read := func(id int) (json.RawMessage, error) {
		for sc.Scan() {
			var msg struct {
				ID     int             `json:"id"`
				Result json.RawMessage `json:"result"`
				Error  json.RawMessage `json:"error"`
			}
			if json.Unmarshal(sc.Bytes(), &msg) != nil {
				return nil, errors.New("invalid codex usage response")
			}
			if msg.ID != id {
				continue
			}
			if !isNullOrEmptyJSON(msg.Error) {
				return nil, errors.New("codex account usage request failed")
			}
			return msg.Result, nil
		}
		if sc.Err() != nil {
			return nil, fmt.Errorf("codex usage response: %w", sc.Err())
		}
		return nil, errors.New("codex usage server closed before reply")
	}
	if err = send(`{"id":1,"method":"initialize","params":{"clientInfo":{"name":"beekeeper","version":"1"}}}`); err != nil {
		return UsageReading{}, err
	}
	if _, err = read(1); err != nil {
		return UsageReading{}, err
	}
	if err = send(`{"method":"initialized"}`); err != nil {
		return UsageReading{}, err
	}
	if err = send(`{"id":2,"method":"account/rateLimits/read"}`); err != nil {
		return UsageReading{}, err
	}
	raw, err := read(2)
	if err != nil {
		return UsageReading{}, err
	}
	return parseProbeUsage(raw, at)
}

var ErrUsageStopped = errors.New("codex account usage stopped")

func parseProbeUsage(raw json.RawMessage, at time.Time) (UsageReading, error) {
	type window struct {
		UsedPercent   *float64 `json:"usedPercent"`
		WindowMinutes *int     `json:"windowDurationMins"`
		ResetsAt      *int64   `json:"resetsAt"`
	}
	var response struct {
		Allowed    *bool `json:"ordinaryUsageAllowed"`
		RateLimits struct {
			Primary              *window `json:"primary"`
			Secondary            *window `json:"secondary"`
			SpendControlReached  bool    `json:"spendControlReached"`
			RateLimitReached     bool    `json:"rateLimitReached"`
			RateLimitReachedType *string `json:"rateLimitReachedType"`
		} `json:"rateLimits"`
	}
	if err := json.Unmarshal(raw, &response); err != nil {
		return UsageReading{}, errors.New("invalid codex account usage")
	}
	r := response.RateLimits
	stopped := response.Allowed != nil && !*response.Allowed
	if stopped || r.SpendControlReached || r.RateLimitReached || (r.RateLimitReachedType != nil && *r.RateLimitReachedType != "") {
		return UsageReading{}, ErrUsageStopped
	}
	convert := func(w *window) any {
		if w == nil {
			return nil
		}
		return map[string]any{"used_percent": w.UsedPercent, "window_minutes": w.WindowMinutes, "resets_at": w.ResetsAt}
	}
	normalized, err := json.Marshal(map[string]any{"primary": convert(r.Primary), "secondary": convert(r.Secondary), "spend_control_reached": r.SpendControlReached || stopped, "rate_limit_reached": r.RateLimitReached, "rate_limit_reached_type": r.RateLimitReachedType})
	if err != nil {
		return UsageReading{}, err
	}
	return ParseRateLimits(normalized, at)
}
