package claude

import (
	"bytes"
	"encoding/json"
	"slices"
	"strings"
	"time"

	"github.com/giantswarm/beekeeper/internal/config"
)

// Activity is how a session has been doing, read from the same window of
// its transcript as its Work (the last 512 KiB): reading whole transcripts
// on every call would cost seconds with thirty sessions. Since is the
// window's first entry; Whole says the window is the whole transcript.
type Activity struct {
	Since    time.Time `json:"since,omitzero"`
	Whole    bool      `json:"whole"`
	Total    Counts    `json:"total"`
	LastHour Counts    `json:"lastHour"`
	// Context is the size of the last request: the tokens it read and the
	// tokens it wrote, which the next request reads.
	Context       int64   `json:"contextTokens"`
	ContextWindow int     `json:"contextWindow,omitempty"`
	ContextFill   float64 `json:"contextFill,omitempty"`
	Model         string  `json:"model,omitempty"`
	messages      []message
	window        time.Time
}

// Counts are the figures of one span of a transcript.
type Counts struct {
	// Busy is the time between entries less than busyGap apart.
	Busy        time.Duration `json:"busy"`
	Turns       int           `json:"turns"`
	ToolCalls   int           `json:"toolCalls"`
	ToolErrors  int           `json:"toolErrors"`
	GitHubCalls int           `json:"githubCalls"`
	// SameErrors is how often the most repeated failing tool call failed;
	// SameError names it.
	SameErrors int    `json:"sameErrors"`
	SameError  string `json:"sameError,omitempty"`
	Tokens     Tokens `json:"tokens"`
	// CostUSD is nil when a request's model has no price: CostUnknown
	// names those models.
	CostUSD     *float64 `json:"costUSD,omitempty"`
	CostUnknown []string `json:"costUnknown,omitempty"`
}

// Add adds o to c: the sums, the larger repeat and an unknown cost.
func (c *Counts) Add(o Counts) {
	c.Busy += o.Busy
	c.Turns += o.Turns
	c.ToolCalls += o.ToolCalls
	c.ToolErrors += o.ToolErrors
	c.GitHubCalls += o.GitHubCalls
	if o.SameErrors > c.SameErrors {
		c.SameErrors, c.SameError = o.SameErrors, o.SameError
	}
	c.Tokens.add(o.Tokens)
	for _, m := range o.CostUnknown {
		if !slices.Contains(c.CostUnknown, m) {
			c.CostUnknown = append(c.CostUnknown, m)
		}
	}
	switch {
	case len(c.CostUnknown) > 0:
		c.CostUSD = nil
	case c.CostUSD == nil && o.CostUSD != nil:
		v := *o.CostUSD
		c.CostUSD = &v
	case o.CostUSD != nil:
		*c.CostUSD += *o.CostUSD
	}
}

// Tokens are the usage fields of the API's responses, summed.
type Tokens struct {
	Input        int64 `json:"input"`
	CacheWrite5m int64 `json:"cacheWrite5m"`
	CacheWrite1h int64 `json:"cacheWrite1h"`
	CacheRead    int64 `json:"cacheRead"`
	Output       int64 `json:"output"`
}

// Sum is every token of t.
func (t Tokens) Sum() int64 {
	return t.Input + t.CacheWrite5m + t.CacheWrite1h + t.CacheRead + t.Output
}

func (t *Tokens) add(o Tokens) {
	t.Input += o.Input
	t.CacheWrite5m += o.CacheWrite5m
	t.CacheWrite1h += o.CacheWrite1h
	t.CacheRead += o.CacheRead
	t.Output += o.Output
}

// busyGap is the longest pause between two entries that counts as busy.
const busyGap = 5 * time.Minute

// message is one API response: its streamed content blocks repeat the
// message id and the usage on one line each.
type message struct {
	at     time.Time
	model  string
	fast   bool
	tokens Tokens
}

type activityEntry struct {
	Type        string    `json:"type"`
	Timestamp   time.Time `json:"timestamp"`
	IsMeta      bool      `json:"isMeta"`
	IsSidechain bool      `json:"isSidechain"`
	Message     struct {
		ID      string          `json:"id"`
		Model   string          `json:"model"`
		Usage   *apiUsage       `json:"usage"`
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

type apiUsage struct {
	Input         int64  `json:"input_tokens"`
	CacheCreation int64  `json:"cache_creation_input_tokens"`
	CacheRead     int64  `json:"cache_read_input_tokens"`
	Output        int64  `json:"output_tokens"`
	Speed         string `json:"speed"`
	ByTTL         *struct {
		M5 int64 `json:"ephemeral_5m_input_tokens"`
		H1 int64 `json:"ephemeral_1h_input_tokens"`
	} `json:"cache_creation"`
}

// tokens splits the cache writes by TTL; a response without the split
// wrote the API's default, the 5-minute TTL.
func (u *apiUsage) tokens() Tokens {
	t := Tokens{Input: u.Input, CacheRead: u.CacheRead, Output: u.Output, CacheWrite5m: u.CacheCreation}
	if u.ByTTL != nil && u.ByTTL.M5+u.ByTTL.H1 == u.CacheCreation {
		t.CacheWrite5m, t.CacheWrite1h = u.ByTTL.M5, u.ByTTL.H1
	}
	return t
}

type activityBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	IsError   bool            `json:"is_error"`
}

// ReadTranscript reads the window of the transcript at path once for what
// the session is on and how it has been doing.
func ReadTranscript(path string, now time.Time) (Work, Activity) {
	buf, whole := readWindow(path)
	return scanWork(string(buf)), scanActivity(buf, whole, now)
}

// spanCounter adds up one span of the window.
type spanCounter struct {
	c      *Counts
	errors map[string]int
	last   time.Time
}

func (s *spanCounter) at(t time.Time) {
	if !s.last.IsZero() && t.After(s.last) && t.Sub(s.last) <= busyGap {
		s.c.Busy += t.Sub(s.last)
	}
	s.last = t
}

func (s *spanCounter) failed(call toolCall) {
	s.c.ToolErrors++
	if call.key == "" {
		return
	}
	s.errors[call.key]++
	if n := s.errors[call.key]; n > s.c.SameErrors {
		s.c.SameErrors, s.c.SameError = n, call.label
	}
}

// toolCall identifies a tool call across its repeats: the tool and its
// input.
type toolCall struct{ key, label string }

func scanActivity(buf []byte, whole bool, now time.Time) Activity {
	a := Activity{Whole: whole, window: now.Add(-time.Hour)}
	if !whole {
		if i := bytes.IndexByte(buf, '\n'); i >= 0 {
			buf = buf[i+1:] // a partial first line
		} else {
			buf = nil
		}
	}
	total := &spanCounter{c: &a.Total, errors: map[string]int{}}
	hour := &spanCounter{c: &a.LastHour, errors: map[string]int{}}
	calls := map[string]toolCall{}
	byID := map[string]int{}
	for len(buf) > 0 {
		line := buf
		if i := bytes.IndexByte(buf, '\n'); i >= 0 {
			line, buf = buf[:i], buf[i+1:]
		} else {
			buf = nil
		}
		// Most lines are neither: skip them before decoding.
		if !bytes.Contains(line, []byte(`"type":"assistant"`)) && !bytes.Contains(line, []byte(`"type":"user"`)) {
			continue
		}
		var e activityEntry
		if json.Unmarshal(line, &e) != nil || (e.Type != roleUser && e.Type != roleAssistant) {
			continue
		}
		spans := []*spanCounter{total}
		if !e.Timestamp.Before(a.window) {
			spans = append(spans, hour)
		}
		if a.Since.IsZero() {
			a.Since = e.Timestamp
		}
		total.at(e.Timestamp)
		if len(spans) > 1 {
			hour.at(e.Timestamp)
		}
		var blocks []activityBlock
		var text string
		if json.Unmarshal(e.Message.Content, &text) != nil {
			_ = json.Unmarshal(e.Message.Content, &blocks)
		}
		if e.Type == roleAssistant {
			a.assistant(e, blocks, spans, calls, byID)
			continue
		}
		turn := !e.IsMeta && isTurn(text)
		for _, b := range blocks {
			switch b.Type {
			case "text":
				turn = turn || (!e.IsMeta && isTurn(b.Text))
			case "tool_result":
				turn = false
				if b.IsError {
					for _, s := range spans {
						s.failed(calls[b.ToolUseID])
					}
				}
			}
		}
		if turn {
			for _, s := range spans {
				s.c.Turns++
			}
		}
	}
	for _, m := range a.messages {
		a.Total.Tokens.add(m.tokens)
		if !m.at.Before(a.window) {
			a.LastHour.Tokens.add(m.tokens)
		}
	}
	return a
}

// assistant counts a response's tool calls and keeps its usage, the last
// line of a message id being its final usage.
func (a *Activity) assistant(e activityEntry, blocks []activityBlock, spans []*spanCounter, calls map[string]toolCall, byID map[string]int) {
	for _, b := range blocks {
		if b.Type != "tool_use" {
			continue
		}
		call, github := describeCall(b)
		calls[b.ID] = call
		for _, s := range spans {
			s.c.ToolCalls++
			if github {
				s.c.GitHubCalls++
			}
		}
	}
	u := e.Message.Usage
	if u == nil {
		return
	}
	m := message{at: e.Timestamp, model: e.Message.Model, fast: u.Speed == "fast", tokens: u.tokens()}
	if m.tokens.Sum() == 0 {
		return // a synthetic message: no request
	}
	if i, ok := byID[e.Message.ID]; ok && e.Message.ID != "" {
		m.at = a.messages[i].at
		a.messages[i] = m
	} else {
		byID[e.Message.ID] = len(a.messages)
		a.messages = append(a.messages, m)
	}
	if !e.IsSidechain {
		a.Context = m.tokens.Sum()
		a.Model = m.model
	}
}

// isTurn says a user message's text starts a model turn: a person's or a
// peer's words, a task notification; not a reminder the harness injected
// or a shell command the person ran in the prompt.
func isTurn(text string) bool {
	t := strings.TrimSpace(text)
	return t != "" && !strings.HasPrefix(t, "<system-reminder>") &&
		!strings.HasPrefix(t, "<bash-") && !strings.HasPrefix(t, "<local-command-")
}

// describeCall identifies a tool call and says whether it calls GitHub: a
// gh or devctl command, or a tool of a GitHub MCP server.
func describeCall(b activityBlock) (toolCall, bool) {
	var in struct {
		Command string `json:"command"`
	}
	_ = json.Unmarshal(b.Input, &in)
	if in.Command == "" {
		return toolCall{key: b.Name + " " + string(b.Input), label: b.Name}, strings.Contains(strings.ToLower(b.Name), "github")
	}
	return toolCall{key: b.Name + " " + in.Command, label: b.Name + ": " + commandHead(in.Command)},
		invokesGitHub(in.Command)
}

// commandHead is a command's first words, on one line.
func commandHead(cmd string) string {
	f := strings.Fields(cmd)
	return strings.Join(f[:min(4, len(f))], " ")
}

// invokesGitHub says a shell command runs gh or devctl as one of its
// commands.
func invokesGitHub(cmd string) bool {
	f := strings.FieldsFunc(cmd, func(r rune) bool {
		return r == ' ' || r == '\t' || r == '\n' || r == ';' || r == '|' || r == '&' || r == '(' || r == ')' || r == '`' || r == '$'
	})
	return slices.ContainsFunc(f, func(w string) bool {
		w = w[strings.LastIndex(w, "/")+1:]
		return w == "gh" || w == "devctl"
	})
}

// Price sets the costs and the context window from the configured models.
func (a *Activity) Price(m config.Metrics) {
	a.Total.CostUSD, a.Total.CostUnknown = cost(a.messages, m, time.Time{})
	a.LastHour.CostUSD, a.LastHour.CostUnknown = cost(a.messages, m, a.window)
	if p, ok := m.Model(a.Model); ok && p.ContextWindow > 0 {
		a.ContextWindow = p.ContextWindow
		a.ContextFill = float64(a.Context) / float64(p.ContextWindow)
	}
}

// cost prices the messages from since on; a model without a price, or a
// fast request without a fast multiplier, leaves the cost unknown.
func cost(msgs []message, m config.Metrics, since time.Time) (*float64, []string) {
	var usd float64
	var unknown []string
	for _, msg := range msgs {
		if msg.at.Before(since) {
			continue
		}
		p, ok := m.Model(msg.model)
		if !ok || (msg.fast && p.Fast == 0) {
			name := msg.model
			if msg.fast {
				name += " (fast)"
			}
			if !slices.Contains(unknown, name) {
				unknown = append(unknown, name)
			}
			continue
		}
		t := msg.tokens
		c := (float64(t.Input)*p.Input + float64(t.CacheWrite5m)*p.CacheWrite5m + float64(t.CacheWrite1h)*p.CacheWrite1h +
			float64(t.CacheRead)*p.CacheRead + float64(t.Output)*p.Output) / 1e6
		if msg.fast {
			c *= p.Fast
		}
		usd += c
	}
	if len(unknown) > 0 {
		return nil, unknown
	}
	return &usd, nil
}
