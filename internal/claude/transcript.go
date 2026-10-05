package claude

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"regexp"
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

// tailWindow is how much of a transcript's end is read: tool results make
// lines large, and the last few turns are what is asked for.
const tailWindow = 2 << 20

// Tail returns the last n turns of the transcript at path.
func Tail(path string, n int) ([]Turn, error) { return tail(path, n, false) }

// Follow returns the last n turns of the transcript at path with the
// model's tool calls among them: what a session does right now, as the
// person's screen follows it.
func Follow(path string, n int) ([]Turn, error) { return tail(path, n, true) }

func tail(path string, n int, tools bool) ([]Turn, error) {
	f, err := os.Open(filepath.Clean(path))
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	off := max(fi.Size()-tailWindow, 0)
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
	for {
		line, err := r.ReadBytes('\n')
		turns = append(turns, parseTurns(bytes.TrimSpace(line), tools)...)
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
	}
	if n > 0 && len(turns) > n {
		turns = turns[len(turns)-n:]
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
}

type block struct {
	Type  string          `json:"type"`
	Text  string          `json:"text"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

func parseTurn(line []byte) (Turn, bool) {
	ts := parseTurns(line, false)
	if len(ts) == 0 {
		return Turn{}, false
	}
	return ts[0], true
}

// parseTurns reads one transcript line: its text as one turn and, with
// tools, a turn for each tool call after it.
func parseTurns(line []byte, tools bool) []Turn {
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
			case tools && b.Type == blockToolUse && b.Name != "":
				calls = append(calls, Turn{At: e.Timestamp, Role: RoleTool, Text: ToolGist(b.Name, b.Input)})
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
