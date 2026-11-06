package transcripts

import (
	"bufio"
	"bytes"
	"cmp"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/giantswarm/beekeeper/internal/claude"
)

const (
	contentTypeText    = "text"
	contentTypeMessage = "message"
	harnessAgy         = "agy"
)

// maxResultBytes bounds the stored text of a single tool result so large file
// reads or command outputs do not bloat the transcript index.
const maxResultBytes = 4096

// parserVersions are the harnesses whose parser learned to read more of an
// unchanged source: a transcript indexed by an older version is read again
// once. Bump one only for that harness; a store-wide reparse is costly.
var parserVersions = map[string]int{harnessAgy: 1}

// Parsed holds the turns and metadata extracted from a harness transcript file
// or remote execution stream.
type Parsed struct {
	SessionID  string
	Title      string
	Cwd        string
	Model      string
	Repo       string
	Refs       []string
	StartedAt  time.Time
	LastActive time.Time
	Turns      []Turn
}

var (
	githubRefURL = regexp.MustCompile(`https://github\.com/([A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+)/(?:issues|pull)/([0-9]+)`)
	shortRefRe   = regexp.MustCompile(`^([A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+)#([0-9]+)$`)
)

// NormalizeRef converts a GitHub issue/PR URL or owner/repo#n string into
// canonical "owner/repo#n" form; returns "" when s is neither.
func NormalizeRef(s string) string {
	s = strings.TrimSpace(s)
	if m := githubRefURL.FindStringSubmatch(s); len(m) == 3 {
		return m[1] + "#" + m[2]
	}
	if m := shortRefRe.FindStringSubmatch(s); len(m) == 3 {
		return m[1] + "#" + m[2]
	}
	return ""
}

// RepoOfRef returns the "owner/repo" prefix of a normalized ref ("owner/repo#n").
func RepoOfRef(ref string) string {
	norm := NormalizeRef(ref)
	if norm == "" {
		return ""
	}
	repo, _, _ := strings.Cut(norm, "#")
	return repo
}

// ExtractRefs extracts canonical "owner/repo#n" references from text.
func ExtractRefs(text string) []string {
	var out []string
	for _, m := range githubRefURL.FindAllStringSubmatch(text, -1) {
		ref := m[1] + "#" + m[2]
		if !slices.ContainsFunc(out, func(x string) bool { return strings.EqualFold(x, ref) }) {
			out = append(out, ref)
		}
	}
	return out
}

// ParseFile parses a transcript file at path according to harness ("claude",
// "agy", "codex", "omp").
func ParseFile(harness, path string) (Parsed, error) {
	f, err := os.Open(filepath.Clean(path))
	if err != nil {
		return Parsed{}, err
	}
	defer func() { _ = f.Close() }()
	return parseReader(harness, f, nil)
}

// ParseRemoteStream parses a remote execution log stream (along with any
// initial/delivered prompt turns from the coordinator's ExecutionRecord).
func ParseRemoteStream(harness string, raw []byte, prompts []Turn) Parsed {
	p, _ := parseReader(harness, bytes.NewReader(raw), prompts)
	return p
}

func parseReader(harness string, r io.Reader, prompts []Turn) (Parsed, error) {
	var p Parsed
	switch strings.ToLower(strings.TrimSpace(harness)) {
	case harnessAgy:
		p = parseAgy(r)
	case "codex":
		p = parseCodex(r)
	case "omp":
		p = parseOmp(r)
	default:
		p = parseClaude(r)
	}
	if len(prompts) > 0 {
		p.Turns = mergePromptTurns(prompts, p.Turns)
	}
	for _, t := range p.Turns {
		if !t.At.IsZero() {
			if p.StartedAt.IsZero() || t.At.Before(p.StartedAt) {
				p.StartedAt = t.At
			}
			if t.At.After(p.LastActive) {
				p.LastActive = t.At
			}
		}
		for _, ref := range ExtractRefs(t.Text) {
			if !slices.ContainsFunc(p.Refs, func(x string) bool { return strings.EqualFold(x, ref) }) {
				p.Refs = append(p.Refs, ref)
			}
		}
	}
	if p.Repo == "" && len(p.Refs) > 0 {
		p.Repo = RepoOfRef(p.Refs[0])
	}
	return p, nil
}

// mergePromptTurns prepends prompt turns that are not already present as user
// turns in the parsed stream (for remote codex or agy stream logs that do not
// echo the initial CLI prompt).
func mergePromptTurns(prompts, turns []Turn) []Turn {
	var missing []Turn
	for _, pr := range prompts {
		txt := strings.TrimSpace(pr.Text)
		if txt == "" {
			continue
		}
		found := false
		for _, t := range turns {
			if t.Role == RoleUser && strings.Contains(t.Text, txt) {
				found = true
				break
			}
		}
		if !found {
			missing = append(missing, Turn{At: pr.At, Role: RoleUser, Text: txt})
		}
	}
	if len(missing) == 0 {
		return turns
	}
	return append(missing, turns...)
}

// answer records s as the result of the tool call t, empty output included.
func (t *Turn) answer(s string) {
	t.Result, t.Answered = clipResult(s), true
}

func clipResult(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > maxResultBytes {
		return s[:maxResultBytes] + fmt.Sprintf("… <%d bytes not stored>", len(s)-maxResultBytes)
	}
	return s
}

var (
	harnessCut = regexp.MustCompile(`<truncated (\d+) bytes>`)
	storeClip  = regexp.MustCompile(`… <(\d+) bytes not stored>$`)
)

// bytesInText sums the byte counts of re's markers in s, and counts them.
func bytesInText(re *regexp.Regexp, s string) (n int64, hits int) {
	for _, m := range re.FindAllStringSubmatch(s, -1) {
		b, _ := strconv.ParseInt(m[1], 10, 64)
		n += b
		hits++
	}
	return n, hits
}

// Loss is what a transcript's turns do not hold. Text its harness cut before
// the store read it is gone for good. The rest is only missing from the
// index, and its source may still hold it: tool results the store clipped,
// and tool calls of an ended session stored with no result. A running
// session's Pending calls may simply not have finished. A call answered with
// empty output is none of these.
type Loss struct {
	HarnessBytes int64 `json:"harnessBytes,omitempty"`
	HarnessCuts  int   `json:"harnessCuts,omitempty"`
	ClippedBytes int64 `json:"clippedBytes,omitempty"`
	Clipped      int   `json:"clipped,omitempty"`
	NoResult     int   `json:"noResult,omitempty"`
	Pending      int   `json:"pending,omitempty"`
	// SourceGone says the source file the index was read from no longer
	// exists, so what the index misses is gone too.
	SourceGone bool `json:"sourceGone,omitempty"`
}

// LossOf reads the loss of turns, as ReadTurns returned them: results counts
// only when they were asked for, and running says the session still runs.
func LossOf(turns []Turn, results, running bool) Loss {
	var l Loss
	for _, t := range turns {
		for _, text := range []string{t.Text, t.Result} {
			n, hits := bytesInText(harnessCut, text)
			l.HarnessBytes += n
			l.HarnessCuts += hits
		}
		n, hits := bytesInText(storeClip, t.Result)
		// An older index clipped with a bare ellipsis, saying no size.
		if hits == 0 && len(t.Result) >= maxResultBytes && strings.HasSuffix(t.Result, "…") {
			hits = 1
		}
		l.ClippedBytes += n
		l.Clipped += hits
		if results && t.Role == RoleTool && t.Result == "" && !t.Answered {
			if running {
				l.Pending++
			} else {
				l.NoResult++
			}
		}
	}
	return l
}

// Lost says what no read brings back, "" for nothing.
func (l Loss) Lost() string {
	if l.HarnessCuts == 0 {
		return ""
	}
	return fmt.Sprintf("the harness cut %d bytes in %d places before the store read them", l.HarnessBytes, l.HarnessCuts)
}

// Unindexed says what the index misses and whether its source may hold it,
// "" for nothing.
func (l Loss) Unindexed() string {
	var parts []string
	if l.Clipped > 0 {
		parts = append(parts, fmt.Sprintf("%d tool results are clipped to %d bytes", l.Clipped, maxResultBytes))
	}
	if l.NoResult > 0 {
		parts = append(parts, fmt.Sprintf("%d tool calls have no stored result", l.NoResult))
	}
	var out string
	if len(parts) > 0 {
		out = strings.Join(parts, ", ") + ": the source transcript may still hold them"
		if l.SourceGone {
			out = strings.Join(parts, ", ") + ": the source transcript is gone, so they are lost too"
		}
	}
	if l.Pending > 0 {
		out = strings.TrimPrefix(out+fmt.Sprintf("; %d tool calls have no result yet", l.Pending), "; ")
	}
	return out
}

func forEachLine(r io.Reader, fn func(line []byte)) {
	br := bufio.NewReaderSize(r, 256<<10)
	for {
		line, err := br.ReadBytes('\n')
		trimmed := bytes.TrimSpace(line)
		if len(trimmed) > 0 {
			fn(trimmed)
		}
		if err != nil {
			return
		}
	}
}

func parseClaude(r io.Reader) Parsed {
	var p Parsed
	callIdx := map[string]int{}
	askCall := map[string]bool{}

	forEachLine(r, func(line []byte) {
		var e struct {
			Type          string          `json:"type"`
			SessionID     string          `json:"sessionId"`
			SessionIDAlt  string          `json:"session_id"`
			CustomTitle   string          `json:"customTitle"`
			Cwd           string          `json:"cwd"`
			Timestamp     time.Time       `json:"timestamp"`
			IsMeta        bool            `json:"isMeta"`
			IsSidechain   bool            `json:"isSidechain"`
			Result        string          `json:"result"`
			ToolUseResult json.RawMessage `json:"toolUseResult"`
			Message       struct {
				Role    string          `json:"role"`
				Model   string          `json:"model"`
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(line, &e) != nil || e.IsMeta || e.IsSidechain {
			return
		}
		if p.SessionID == "" {
			p.SessionID = cmp.Or(e.SessionID, e.SessionIDAlt)
		}
		if e.CustomTitle != "" {
			p.Title = e.CustomTitle
		}
		if p.Cwd == "" && e.Cwd != "" {
			p.Cwd = e.Cwd
		}
		if e.Message.Model != "" && e.Message.Model != "<synthetic>" {
			p.Model = e.Message.Model
		}
		if e.Type == "result" && strings.TrimSpace(e.Result) != "" {
			// Stream-json final result: record if no assistant turn already holds it.
			res := strings.TrimSpace(e.Result)
			hasAssistant := false
			for i := len(p.Turns) - 1; i >= 0; i-- {
				if p.Turns[i].Role == RoleAssistant {
					hasAssistant = strings.Contains(p.Turns[i].Text, res)
					break
				}
			}
			if !hasAssistant {
				p.Turns = append(p.Turns, Turn{At: e.Timestamp, Role: RoleAssistant, Text: res})
			}
			return
		}
		if e.Type != RoleUser && e.Type != RoleAssistant {
			return
		}

		var textParts []string
		var toolTurns []Turn
		var s string
		if json.Unmarshal(e.Message.Content, &s) == nil {
			if strings.TrimSpace(s) != "" {
				textParts = append(textParts, s)
			}
		} else {
			var blocks []struct {
				Type      string          `json:"type"`
				ID        string          `json:"id"`
				Text      string          `json:"text"`
				Name      string          `json:"name"`
				Input     json.RawMessage `json:"input"`
				ToolUseID string          `json:"tool_use_id"`
				IsError   bool            `json:"is_error"`
				Content   json.RawMessage `json:"content"`
			}
			if json.Unmarshal(e.Message.Content, &blocks) != nil {
				return
			}
			var resMeta struct {
				Answers map[string]string `json:"answers"`
			}
			_ = json.Unmarshal(e.ToolUseResult, &resMeta)

			var askParts []string
			for _, b := range blocks {
				switch b.Type {
				case contentTypeText:
					if strings.TrimSpace(b.Text) != "" {
						textParts = append(textParts, b.Text)
					}
				case "tool_use":
					if b.Name != "" {
						if b.Name == "AskUserQuestion" && b.ID != "" {
							askCall[b.ID] = true
						}
						gist := claude.ToolGist(b.Name, b.Input)
						toolTurns = append(toolTurns, Turn{
							At:   e.Timestamp,
							Role: RoleTool,
							Tool: b.Name,
							Text: gist,
						})
						if b.ID != "" {
							// Index will be assigned after text turn is appended.
							callIdx[b.ID] = -len(toolTurns)
						}
					}
				case "tool_result":
					resText := extractToolResultText(b.Content)
					if idx, ok := callIdx[b.ToolUseID]; ok && idx >= 0 && idx < len(p.Turns) {
						p.Turns[idx].answer(resText)
					}
					if e.Type == RoleUser && !b.IsError {
						if askCall[b.ToolUseID] || len(resMeta.Answers) > 0 || strings.HasPrefix(strings.TrimSpace(resText), "User has answered") {
							if strings.TrimSpace(resText) != "" {
								askParts = append(askParts, strings.TrimSpace(resText))
							}
						}
					}
				}
			}
			if e.Type == RoleUser && (len(askParts) > 0 || len(resMeta.Answers) > 0) {
				joinedAsk := strings.Join(askParts, "\n")
				for _, k := range slices.Sorted(maps.Keys(resMeta.Answers)) {
					v := strings.TrimSpace(resMeta.Answers[k])
					if v != "" && !strings.Contains(joinedAsk, v) {
						askParts = append(askParts, k+": "+v)
					}
				}
				if ans := strings.TrimSpace(strings.Join(askParts, "\n")); ans != "" {
					textParts = append(textParts, ans)
				}
			}
		}

		joined := strings.TrimSpace(strings.Join(textParts, "\n"))
		if joined != "" && !strings.HasPrefix(joined, "<system-reminder>") {
			p.Turns = append(p.Turns, Turn{At: e.Timestamp, Role: e.Type, Text: joined})
		}
		base := len(p.Turns)
		p.Turns = append(p.Turns, toolTurns...)
		for id, neg := range callIdx {
			if neg < 0 {
				callIdx[id] = base + (-neg - 1)
			}
		}
	})
	return p
}

func extractToolResultText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) == nil {
		var parts []string
		for _, b := range blocks {
			if b.Type == contentTypeText && strings.TrimSpace(b.Text) != "" {
				parts = append(parts, b.Text)
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

func parseAgy(r io.Reader) Parsed {
	var p Parsed
	type agyToolCall struct {
		ID   string          `json:"id"`
		Name string          `json:"name"`
		Args json.RawMessage `json:"args"`
	}
	type agyStep struct {
		Type      string        `json:"type"`
		CreatedAt time.Time     `json:"created_at"`
		Content   string        `json:"content"`
		ToolCalls []agyToolCall `json:"tool_calls"`
	}
	// A step after a planner response answers its oldest open tool call.
	var pending []int
	addStep := func(st agyStep, fallbackAt time.Time) {
		at := st.CreatedAt
		if at.IsZero() {
			at = fallbackAt
		}
		switch st.Type {
		case "USER_INPUT":
			if txt := cleanAgyUser(st.Content); txt != "" {
				p.Turns = append(p.Turns, Turn{At: at, Role: RoleUser, Text: txt})
			}
		case "PLANNER_RESPONSE":
			if txt := strings.TrimSpace(st.Content); txt != "" {
				p.Turns = append(p.Turns, Turn{At: at, Role: RoleAssistant, Text: txt})
			}
			for _, tc := range st.ToolCalls {
				if tc.Name != "" {
					p.Turns = append(p.Turns, Turn{
						At:   at,
						Role: RoleTool,
						Tool: tc.Name,
						Text: agyToolGist(tc.Name, tc.Args),
					})
					pending = append(pending, len(p.Turns)-1)
				}
			}
		case "GENERIC", "TOOL_RESPONSE":
			if len(pending) > 0 {
				p.Turns[pending[0]].answer(st.Content)
				pending = pending[1:]
			}
		}
	}

	forEachLine(r, func(line []byte) {
		var ev struct {
			Event          string          `json:"event"`
			Type           string          `json:"type"`
			SessionID      string          `json:"session_id"`
			ConversationID string          `json:"conversation_id"`
			Timestamp      time.Time       `json:"timestamp"`
			CreatedAt      time.Time       `json:"created_at"`
			Role           string          `json:"role"`
			Content        string          `json:"content"`
			Result         json.RawMessage `json:"result"`
			ToolCalls      []agyToolCall   `json:"tool_calls"`
			Step           *agyStep        `json:"step"`
			StepUpdate     *struct {
				ConversationID string        `json:"conversation_id"`
				StepType       string        `json:"step_type"`
				Content        string        `json:"content"`
				ToolCalls      []agyToolCall `json:"tool_calls"`
			} `json:"step_update"`
			Stats struct {
				Models map[string]json.RawMessage `json:"models"`
			} `json:"stats"`
		}
		if json.Unmarshal(line, &ev) != nil {
			return
		}
		if id := cmp.Or(ev.SessionID, ev.ConversationID); id != "" && p.SessionID == "" {
			p.SessionID = id
		}
		for m := range ev.Stats.Models {
			if m != "" {
				p.Model = m
			}
		}
		kind := cmp.Or(ev.Type, ev.Event)
		switch kind {
		case "USER_INPUT", "PLANNER_RESPONSE", "GENERIC", "TOOL_RESPONSE", "user_input", "planner_response":
			addStep(agyStep{Type: strings.ToUpper(kind), CreatedAt: cmp.Or(ev.CreatedAt, ev.Timestamp), Content: ev.Content, ToolCalls: ev.ToolCalls}, ev.Timestamp)
		case "step_update", "step":
			if ev.Step != nil {
				addStep(*ev.Step, ev.Timestamp)
			}
			if ev.StepUpdate != nil {
				if p.SessionID == "" && ev.StepUpdate.ConversationID != "" {
					p.SessionID = ev.StepUpdate.ConversationID
				}
				addStep(agyStep{
					Type:      strings.ToUpper(ev.StepUpdate.StepType),
					CreatedAt: cmp.Or(ev.CreatedAt, ev.Timestamp),
					Content:   ev.StepUpdate.Content,
					ToolCalls: ev.StepUpdate.ToolCalls,
				}, ev.Timestamp)
			}
		case contentTypeMessage:
			role := RoleAssistant
			if ev.Role == "user" {
				role = RoleUser
			}
			if txt := strings.TrimSpace(ev.Content); txt != "" {
				p.Turns = append(p.Turns, Turn{At: ev.Timestamp, Role: role, Text: txt})
			}
		case "result":
			var resStr string
			if json.Unmarshal(ev.Result, &resStr) != nil {
				var resObj struct {
					Result string `json:"result"`
					Text   string `json:"text"`
				}
				if json.Unmarshal(ev.Result, &resObj) == nil {
					resStr = cmp.Or(resObj.Result, resObj.Text)
				}
			}
			if txt := strings.TrimSpace(resStr); txt != "" {
				hasAssistant := false
				for i := len(p.Turns) - 1; i >= 0; i-- {
					if p.Turns[i].Role == RoleAssistant {
						hasAssistant = strings.Contains(p.Turns[i].Text, txt)
						break
					}
				}
				if !hasAssistant {
					p.Turns = append(p.Turns, Turn{At: ev.Timestamp, Role: RoleAssistant, Text: txt})
				}
			}
		}
		if p.Cwd == "" {
			p.Cwd = extractAgyWorkspace(line)
		}
	})
	return p
}

var agyWorkspaceRe = regexp.MustCompile(`Active Workspaces:\s*\n-\s*([^\n\r]+)`)

func extractAgyWorkspace(line []byte) string {
	if m := agyWorkspaceRe.FindSubmatch(line); len(m) == 2 {
		return strings.TrimSpace(string(m[1]))
	}
	return ""
}

func cleanAgyUser(s string) string {
	if _, rest, ok := strings.Cut(s, "<USER_REQUEST>"); ok {
		if body, _, ok := strings.Cut(rest, "</USER_REQUEST>"); ok {
			s = body
		}
	}
	return strings.TrimSpace(s)
}

func agyToolGist(name string, args json.RawMessage) string {
	var in map[string]any
	if json.Unmarshal(args, &in) == nil {
		for _, k := range []string{"toolSummary", "CommandLine", "Path", "Pattern", "Query", "SearchPath", "Url", "query"} {
			if v, ok := in[k].(string); ok && strings.TrimSpace(v) != "" {
				first, _, _ := strings.Cut(strings.TrimSpace(v), "\n")
				return name + ": " + first
			}
		}
	}
	return claude.ToolGist(name, args)
}

func parseCodex(r io.Reader) Parsed {
	var p Parsed
	callIdx := map[string]int{}

	forEachLine(r, func(line []byte) {
		var ev struct {
			Timestamp time.Time `json:"timestamp"`
			Type      string    `json:"type"`
			ThreadID  string    `json:"thread_id"`
			Payload   struct {
				Type      string          `json:"type"`
				ID        string          `json:"id"`
				CallID    string          `json:"call_id"`
				Role      string          `json:"role"`
				Phase     string          `json:"phase"`
				Name      string          `json:"name"`
				Cwd       string          `json:"cwd"`
				Model     string          `json:"model"`
				Arguments json.RawMessage `json:"arguments"`
				Input     string          `json:"input"`
				Output    string          `json:"output"`
				Content   []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			} `json:"payload"`
			Item struct {
				ID               string `json:"id"`
				Type             string `json:"type"`
				Text             string `json:"text"`
				Command          string `json:"command"`
				Tool             string `json:"tool"`
				AggregatedOutput string `json:"aggregated_output"`
			} `json:"item"`
		}
		if json.Unmarshal(line, &ev) != nil {
			return
		}
		switch ev.Type {
		case "session_meta":
			if p.SessionID == "" && ev.Payload.ID != "" {
				p.SessionID = ev.Payload.ID
			}
			if p.Cwd == "" && ev.Payload.Cwd != "" {
				p.Cwd = ev.Payload.Cwd
			}
		case "turn_context":
			if p.Cwd == "" && ev.Payload.Cwd != "" {
				p.Cwd = ev.Payload.Cwd
			}
			if ev.Payload.Model != "" {
				p.Model = ev.Payload.Model
			}
		case "thread.started":
			if p.SessionID == "" && ev.ThreadID != "" {
				p.SessionID = ev.ThreadID
			}
		case "item.completed":
			switch ev.Item.Type {
			case "user_message":
				if txt := strings.TrimSpace(ev.Item.Text); txt != "" {
					p.Turns = append(p.Turns, Turn{At: ev.Timestamp, Role: RoleUser, Text: txt})
				}
			case "agent_message":
				if txt := strings.TrimSpace(ev.Item.Text); txt != "" {
					p.Turns = append(p.Turns, Turn{At: ev.Timestamp, Role: RoleAssistant, Text: txt})
				}
			case "command_execution":
				cmd := strings.TrimSpace(ev.Item.Command)
				if cmd != "" {
					first, _, _ := strings.Cut(cmd, "\n")
					p.Turns = append(p.Turns, Turn{
						At:       ev.Timestamp,
						Role:     RoleTool,
						Tool:     "exec_command",
						Text:     "exec_command: " + first,
						Result:   clipResult(ev.Item.AggregatedOutput),
						Answered: true,
					})
				}
			case "mcp_tool_call":
				if ev.Item.Tool != "" {
					p.Turns = append(p.Turns, Turn{
						At:   ev.Timestamp,
						Role: RoleTool,
						Tool: ev.Item.Tool,
						Text: ev.Item.Tool,
					})
				}
			}
		case "response_item":
			switch ev.Payload.Type {
			case contentTypeMessage:
				role := ev.Payload.Role
				if role != RoleUser && role != RoleAssistant {
					return
				}
				var parts []string
				for _, c := range ev.Payload.Content {
					if (c.Type == "input_text" || c.Type == "output_text" || c.Type == contentTypeText) && isCodexUserOrAssistantTurn(role, c.Text) {
						parts = append(parts, strings.TrimSpace(c.Text))
					}
				}
				if joined := strings.TrimSpace(strings.Join(parts, "\n")); joined != "" {
					// Commentary (progress, explanations, decisions) is
					// part of the conversation as much as the final answer.
					p.Turns = append(p.Turns, Turn{At: ev.Timestamp, Role: role, Phase: ev.Payload.Phase, Text: joined})
				}
			case "function_call":
				if ev.Payload.Name != "" {
					idx := len(p.Turns)
					p.Turns = append(p.Turns, Turn{
						At:   ev.Timestamp,
						Role: RoleTool,
						Tool: ev.Payload.Name,
						Text: codexToolGist(ev.Payload.Name, ev.Payload.Arguments),
					})
					if ev.Payload.CallID != "" {
						callIdx[ev.Payload.CallID] = idx
					}
				}
			case "custom_tool_call":
				if ev.Payload.Name != "" {
					first, _, _ := strings.Cut(strings.TrimSpace(ev.Payload.Input), "\n")
					text := ev.Payload.Name
					if first != "" {
						text += ": " + first
					}
					idx := len(p.Turns)
					p.Turns = append(p.Turns, Turn{
						At:   ev.Timestamp,
						Role: RoleTool,
						Tool: ev.Payload.Name,
						Text: text,
					})
					if ev.Payload.CallID != "" {
						callIdx[ev.Payload.CallID] = idx
					}
				}
			case "function_call_output", "custom_tool_call_output":
				if idx, ok := callIdx[ev.Payload.CallID]; ok && idx >= 0 && idx < len(p.Turns) {
					p.Turns[idx].answer(ev.Payload.Output)
				}
			}
		}
	})
	return p
}

func isCodexUserOrAssistantTurn(role, text string) bool {
	t := strings.TrimSpace(text)
	if t == "" {
		return false
	}
	if role != RoleUser {
		return true
	}
	for _, prefix := range []string{"# AGENTS.md instructions", "<environment_context>", "<permissions instructions>", "<collaboration_mode>", "<system-reminder>"} {
		if strings.HasPrefix(t, prefix) {
			return false
		}
	}
	return true
}

func codexToolGist(name string, args json.RawMessage) string {
	var in map[string]any
	if json.Unmarshal(args, &in) == nil {
		for _, k := range []string{"cmd", "command", "file_path", "path", "pattern", "url", "query"} {
			if v, ok := in[k].(string); ok && strings.TrimSpace(v) != "" {
				first, _, _ := strings.Cut(strings.TrimSpace(v), "\n")
				return name + ": " + first
			}
		}
	}
	return claude.ToolGist(name, args)
}

func parseOmp(r io.Reader) Parsed {
	var p Parsed
	callIdx := map[string]int{}

	forEachLine(r, func(line []byte) {
		var e struct {
			Type      string    `json:"type"`
			ID        string    `json:"id"`
			Title     string    `json:"title"`
			Cwd       string    `json:"cwd"`
			Timestamp time.Time `json:"timestamp"`
			Message   struct {
				Role       string `json:"role"`
				Provider   string `json:"provider"`
				Model      string `json:"model"`
				ToolCallID string `json:"toolCallId"`
				Content    []struct {
					Type      string          `json:"type"`
					ID        string          `json:"id"`
					Text      string          `json:"text"`
					Name      string          `json:"name"`
					Arguments json.RawMessage `json:"arguments"`
				} `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(line, &e) != nil {
			return
		}
		switch e.Type {
		case "session":
			if p.SessionID == "" && e.ID != "" {
				p.SessionID = e.ID
			}
			if p.Cwd == "" && e.Cwd != "" {
				p.Cwd = e.Cwd
			}
		case "title_change":
			if strings.TrimSpace(e.Title) != "" {
				p.Title = strings.TrimSpace(e.Title)
			}
		case contentTypeMessage:
			if e.Message.Model != "" {
				m := e.Message.Model
				if e.Message.Provider != "" && !strings.Contains(m, "/") {
					m = e.Message.Provider + "/" + m
				}
				p.Model = m
			}
			role := e.Message.Role
			if role == "toolResult" {
				var parts []string
				for _, b := range e.Message.Content {
					if b.Type == contentTypeText && strings.TrimSpace(b.Text) != "" {
						parts = append(parts, b.Text)
					}
				}
				if idx, ok := callIdx[e.Message.ToolCallID]; ok && idx >= 0 && idx < len(p.Turns) {
					p.Turns[idx].answer(strings.Join(parts, "\n"))
				}
				return
			}
			if role != RoleUser && role != RoleAssistant {
				return
			}
			var texts []string
			var tools []Turn
			var toolIDs []string
			for _, b := range e.Message.Content {
				switch b.Type {
				case contentTypeText:
					if strings.TrimSpace(b.Text) != "" {
						texts = append(texts, b.Text)
					}
				case "toolCall":
					if b.Name != "" {
						tools = append(tools, Turn{
							At:   e.Timestamp,
							Role: RoleTool,
							Tool: b.Name,
							Text: claude.ToolGist(b.Name, b.Arguments),
						})
						toolIDs = append(toolIDs, b.ID)
					}
				}
			}
			if joined := strings.TrimSpace(strings.Join(texts, "\n")); joined != "" {
				p.Turns = append(p.Turns, Turn{At: e.Timestamp, Role: role, Text: joined})
			}
			base := len(p.Turns)
			p.Turns = append(p.Turns, tools...)
			for i, id := range toolIDs {
				if id != "" {
					callIdx[id] = base + i
				}
			}
		}
	})
	return p
}
