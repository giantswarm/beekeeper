package agy

import (
	"bufio"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// QuotaError records a quota 429 / RESOURCE_EXHAUSTED failure reported by agy
// on stderr or in its stream-json output.
type QuotaError struct {
	ShortError string        `json:"shortError"`
	Status     string        `json:"status,omitempty"`
	ErrorCode  int           `json:"errorCode,omitempty"`
	ResetsIn   time.Duration `json:"resetsIn,omitempty"`
	At         time.Time     `json:"at,omitzero"`
	Model      string        `json:"model,omitempty"`
}

func (e *QuotaError) Error() string {
	if e == nil {
		return ""
	}
	msg := strings.TrimSpace(e.ShortError)
	if msg == "" {
		msg = "quota exhausted"
	}
	switch {
	case e.ErrorCode > 0 && e.Status != "":
		return fmt.Sprintf("agy quota %d (%s): %s", e.ErrorCode, e.Status, msg)
	case e.ErrorCode > 0:
		return fmt.Sprintf("agy quota %d: %s", e.ErrorCode, msg)
	case e.Status != "":
		return fmt.Sprintf("agy quota (%s): %s", e.Status, msg)
	default:
		return "agy quota: " + msg
	}
}

type agyErrorPayload struct {
	ShortError string `json:"short_error"`
	ErrorCode  int    `json:"error_code"`
	Status     string `json:"status"`
	Retryable  bool   `json:"retryable"`
}

// drainWait bounds how long Run waits for agy's output pipes to reach EOF
// after it exited or was killed.
const drainWait = 5 * time.Second

var resetsInPattern = regexp.MustCompile(`(?i)\bresets in\s+([0-9hms.]+)`)

func parseResetsIn(s string) time.Duration {
	m := resetsInPattern.FindStringSubmatch(s)
	if len(m) < 2 {
		return 0
	}
	d, err := time.ParseDuration(strings.ToLower(m[1]))
	if err != nil || d <= 0 {
		return 0
	}
	return d
}

func isQuotaErrorText(s string) bool {
	lower := strings.ToLower(s)
	return strings.Contains(lower, "individual quota reached") ||
		strings.Contains(lower, "resource_exhausted") ||
		(strings.Contains(lower, "quota") && strings.Contains(lower, "429"))
}

// ParseQuotaError parses an agy stderr or stream error line for a quota 429 /
// RESOURCE_EXHAUSTED error, returning nil when line is not a quota error.
func ParseQuotaError(line string) *QuotaError {
	s := strings.TrimSpace(line)
	if s == "" {
		return nil
	}
	if _, rest, ok := strings.Cut(s, "AGY_ERROR:"); ok {
		rest = strings.TrimSpace(rest)
		var p agyErrorPayload
		if json.Unmarshal([]byte(rest), &p) == nil {
			if p.ErrorCode == 429 || strings.EqualFold(p.Status, "RESOURCE_EXHAUSTED") || isQuotaErrorText(p.ShortError) {
				short := strings.TrimSpace(p.ShortError)
				if short == "" {
					short = rest
				}
				return &QuotaError{
					ShortError: short,
					Status:     p.Status,
					ErrorCode:  p.ErrorCode,
					ResetsIn:   parseResetsIn(short),
				}
			}
		}
	}
	if isQuotaErrorText(s) {
		short := strings.TrimSpace(strings.TrimPrefix(s, "error:"))
		return &QuotaError{
			ShortError: short,
			ErrorCode:  429,
			Status:     "RESOURCE_EXHAUSTED",
			ResetsIn:   parseResetsIn(short),
		}
	}
	return nil
}

func mergeQuotaError(dst, src *QuotaError) *QuotaError {
	if src == nil {
		return dst
	}
	if dst == nil {
		cp := *src
		return &cp
	}
	if src.ShortError != "" {
		dst.ShortError = src.ShortError
	}
	if src.Status != "" {
		dst.Status = src.Status
	}
	if src.ErrorCode != 0 {
		dst.ErrorCode = src.ErrorCode
	}
	if src.ResetsIn > 0 {
		dst.ResetsIn = src.ResetsIn
	}
	if !src.At.IsZero() {
		dst.At = src.At
	}
	if src.Model != "" {
		dst.Model = src.Model
	}
	return dst
}

func argvModel(argv []string) string {
	for i := 0; i < len(argv); i++ {
		if argv[i] == flagModel && i+1 < len(argv) {
			return strings.TrimSpace(argv[i+1])
		}
		if v, ok := strings.CutPrefix(argv[i], flagModel+"="); ok {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func quotaPathForInbox(inboxOrLog string) string {
	for _, suf := range []string{".in", ".log", ".conv"} {
		if base, ok := strings.CutSuffix(inboxOrLog, suf); ok {
			return base + ".quota.json"
		}
	}
	if strings.HasSuffix(inboxOrLog, ".quota.json") {
		return inboxOrLog
	}
	return inboxOrLog + ".quota.json"
}

// WriteQuotaError persists qerr beside inbox (<id>.quota.json) so the starter,
// doctor and supervisor can see that the session stopped on a quota error.
func WriteQuotaError(inbox string, qerr *QuotaError) error {
	if qerr == nil || strings.TrimSpace(inbox) == "" {
		return nil
	}
	cp := *qerr
	if cp.At.IsZero() {
		cp.At = time.Now().UTC()
	}
	p := quotaPathForInbox(inbox)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(cp)
	if err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// ReadQuotaError reads the persisted QuotaError for the agy agent id under
// stateDir, or returns nil when none exists.
func ReadQuotaError(stateDir, id string) *QuotaError {
	c := cleanID(id)
	if c == "" {
		return nil
	}
	return ReadQuotaErrorForInbox(QuotaPath(stateDir, c))
}

// ReadQuotaErrorForInbox reads the persisted QuotaError beside an inbox or
// stream log path, or returns nil when none exists.
func ReadQuotaErrorForInbox(inboxOrLog string) *QuotaError {
	if strings.TrimSpace(inboxOrLog) == "" {
		return nil
	}
	b, err := os.ReadFile(quotaPathForInbox(inboxOrLog)) //nolint:gosec // quota marker under the state folder
	if err != nil || len(b) == 0 {
		return nil
	}
	var qerr QuotaError
	if json.Unmarshal(b, &qerr) != nil {
		return nil
	}
	if qerr.ShortError == "" && qerr.ErrorCode == 0 && qerr.Status == "" {
		return nil
	}
	return &qerr
}

// streamEvent is the subset of an agy stream-json stdout event that the
// runner inspects to track turn boundaries and conversation ids.
type streamEvent struct {
	Event          string `json:"event"`
	ConversationID string `json:"conversation_id"`
	Error          string `json:"error"`
	StepUpdate     *struct {
		ConversationID string `json:"conversation_id"`
		StepType       string `json:"step_type"`
		Status         string `json:"status"`
		Error          string `json:"error"`
	} `json:"step_update"`
	Result *struct {
		ConversationID string `json:"conversation_id"`
		Status         string `json:"status"`
		Error          string `json:"error"`
	} `json:"result"`
}

func (ev streamEvent) quotaError() *QuotaError {
	for _, s := range []string{
		ev.Error,
		func() string {
			if ev.StepUpdate != nil {
				return ev.StepUpdate.Error
			}
			return ""
		}(),
		func() string {
			if ev.Result != nil {
				return ev.Result.Error
			}
			return ""
		}(),
	} {
		if qerr := ParseQuotaError(s); qerr != nil {
			return qerr
		}
	}
	return nil
}

// linkConversation records the mapping between convID and inbox so that
// beekeeper hook preinvocation resolves convID to the session's inbox and
// session discovery resolves the agent start id to convID.
func linkConversation(inbox, convID string) {
	c := cleanID(convID)
	if c == "" {
		return
	}
	stateDir := filepath.Dir(filepath.Dir(inbox))
	link := ConvLinkPath(stateDir, c)
	_ = os.MkdirAll(filepath.Dir(link), 0o700)
	tmp := link + ".tmp"
	if os.WriteFile(tmp, []byte(inbox+"\n"), 0o600) == nil {
		_ = os.Rename(tmp, link)
	}
	if id := cleanID(strings.TrimSuffix(filepath.Base(inbox), ".in")); id != "" {
		convFile := AgentConvPath(stateDir, id)
		ctmp := convFile + ".tmp"
		if os.WriteFile(ctmp, []byte(c+"\n"), 0o600) == nil {
			_ = os.Rename(ctmp, convFile)
		}
	}
}

// Run executes argv (an `agy --input-format stream-json --output-format
// stream-json` command) with its stream output appended to logPath and its
// turns fed from the persistent inbox at inbox.
//
// While agy is idle (after its init event or after a turn's result event),
// Run pops the next queued message from inbox into Inflight and writes it as
// a stream-json user event to agy's stdin. While a turn is running, Run does
// not write to agy's stdin: queued messages stay in the persistent queue for
// the PreInvocation hook to drain into Injected before the next model call
// inside the running turn, and each user_input step_update event confirms
// the corresponding Inflight or Injected message. Any Injected messages that
// were not confirmed before the turn ends (e.g. a hook killed or timed out
// on the final model call) are restored to Messages on result and sent on
// stdin as the next turn.
//
// When agy reports a quota 429 / RESOURCE_EXHAUSTED error on stderr or in a
// stream event (including mid-turn when agy stays alive waiting on stdin),
// Run records the quota error beside inbox, terminates the agy process and
// returns *QuotaError.
func Run(ctx context.Context, inbox, logPath string, argv []string, stderr io.Writer) error {
	if len(argv) == 0 {
		return errors.New("agy-run: missing command")
	}
	if err := MakeInbox(inbox); err != nil {
		return err
	}
	_ = os.Remove(quotaPathForInbox(inbox))
	if err := restoreInflight(inbox); err != nil {
		return err
	}
	fifo, err := os.OpenFile(inbox, os.O_RDWR, 0) //nolint:gosec // the agent's inbox under the state folder
	if err != nil {
		return err
	}
	defer func() { _ = fifo.Close() }()
	if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		return err
	}
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600) //nolint:gosec // the agent's stream log under the state folder
	if err != nil {
		return err
	}
	defer func() { _ = logFile.Close() }()

	model := argvModel(argv)
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...) //nolint:gosec // argv from the unit beekeeper started
	stdinPipe, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	defer func() { _ = stdinPipe.Close() }()
	// Own the output pipes so Wait can reap the process independently of
	// stdout EOF without closing unread final events or quota diagnostics.
	stdoutPipe, stdoutWriter, err := os.Pipe()
	if err != nil {
		return err
	}
	defer func() { _ = stdoutPipe.Close() }()
	defer func() { _ = stdoutWriter.Close() }()
	stderrPipe, stderrWriter, err := os.Pipe()
	if err != nil {
		return err
	}
	defer func() { _ = stderrPipe.Close() }()
	defer func() { _ = stderrWriter.Close() }()
	cmd.Stdout, cmd.Stderr = stdoutWriter, stderrWriter
	if err := cmd.Start(); err != nil {
		return err
	}
	_ = stdoutWriter.Close()
	_ = stderrWriter.Close()
	processExited := make(chan error, 1)
	go func() { processExited <- cmd.Wait() }()

	var (
		quotaMu   sync.Mutex
		quotaErr  *QuotaError
		quotaCh   = make(chan struct{}, 1)
		quotaOnce sync.Once
	)
	signalQuota := func() {
		quotaOnce.Do(func() {
			close(quotaCh)
		})
	}
	recordQuota := func(qerr *QuotaError, immediate bool) {
		if qerr == nil {
			return
		}
		if qerr.At.IsZero() {
			qerr.At = time.Now().UTC()
		}
		qerr.Model = cmp.Or(qerr.Model, model)
		quotaMu.Lock()
		quotaErr = mergeQuotaError(quotaErr, qerr)
		cur := *quotaErr
		quotaMu.Unlock()
		_ = WriteQuotaError(inbox, &cur)
		if immediate {
			signalQuota()
		} else {
			time.AfterFunc(50*time.Millisecond, signalQuota)
		}
	}
	getQuotaErr := func() *QuotaError {
		quotaMu.Lock()
		defer quotaMu.Unlock()
		if quotaErr == nil {
			return nil
		}
		cp := *quotaErr
		return &cp
	}

	stderrDone := make(chan struct{})
	go func() {
		defer close(stderrDone)
		r := bufio.NewReaderSize(stderrPipe, 64<<10)
		for {
			line, rerr := r.ReadBytes('\n')
			if len(line) > 0 {
				if stderr != nil {
					_, _ = stderr.Write(line)
				}
				s := string(line)
				if qerr := ParseQuotaError(s); qerr != nil {
					recordQuota(qerr, strings.Contains(s, "AGY_ERROR:"))
				}
			}
			if rerr != nil {
				if getQuotaErr() != nil {
					signalQuota()
				}
				return
			}
		}
	}()

	events := make(chan streamEvent, 16)
	go func() {
		defer close(events)
		r := bufio.NewReaderSize(stdoutPipe, 64<<10)
		for {
			line, rerr := r.ReadBytes('\n')
			if len(line) > 0 {
				var ev streamEvent
				if json.Unmarshal(line, &ev) == nil && ev.Event != "" {
					if ev.Event == eventInit && ev.ConversationID != "" {
						linkConversation(inbox, ev.ConversationID)
					}
					_, _ = logFile.Write(line)
					if qerr := ev.quotaError(); qerr != nil {
						recordQuota(qerr, true)
					}
					events <- ev
				} else {
					_, _ = logFile.Write(line)
					if qerr := ParseQuotaError(string(line)); qerr != nil {
						recordQuota(qerr, true)
					}
				}
			}
			if rerr != nil {
				return
			}
		}
	}()

	wakeCh := make(chan struct{}, 1)
	go func() {
		buf := make([]byte, 256)
		for {
			n, rerr := fifo.Read(buf)
			if n > 0 {
				select {
				case wakeCh <- struct{}{}:
				default:
				}
			}
			if rerr != nil {
				return
			}
		}
	}()

	ready, busy := false, false
	feedIfIdle := func() {
		if !ready || busy {
			return
		}
		msg, ok, perr := popInflight(inbox)
		if perr != nil || !ok {
			return
		}
		line, lerr := streamLine(msg)
		if lerr != nil {
			return
		}
		if _, werr := stdinPipe.Write(line); werr != nil {
			return
		}
		busy = true
	}

	handleEvent := func(ev streamEvent, feed bool) {
		switch ev.Event {
		case eventInit:
			ready, busy = true, false
			if feed {
				feedIfIdle()
			}
		case "step_update":
			if ev.StepUpdate != nil && ev.StepUpdate.StepType == "user_input" {
				_ = Confirm(inbox)
			}
		case "result":
			_ = finishTurn(inbox)
			busy = false
			if feed {
				feedIfIdle()
			}
		}
	}

	// drain preserves final acknowledgements and diagnostics after process
	// exit or cancellation. A descendant retaining an output pipe cannot
	// postpone starting this deadline by keeping stdout open.
	drain := func(events <-chan streamEvent, stderrDone <-chan struct{}) {
		timer := time.NewTimer(drainWait)
		defer timer.Stop()
		for events != nil || stderrDone != nil {
			select {
			case ev, ok := <-events:
				if !ok {
					events = nil
				} else {
					handleEvent(ev, false)
				}
			case <-stderrDone:
				stderrDone = nil
			case <-timer.C:
				_ = stdoutPipe.Close()
				_ = stderrPipe.Close()
				if events != nil {
					for ev := range events {
						handleEvent(ev, false)
					}
				}
				if stderrDone != nil {
					<-stderrDone
				}
				return
			}
		}
	}

	for {
		select {
		case <-quotaCh:
			_ = stdinPipe.Close()
			if cmd.Process != nil {
				_ = cmd.Process.Kill()
			}
			drain(events, stderrDone)
			<-processExited
			_ = fifo.Close()
			qerr := getQuotaErr()
			_ = WriteQuotaError(inbox, qerr)
			return qerr
		case err := <-processExited:
			_ = stdinPipe.Close()
			drain(events, stderrDone)
			if qerr := getQuotaErr(); qerr != nil {
				_ = WriteQuotaError(inbox, qerr)
				return qerr
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		case <-ctx.Done():
			_ = stdinPipe.Close()
			_ = cmd.Process.Kill()
			drain(events, stderrDone)
			<-processExited
			if qerr := getQuotaErr(); qerr != nil {
				_ = WriteQuotaError(inbox, qerr)
				return qerr
			}
			return ctx.Err()
		case ev, ok := <-events:
			if !ok {
				events = nil
				ready = false
				_ = stdinPipe.Close()
				continue
			}
			handleEvent(ev, true)
		case <-wakeCh:
			feedIfIdle()
		}
	}
}
