package claude

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

// Turn is one message of a transcript: what a person, a peer or the model
// said, without tool results. Follow adds the model's tool calls as turns
// of RoleTool, the call's name and its gist.
type Turn struct {
	At   time.Time `json:"at"`
	Role string    `json:"role"`
	Text string    `json:"text"`
}

// tailWindow is the initial transcript window. Sparse or larger requests
// expand it until enough turns have been read, or the start is reached.
const tailWindow = 2 << 20

// Tail returns the last n turns of the transcript at path.
func Tail(path string, n int) ([]Turn, error) { return tail(path, n, false, nil) }

// Follow returns the last n turns of the transcript at path with the
// model's tool calls among them: what a session does right now, as the
// person's screen follows it.
func Follow(path string, n int) ([]Turn, error) { return tail(path, n, true, nil) }

func tail(path string, n int, tools bool, onDialog func(Dialog)) ([]Turn, error) {
	f, err := os.Open(filepath.Clean(path))
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	window := int64(tailWindow)
	if n <= 0 {
		window = fi.Size()
	}
	for {
		off := max(fi.Size()-window, 0)
		turns, err := readTail(f, off, tools, onDialog)
		if err != nil {
			return nil, err
		}
		if off == 0 || n > 0 && len(turns) >= n {
			if n > 0 && len(turns) > n {
				turns = turns[len(turns)-n:]
			}
			return turns, nil
		}
		window *= 2
	}
}

func readTail(f *os.File, off int64, tools bool, onDialog func(Dialog)) ([]Turn, error) {
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		return nil, err
	}
	r := bufio.NewReaderSize(f, 1<<20)
	if off > 0 {
		if _, err := r.ReadBytes('\n'); err != nil { // a partial first line
			return nil, err
		}
	}
	var turns []Turn
	tp := turnParser{tools: tools, asks: map[string][]string{}, onDialog: onDialog}
	for {
		line, err := r.ReadBytes('\n')
		turns = append(turns, tp.parse(bytes.TrimSpace(line))...)
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
	}
	return turns, nil
}

// firstWindow bounds how much of a transcript's start First reads.
const firstWindow = 4 << 20

// First returns the first turn a person or a starter gave the session: its
// brief. found is false when the transcript's start holds none.
func First(path string) (t Turn, found bool, err error) {
	f, err := os.Open(filepath.Clean(path))
	if err != nil {
		return Turn{}, false, err
	}
	defer func() { _ = f.Close() }()
	r := bufio.NewReaderSize(io.LimitReader(f, firstWindow), 1<<20)
	for {
		line, err := r.ReadBytes('\n')
		if t, ok := parseTurn(bytes.TrimSpace(line)); ok && t.Role == roleUser {
			return t, true, nil
		}
		if err == io.EOF {
			return Turn{}, false, nil
		}
		if err != nil {
			return Turn{}, false, err
		}
	}
}

// The roles of a transcript's message entries.
const (
	roleUser      = "user"
	roleAssistant = "assistant"
)

// RoleTool is the role of a tool call's turn in what Follow returns.
const RoleTool = "tool"

// The content block types of a tool call and of text.
const (
	blockToolUse    = "tool_use"
	blockToolResult = "tool_result"
	blockText       = "text"
)

type entry struct {
	Type      string    `json:"type"`
	Timestamp time.Time `json:"timestamp"`
	IsMeta    bool      `json:"isMeta"`
	Message   struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"message"`
	ToolUseResult json.RawMessage `json:"toolUseResult"`
}

type block struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	IsError   bool            `json:"is_error"`
	Content   json.RawMessage `json:"content"`
}

func parseTurn(line []byte) (Turn, bool) {
	ts := (&turnParser{}).parse(line)
	if len(ts) == 0 {
		return Turn{}, false
	}
	return ts[0], true
}

// askTool is Claude Code's tool that puts a question to the person
// (guard.AskTool).
const askTool = "AskUserQuestion"

// turnParser reads a transcript's lines in order. asks holds the questions of
// each AskUserQuestion call seen so far by its tool_use id: only a result of
// one of those calls is a person's dialog answer (a nil map reads none).
type turnParser struct {
	tools    bool
	asks     map[string][]string
	onDialog func(Dialog)
}

// parse reads one transcript line: its text as one turn and, with tools, a
// turn for each tool call after it.
func (p *turnParser) parse(line []byte) []Turn {
	if len(line) == 0 {
		return nil
	}
	var e entry
	if json.Unmarshal(line, &e) != nil || e.IsMeta || (e.Type != roleUser && e.Type != roleAssistant) {
		return nil
	}
	var text []string
	var calls []Turn
	var s string
	if json.Unmarshal(e.Message.Content, &s) == nil {
		text = append(text, s)
	} else {
		var blocks []block
		if json.Unmarshal(e.Message.Content, &blocks) != nil {
			return nil
		}
		for _, b := range blocks {
			switch {
			case b.Type == blockText && strings.TrimSpace(b.Text) != "":
				text = append(text, b.Text)
			case p.tools && b.Type == blockToolUse && b.Name != "":
				calls = append(calls, Turn{At: e.Timestamp, Role: RoleTool, Text: ToolGist(b.Name, b.Input)})
			}
			if p.asks != nil && b.Type == blockToolUse && b.Name == askTool && b.ID != "" {
				p.asks[b.ID] = askQuestions(b.Input)
			}
		}
		if e.Type == roleUser && p.asks != nil {
			if ans, questions := askUserTurnText(e.ToolUseResult, blocks, p.asks); ans != "" {
				text = append(text, ans)
				if p.onDialog != nil {
					p.onDialog(Dialog{At: e.Timestamp, Questions: questions, Text: ans})
				}
			}
		}
	}
	var out []Turn
	joined := strings.TrimSpace(strings.Join(text, "\n"))
	if joined != "" && !strings.HasPrefix(joined, "<system-reminder>") {
		out = append(out, Turn{At: e.Timestamp, Role: e.Type, Text: joined})
	}
	return append(out, calls...)
}

// Dialog is a person's answer to one AskUserQuestion call: the questions it
// showed and the selected option plus any notes or free text, verbatim.
type Dialog struct {
	At        time.Time
	Questions []string
	Text      string
}

// DialogsSince returns the question-dialog answers in the whole
// transcript at path whose timestamp is not before since.
func DialogsSince(path string, since time.Time) ([]Dialog, error) {
	var out []Dialog
	if _, err := tail(path, 0, false, func(d Dialog) {
		if since.IsZero() || !d.At.IsZero() && !d.At.Before(since) {
			out = append(out, d)
		}
	}); err != nil {
		return nil, err
	}
	return out, nil
}

func askQuestions(input json.RawMessage) []string {
	var in struct {
		Questions []struct {
			Question string `json:"question"`
		} `json:"questions"`
	}
	if json.Unmarshal(input, &in) != nil {
		return nil
	}
	out := []string{}
	for _, q := range in.Questions {
		out = append(out, q.Question)
	}
	return out
}

// askUserTurnText extracts a person's AskUserQuestion answer (the selected
// option plus any notes or free text, verbatim) and the questions it answers
// from a user entry's toolUseResult or tool_result blocks. Only a result of a
// call in asks (by tool_use id) counts: another tool's result is no answer.
func askUserTurnText(raw json.RawMessage, blocks []block, asks map[string][]string) (string, []string) {
	var questions []string
	ask := false
	for _, b := range blocks {
		if q, ok := asks[b.ToolUseID]; ok && b.Type == blockToolResult {
			ask = true
			questions = append(questions, q...)
		}
	}
	if !ask {
		return "", nil
	}
	if s := askToolUseResultText(raw, questions); s != "" {
		return s, questions
	}
	var parts []string
	for _, b := range blocks {
		if _, ok := asks[b.ToolUseID]; ok && b.Type == blockToolResult && !b.IsError {
			if s := askToolResultBlockText(resultText(b.Content)); s != "" {
				parts = append(parts, s)
			}
		}
	}
	return strings.Join(parts, "\n"), questions
}

func askToolUseResultText(raw json.RawMessage, asked []string) string {
	if len(raw) == 0 {
		return ""
	}
	var r struct {
		Questions []struct {
			Question string `json:"question"`
		} `json:"questions"`
		Answers      map[string]string          `json:"answers"`
		Annotations  map[string]json.RawMessage `json:"annotations"`
		Notes        json.RawMessage            `json:"notes"`
		Response     string                     `json:"response"`
		AFKTimeoutMs int                        `json:"afkTimeoutMs"`
		FollowUp     bool                       `json:"followUp"`
	}
	if json.Unmarshal(raw, &r) != nil || r.AFKTimeoutMs > 0 || r.FollowUp {
		return ""
	}
	if len(r.Answers) == 0 && len(r.Annotations) == 0 && len(r.Notes) == 0 && strings.TrimSpace(r.Response) == "" {
		return ""
	}
	var keys []string
	seen := map[string]bool{}
	for _, q := range r.Questions {
		asked = append(asked, q.Question)
	}
	for _, q := range asked {
		if q != "" && !seen[q] {
			seen[q] = true
			keys = append(keys, q)
		}
	}
	var extraKeys []string
	for k := range r.Answers {
		if !seen[k] {
			seen[k] = true
			extraKeys = append(extraKeys, k)
		}
	}
	for k := range r.Annotations {
		if !seen[k] {
			seen[k] = true
			extraKeys = append(extraKeys, k)
		}
	}
	slices.Sort(extraKeys)
	keys = append(keys, extraKeys...)
	notesByQ, topNotes := parseDialogNotes(r.Notes)
	var parts []string
	for _, k := range keys {
		annNotes, unanswered := parseDialogAnnotation(r.Annotations[k])
		if unanswered != "" {
			continue
		}
		ans := strings.TrimSpace(r.Answers[k])
		if ans == "(notes only)" {
			ans = ""
		}
		note := joinDialogParts(annNotes, notesByQ[k])
		if part := joinOptionAndNote(ans, note); part != "" {
			parts = append(parts, part)
		}
	}
	for _, extra := range []string{topNotes, strings.TrimSpace(r.Response)} {
		if extra == "" || slices.Contains(parts, extra) {
			continue
		}
		if len(parts) == 1 && !strings.Contains(parts[0], " — ") {
			parts[0] = joinOptionAndNote(parts[0], extra)
		} else {
			parts = append(parts, extra)
		}
	}
	return strings.Join(parts, "\n")
}

func parseDialogAnnotation(raw json.RawMessage) (notes, unanswered string) {
	if len(raw) == 0 {
		return "", ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return strings.TrimSpace(s), ""
	}
	var ann struct {
		Notes      string `json:"notes"`
		Unanswered string `json:"unanswered"`
	}
	if json.Unmarshal(raw, &ann) == nil {
		return strings.TrimSpace(ann.Notes), strings.TrimSpace(ann.Unanswered)
	}
	return "", ""
}

func parseDialogNotes(raw json.RawMessage) (byQuestion map[string]string, single string) {
	if len(raw) == 0 {
		return nil, ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return nil, strings.TrimSpace(s)
	}
	var m map[string]string
	if json.Unmarshal(raw, &m) == nil {
		return m, ""
	}
	return nil, ""
}

func joinDialogParts(parts ...string) string {
	var out []string
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" && !slices.Contains(out, p) {
			out = append(out, p)
		}
	}
	return strings.Join(out, "\n")
}

func joinOptionAndNote(ans, note string) string {
	ans, note = strings.TrimSpace(ans), strings.TrimSpace(note)
	switch {
	case ans == "":
		return note
	case note == "" || ans == note || strings.HasSuffix(ans, " — "+note):
		return ans
	default:
		return ans + " — " + note
	}
}

// Only simplify a complete single pair; a suffix match would discard earlier answers.
var askResultPair = regexp.MustCompile(`^"[^"]*"=(?:"([^"]*)"|\(no option selected\))(?:\s+selected preview:\n.*?)?(?:\s+(?:user's\s+)?notes:\s*(.+))?$`)

func askToolResultBlockText(s string) string {
	s = strings.TrimSpace(s)
	if rest, ok := strings.CutPrefix(s, "The user responded:"); ok {
		return strings.TrimSpace(rest)
	}
	for _, frame := range []struct{ prefix, suffix string }{
		{"Your questions have been answered: ", ". You can now continue with these answers in mind."},
		{"User has answered your questions: ", ". You can now continue with the user's answers in mind."},
		{"The user answered: ", ". Read the answers carefully — they may request clarification, changes, or that you not proceed — and follow what they actually say."},
	} {
		body, ok := strings.CutPrefix(s, frame.prefix)
		if !ok {
			continue
		}
		body = strings.TrimSuffix(strings.TrimSpace(body), frame.suffix)
		body = strings.TrimSuffix(strings.TrimSpace(body), ".")
		if m := askResultPair.FindStringSubmatch(strings.TrimSpace(body)); m != nil {
			ans := strings.TrimSpace(m[1])
			if ans == "(notes only)" {
				ans = ""
			}
			note := strings.Trim(strings.TrimSpace(m[2]), `"`)
			return joinOptionAndNote(ans, note)
		}
		return strings.TrimSpace(body)
	}
	return ""
}

// turnEndReasons are the stop reasons that close a turn: the model answered
// and stopped. A tool call round stops with another reason (its result feeds
// the next round of the same turn).
var turnEndReasons = map[string]bool{"end_turn": true, "stop_sequence": true}

// TurnDone reports whether the transcript at path proves its session's last
// turn ended: its last message of its own is an assistant message that
// stopped with an end reason. A tail that ends on a user message (a wake or
// a tool result the model has not answered yet), on an assistant message
// still running (a tool call, a stop reason missing while the CLI writes
// it), on a line only partly written, or says nothing at all (no file, no
// message) is not proof: false. A wake or resume appends the message it was
// given as a user message before its model answers, so a session in a turn
// a wake just started reads as not done.
func TurnDone(path string) bool {
	if path == "" {
		return false
	}
	f, err := os.Open(filepath.Clean(path))
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	off := max(fi.Size()-tailWindow, 0)
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		return false
	}
	raw, err := io.ReadAll(f)
	if err != nil {
		return false
	}
	if off > 0 {
		_, raw, _ = bytes.Cut(raw, []byte{'\n'}) // a partial first line
	}
	if !bytes.HasSuffix(raw, []byte{'\n'}) {
		return false // the last line is only partly written: the CLI is writing right now,
		// and that line may be the message that opens the session's next turn.
	}
	return lastTurnEnded(raw)
}

// lastTurnEnded reports whether the whole transcript lines end on a finished
// assistant message of the session's own.
func lastTurnEnded(raw []byte) bool {
	lines := bytes.Split(bytes.TrimRight(raw, "\n"), []byte{'\n'})
	for i := len(lines) - 1; i >= 0; i-- {
		line := bytes.TrimSpace(lines[i])
		if len(line) == 0 {
			continue
		}
		var e struct {
			Type        string `json:"type"`
			IsMeta      bool   `json:"isMeta"`
			IsSidechain bool   `json:"isSidechain"`
			Message     struct {
				StopReason string          `json:"stop_reason"`
				Content    json.RawMessage `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(line, &e) != nil || e.IsMeta || e.IsSidechain {
			continue // no message, or another session's (a subagent's)
		}
		if e.Type == roleUser {
			return false // a message or a tool result the model has not answered
		}
		if e.Type != roleAssistant {
			continue
		}
		if !turnEndReasons[e.Message.StopReason] {
			return false // still running: a tool call round, a line mid-write
		}
		return !bytes.Contains(e.Message.Content, []byte(`"`+blockToolUse+`"`))
	}
	return false // no message of its own: nothing proves a turn ended
}

// gistKeys are the tool input fields that say what a call does, the most
// telling first: a command's description before the command itself.
var gistKeys = []string{"description", "command", "file_path", "path", "pattern", "url", "query", "prompt", "to", "skill"}

// ToolGist is one line naming a tool call: its name and the first of its
// input's telling fields ("Bash: Run the tests").
func ToolGist(name string, input json.RawMessage) string {
	var in map[string]any
	if json.Unmarshal(input, &in) == nil {
		for _, k := range gistKeys {
			if v, ok := in[k].(string); ok && strings.TrimSpace(v) != "" {
				first, _, _ := strings.Cut(strings.TrimSpace(v), "\n")
				return name + ": " + first
			}
		}
		if qs, ok := in["questions"].([]any); ok {
			for _, item := range qs {
				if m, ok := item.(map[string]any); ok {
					if q, ok := m["question"].(string); ok && strings.TrimSpace(q) != "" {
						first, _, _ := strings.Cut(strings.TrimSpace(q), "\n")
						return name + ": " + first
					}
				}
			}
		}
	}
	return name
}

// modelWindow is how much of a transcript's end Model reads: the window
// Claude Desktop scans when it imports a session.
const modelWindow = 256 << 10

// Model returns the model of the transcript's last assistant message, which
// Claude Desktop takes as an imported session's model for every later turn;
// empty while the transcript holds none. A partial last line (the CLI still
// writing it) is skipped.
func Model(path string) (string, error) {
	f, err := os.Open(filepath.Clean(path))
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return "", err
	}
	off := max(fi.Size()-modelWindow, 0)
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		return "", err
	}
	raw, err := io.ReadAll(f)
	if err != nil {
		return "", err
	}
	if off > 0 {
		_, raw, _ = bytes.Cut(raw, []byte{'\n'})
	}
	var model string
	for line := range bytes.SplitSeq(raw, []byte{'\n'}) {
		var e struct {
			Type    string `json:"type"`
			Message struct {
				Model string `json:"model"`
			} `json:"message"`
		}
		if json.Unmarshal(line, &e) == nil && e.Type == roleAssistant && e.Message.Model != "" && e.Message.Model != "<synthetic>" {
			model = e.Message.Model
		}
	}
	return model, nil
}

// Called reports whether the transcript at path holds a call of a tool
// whose name ends in suffix that returned without an error: a posted
// Slack message is a slack_send_message call's result.
func Called(path, suffix string) (bool, error) {
	_, ok, err := CallResult(path, suffix)
	return ok, err
}

// CallResult is the text of the first result without an error of a call of
// a tool whose name ends in suffix in the transcript at path: a posted Slack
// message's channel and ts.
func CallResult(path, suffix string) (string, bool, error) {
	f, err := os.Open(filepath.Clean(path))
	if err != nil {
		return "", false, err
	}
	defer func() { _ = f.Close() }()
	calls := map[string]bool{}
	r := bufio.NewReaderSize(f, 1<<20)
	for {
		line, err := r.ReadBytes('\n')
		var e entry
		var blocks []struct {
			Type      string          `json:"type"`
			ID        string          `json:"id"`
			Name      string          `json:"name"`
			ToolUseID string          `json:"tool_use_id"`
			IsError   bool            `json:"is_error"`
			Content   json.RawMessage `json:"content"`
		}
		if json.Unmarshal(bytes.TrimSpace(line), &e) == nil && json.Unmarshal(e.Message.Content, &blocks) == nil {
			for _, b := range blocks {
				switch {
				case b.Type == blockToolUse && strings.HasSuffix(b.Name, suffix):
					calls[b.ID] = true
				case b.Type == blockToolResult && calls[b.ToolUseID] && !b.IsError:
					return resultText(b.Content), true, nil
				}
			}
		}
		if err == io.EOF {
			return "", false, nil
		}
		if err != nil {
			return "", false, err
		}
	}
}

// TranscriptCwd is the working directory the transcript at path records for
// its session: the first entry's cwd within firstWindow, "" for none.
func TranscriptCwd(path string) (string, error) {
	f, err := os.Open(filepath.Clean(path))
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	r := bufio.NewReaderSize(io.LimitReader(f, firstWindow), 1<<20)
	for {
		line, err := r.ReadBytes('\n')
		var e struct {
			Cwd string `json:"cwd"`
		}
		if json.Unmarshal(bytes.TrimSpace(line), &e) == nil && e.Cwd != "" {
			return e.Cwd, nil
		}
		if err == io.EOF {
			return "", nil
		}
		if err != nil {
			return "", err
		}
	}
}

// Background is a Bash command a session started in the background
// (run_in_background) whose completion notice its transcript does not hold.
type Background struct {
	Command, Description string
}

// taskNotice is the id a background task's completion notice names.
var taskNotice = regexp.MustCompile(`<task-id>([^<\\]+)</task-id>`)

// OpenBackground lists the background Bash commands of the transcript at path
// that no completion notice followed: a headless turn that ended on one never
// hears it finish, since the end of the turn is the end of its process.
func OpenBackground(path string) ([]Background, error) {
	f, err := os.Open(filepath.Clean(path))
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	launched := map[string]Background{} // by tool use id
	var tasks []string                  // tool use ids in launch order
	byTask := map[string]string{}       // task id → tool use id
	r := bufio.NewReaderSize(f, 1<<20)
	for {
		line, err := r.ReadBytes('\n')
		var e struct {
			Message struct {
				Content json.RawMessage `json:"content"`
			} `json:"message"`
			ToolUseResult json.RawMessage `json:"toolUseResult"`
		}
		var blocks []struct {
			Type      string `json:"type"`
			ID        string `json:"id"`
			Name      string `json:"name"`
			ToolUseID string `json:"tool_use_id"`
			Input     struct {
				Command         string `json:"command"`
				Description     string `json:"description"`
				RunInBackground bool   `json:"run_in_background"`
			} `json:"input"`
		}
		if json.Unmarshal(bytes.TrimSpace(line), &e) == nil {
			_ = json.Unmarshal(e.Message.Content, &blocks)
			var res struct {
				BackgroundTaskID string `json:"backgroundTaskId"`
			}
			_ = json.Unmarshal(e.ToolUseResult, &res)
			for _, b := range blocks {
				switch {
				case b.Type == blockToolUse && b.Name == "Bash" && b.Input.RunInBackground:
					launched[b.ID] = Background{Command: b.Input.Command, Description: b.Input.Description}
				case b.Type == blockToolResult && res.BackgroundTaskID != "":
					if _, ok := launched[b.ToolUseID]; ok {
						byTask[res.BackgroundTaskID] = b.ToolUseID
						tasks = append(tasks, b.ToolUseID)
					}
				}
			}
			for _, m := range taskNotice.FindAllSubmatch(line, -1) {
				delete(launched, byTask[string(m[1])])
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
	}
	var out []Background
	for _, id := range tasks {
		if b, ok := launched[id]; ok {
			out = append(out, b)
		}
	}
	return out, nil
}

// Answer is what a session did in the turns it ran since a message to it:
// its tool calls, in order, and the last text it wrote.
type Answer struct {
	Calls []AnswerCall
	Text  string
}

// AnswerCall is one tool call of an Answer: the tool, its input and its
// result, Done once the result came.
type AnswerCall struct {
	Name   string
	Input  json.RawMessage
	Result string
	Error  bool
	Done   bool
}

// ReadAnswer reads from the end of the transcript at path what its session
// did from since on.
func ReadAnswer(path string, since time.Time) (Answer, error) {
	f, err := os.Open(filepath.Clean(path))
	if err != nil {
		return Answer{}, err
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return Answer{}, err
	}
	off := max(fi.Size()-tailWindow, 0)
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		return Answer{}, err
	}
	var a Answer
	at := map[string]int{}
	r := bufio.NewReaderSize(f, 1<<20)
	for first := off > 0; ; first = false {
		line, err := r.ReadBytes('\n')
		if !first {
			a.add(bytes.TrimSpace(line), since, at)
		}
		if err == io.EOF {
			return a, nil
		}
		if err != nil {
			return a, err
		}
	}
}

// add takes one transcript line into a, when written from since on; at
// indexes a's calls by their tool_use id.
func (a *Answer) add(line []byte, since time.Time, at map[string]int) {
	var e entry
	var blocks []activityBlock
	if json.Unmarshal(line, &e) != nil || e.Timestamp.Before(since) || json.Unmarshal(e.Message.Content, &blocks) != nil {
		return
	}
	for _, b := range blocks {
		switch {
		case e.Type == "assistant" && b.Type == blockText && strings.TrimSpace(b.Text) != "":
			a.Text = strings.TrimSpace(b.Text)
		case e.Type == "assistant" && b.Type == blockToolUse:
			at[b.ID] = len(a.Calls)
			a.Calls = append(a.Calls, AnswerCall{Name: b.Name, Input: b.Input})
		case b.Type == blockToolResult:
			if i, ok := at[b.ToolUseID]; ok {
				c := &a.Calls[i]
				c.Result, c.Error, c.Done = resultText(b.Content), b.IsError, true
			}
		}
	}
}
