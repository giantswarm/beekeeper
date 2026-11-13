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
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// UsageRequestError is the app-server's error reply to the account usage
// request. Cause says what failed (the endpoint, the HTTP status it answered
// and the error code of its body), never the body itself, which can carry
// account metadata.
type UsageRequestError struct {
	// Cause is "GET <endpoint> answered 401 Unauthorized (token_expired)",
	// or the app-server's own words when the account did not answer with a
	// status ("codex account authentication required to read rate limits").
	Cause string
	// Transient is true for a failure that may pass when the request is
	// sent again: no answer, or an answer of 429 or 5xx.
	Transient bool
}

func (e *UsageRequestError) Error() string {
	if e.Cause == "" {
		return "codex account usage request failed"
	}
	return "codex account usage request failed: " + e.Cause
}

// UsageRequestTransient reports whether err is an account usage request
// that may succeed when sent again.
func UsageRequestTransient(err error) bool {
	var re *UsageRequestError
	return errors.As(err, &re) && re.Transient
}

// jsonRPCInvalidRequest is the error code of a request the app-server
// refuses itself, such as one without a login.
const jsonRPCInvalidRequest = -32600

// maxUsageCause bounds a cause's length: it is shown in one line.
const maxUsageCause = 160

var (
	// unexpectedStatus is the backend client's "GET <url> failed: <status>;
	// content-type=…; body=…".
	unexpectedStatus = regexp.MustCompile(`^([A-Z]+) (\S+) failed: (\d{3})([^;]*);`)
	errorCode        = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)
)

// usageRequestError turns the app-server's error reply into the cause of the
// failure.
func usageRequestError(raw json.RawMessage) *UsageRequestError {
	var reply struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}
	if json.Unmarshal(raw, &reply) != nil {
		return &UsageRequestError{}
	}
	msg := strings.TrimSpace(reply.Message)
	msg = strings.TrimPrefix(msg, "failed to fetch codex rate limits: ")
	if m := unexpectedStatus.FindStringSubmatch(msg); m != nil {
		endpoint, _, _ := strings.Cut(m[2], "?")
		status, _ := strconv.Atoi(m[3])
		cause := m[1] + " " + endpoint + " answered " + strings.TrimSpace(m[3]+" "+strings.TrimSpace(m[4]))
		if _, body, ok := strings.Cut(msg, "; body="); ok {
			if code := bodyErrorCode(body); code != "" {
				cause += " (" + code + ")"
			}
		}
		return &UsageRequestError{Cause: shortCause(cause), Transient: status == 429 || status >= 500}
	}
	// No status: the request went unanswered, or the app-server refused it
	// itself (no login). Its words never carry a body.
	line, _, _ := strings.Cut(msg, "\n")
	line, _, _ = strings.Cut(line, "; body=")
	return &UsageRequestError{Cause: shortCause(line), Transient: reply.Code != jsonRPCInvalidRequest}
}

// bodyErrorCode is the error code of an error body ({"error":{"code":…}},
// {"detail":{"code":…}} or {"code":…}), or "" when it has none that looks
// like a code.
func bodyErrorCode(body string) string {
	type coded struct {
		Code any `json:"code"`
	}
	var doc struct {
		Error  coded `json:"error"`
		Detail coded `json:"detail"`
		Code   any   `json:"code"`
	}
	if json.Unmarshal([]byte(strings.TrimSpace(body)), &doc) != nil {
		return ""
	}
	for _, c := range []any{doc.Error.Code, doc.Detail.Code, doc.Code} {
		if s, ok := c.(string); ok && errorCode.MatchString(s) {
			return s
		}
	}
	return ""
}

func shortCause(s string) string {
	s = strings.TrimSpace(s)
	if len(s) <= maxUsageCause {
		return s
	}
	cut := maxUsageCause
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}

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
				return nil, usageRequestError(msg.Error)
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
