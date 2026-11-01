package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata" // Named reset zones also work on hosts without zoneinfo.
)

const (
	claudeNoPlanWindows = "no plan windows"
	claudeCodeEnv       = "CLAUDECODE"
)

// parseClaudeUsage reads the local command's JSON result without credentials
// or a model turn. Model weeks supplement the aggregate subscription windows.
func parseClaudeUsage(raw []byte, now time.Time) (usageReading, error) {
	var doc struct {
		Result  string `json:"result"`
		IsError bool   `json:"is_error"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return usageReading{}, err
	}
	if doc.IsError {
		return usageReading{}, fmt.Errorf("claude /usage: %s", doc.Result)
	}
	r := usageReading{Buckets: map[string]usageBucket{}}
	for _, line := range strings.Split(doc.Result, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "Current ") {
			continue
		}
		label, value, ok := strings.Cut(line, ": ")
		if !ok {
			return usageReading{}, fmt.Errorf("invalid Claude usage line %q", line)
		}
		w, err := parseClaudeWindow(value, now)
		if err != nil {
			return usageReading{}, fmt.Errorf("invalid Claude usage line %q: %w", line, err)
		}
		switch {
		case label == "Current session":
			if r.FiveHour != nil {
				return usageReading{}, fmt.Errorf("duplicate Claude usage line %q", line)
			}
			r.FiveHour = w
		case label == "Current week (all models)":
			if r.SevenDay != nil {
				return usageReading{}, fmt.Errorf("duplicate Claude usage line %q", line)
			}
			r.SevenDay = w
		case strings.HasPrefix(label, "Current week (") && strings.HasSuffix(label, ")"):
			name := strings.TrimSuffix(strings.TrimPrefix(label, "Current week ("), ")")
			if name == "" || r.Buckets[name].SevenDay != nil {
				return usageReading{}, fmt.Errorf("invalid Claude usage line %q", line)
			}
			r.Buckets[name] = usageBucket{SevenDay: w}
			r.BucketOrder = append(r.BucketOrder, name)
		default:
			return usageReading{}, fmt.Errorf("unknown Claude usage line %q", line)
		}
	}
	if r.FiveHour == nil && r.SevenDay == nil && len(r.Buckets) == 0 {
		lower := strings.ToLower(doc.Result)
		if strings.Contains(lower, "api key") || strings.Contains(lower, "api usage billing") || strings.Contains(lower, "not using a subscription") {
			r.NoPlanWindows = true
			return r, nil
		}
		return usageReading{}, fmt.Errorf("claude /usage has no plan windows: %s", doc.Result)
	}
	if r.FiveHour == nil || r.SevenDay == nil {
		return usageReading{}, errors.New("claude /usage omits Current session or Current week (all models)")
	}
	return r, nil
}

func parseClaudeWindow(value string, now time.Time) (*usageWindow, error) {
	percent, tail, ok := strings.Cut(value, "% used")
	used, err := strconv.ParseFloat(percent, 64)
	if !ok || err != nil || math.IsNaN(used) || math.IsInf(used, 0) || used < 0 || used > 100 {
		return nil, errors.New("invalid used percentage")
	}
	w := &usageWindow{UsedPercent: used}
	tail = strings.TrimSpace(tail)
	if tail == "" && used == 0 {
		return w, nil
	}
	reset, ok := strings.CutPrefix(tail, "· resets ")
	if !ok {
		return nil, errors.New("missing reset")
	}
	stamp, zone, ok := strings.Cut(reset, " (")
	if !ok || !strings.HasSuffix(zone, ")") {
		return nil, errors.New("missing reset timezone")
	}
	loc, err := time.LoadLocation(strings.TrimSuffix(zone, ")"))
	if err != nil {
		return nil, err
	}
	localNow := now.In(loc)
	for _, layout := range []string{"Jan 2, 3:04pm", "Jan 2, 3pm", "3:04pm", "3pm"} {
		t, parseErr := time.ParseInLocation(layout, stamp, loc)
		if parseErr != nil {
			continue
		}
		year, month, day := localNow.Date()
		dated := strings.HasPrefix(layout, "Jan")
		if dated {
			month, day = t.Month(), t.Day()
		}
		resetAt := time.Date(year, month, day, t.Hour(), t.Minute(), 0, 0, loc)
		// Printed resets have minute precision and may lag the rollover.
		// Keep a recent past reset rather than extending the old window.
		if dated {
			if resetAt.Before(now.Add(-183 * 24 * time.Hour)) {
				resetAt = resetAt.AddDate(1, 0, 0)
			} else if previous := resetAt.AddDate(-1, 0, 0); !previous.After(now) && now.Sub(previous) <= 5*time.Minute {
				resetAt = previous
			}
		} else if resetAt.Before(now.Add(-5 * time.Minute)) {
			resetAt = resetAt.AddDate(0, 0, 1)
		} else if previous := resetAt.AddDate(0, 0, -1); !previous.After(now) && now.Sub(previous) <= 5*time.Minute {
			resetAt = previous
		}
		w.ResetsAt = resetAt.UTC()
		return w, nil
	}
	return nil, fmt.Errorf("unparseable reset %q", stamp)
}
