package cmd

import (
	"bytes"
	"context"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"

	"github.com/giantswarm/beekeeper/internal/central/centraltest"
	"github.com/giantswarm/beekeeper/internal/state"
)

// reportTS is the report post's ts in the tests.
const reportTS = "100.000001"

// slackStandIn is a Slack MCP server stand-in: read_thread answers the
// thread set for a ts, add_reaction records its target.
type slackStandIn struct {
	mu        sync.Mutex
	threads   map[string]string
	fail      string
	reactions []string
}

func (s *slackStandIn) url(t *testing.T) string {
	srv := mcpserver.NewMCPServer("slack", "test")
	srv.AddTool(mcp.NewTool("read_thread"), func(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.fail != "" {
			return mcp.NewToolResultError(s.fail), nil
		}
		return mcp.NewToolResultText(s.threads[req.GetString("message_ts", "")]), nil
	})
	srv.AddTool(mcp.NewTool("add_reaction"), func(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.reactions = append(s.reactions, req.GetString("channel_id", "")+"/"+req.GetString("message_ts", "")+":"+req.GetString("emoji", ""))
		return mcp.NewToolResultText(`{"ok":true}`), nil
	})
	h := httptest.NewServer(mcpserver.NewStreamableHTTPServer(srv, mcpserver.WithStateLess(true)))
	t.Cleanup(h.Close)
	return h.URL + "/mcp"
}

const feedbackThread = `{"messages":"=== THREAD PARENT MESSAGE ===\nFrom: Ada (U1)\nTime: 2026-10-04 15:00:12 CEST\nMessage TS: 100.000001\nStatus 15:00\n\n=== THREAD REPLIES (3 total) ===\n\n--- Reply 1 of 3 ---\nFrom: Ada (U1)\nTime: 2026-10-04 15:04:00 CEST\nMessage TS: 100.000002\nboard pull 9: look at the CI first\n\n--- Reply 2 of 3 ---\nFrom: Bob (U2)\nTime: 2026-10-04 15:05:00 CEST\nMessage TS: 100.000003\nhi\n\n--- Reply 3 of 3 ---\nFrom: Ada (U1)\nTime: 2026-10-04 15:06:00 CEST\nMessage TS: 100.000004\nslow down please\n"}`

// feedbackWatch is a reporting standby watch whose feedback watch reads
// slack through a test muster; sent collects the deliveries.
func feedbackWatch(t *testing.T) (*watcher, *slackStandIn, *[]string, *bytes.Buffer) {
	t.Helper()
	dir := t.TempDir()
	w, _, out := reportingWatch(t, dir)
	slack := &slackStandIn{threads: map[string]string{reportTS: feedbackThread}}
	m := centraltest.New(t, slack.url(t), "slack")
	bin, _ := centraltest.Binary(t, m.URL, "token")
	w.cfg.Feedback.Context, w.cfg.Feedback.Server = gazelle, viaSlack
	w.cfg.Feedback.Every.Duration, w.cfg.Feedback.Window.Duration = 5*time.Minute, 24*time.Hour
	w.cfg.Central.Muster, w.cfg.Central.Timeout.Duration = bin, 10*time.Second
	var sent []string
	w.deliver = func(_ context.Context, q, msg string) error {
		sent = append(sent, q+" <- "+msg)
		return nil
	}
	err := w.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Supervisor = &state.Supervisor{Party: state.Party{Name: "Supervisor run 1", Session: "sup"}}
		st.Agents = []state.Agent{{Party: state.Party{Name: "Board pull 9", Session: "bp9"}}}
		st.ReportThreads = []state.ReportThread{
			{Report: "Status report 15:00", Channel: "C1", TS: reportTS, Posted: w.now.Add(-time.Hour)},
			{Report: "Status report yesterday", Channel: "C1", TS: "50.000001", Posted: w.now.Add(-25 * time.Hour)},
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return w, slack, &sent, out
}

func TestFeedbackDeliversTheAuthorsReplies(t *testing.T) {
	w, slack, sent, out := feedbackWatch(t)
	ctx := context.Background()
	w.readFeedback(ctx, w.app)
	want := []string{
		"Board pull 9 <- Feedback from Ada in Slack, a reply to the status report Status report 15:00: look at the CI first",
		"sup <- Feedback from Ada in Slack, a reply to the status report Status report 15:00: slow down please",
	}
	if strings.Join(*sent, "\n") != strings.Join(want, "\n") {
		t.Errorf("sent:\n%s", strings.Join(*sent, "\n"))
	}
	if r := strings.Join(slack.reactions, " "); r != "C1/100.000002:eyes C1/100.000004:eyes" {
		t.Errorf("reactions %s", r)
	}
	st, _ := w.store.Read()
	if len(st.ReportThreads) != 1 || st.ReportThreads[0].Seen != "100.000004" {
		t.Errorf("threads %+v: want the day-old one dropped, the reply seen", st.ReportThreads)
	}
	if !strings.Contains(out.String(), "FEEDBACK to Board pull 9, a reply to Status report 15:00: look at the CI first") ||
		!strings.Contains(out.String(), "FEEDBACK to the supervisor, a reply to Status report 15:00: slow down please") {
		t.Errorf("out:\n%s", out)
	}
	if verbs(t, w, "feedback.delivered") != 2 {
		t.Error("want two feedback.delivered events")
	}
	w.readFeedback(ctx, w.app)
	if len(*sent) != 2 {
		t.Errorf("read again, sent %v", *sent)
	}
}

func TestFeedbackKeepsAReplyNotDelivered(t *testing.T) {
	w, _, sent, out := feedbackWatch(t)
	deliver := w.deliver
	w.deliver = func(context.Context, string, string) error { return errors.New("no CLI") }
	w.readFeedback(context.Background(), w.app)
	st, _ := w.store.Read()
	if st.ReportThreads[0].Seen != "" || !strings.Contains(out.String(), "FEEDBACK on Status report 15:00 not delivered to Board pull 9, read again next time: no CLI") {
		t.Errorf("threads %+v; out:\n%s", st.ReportThreads, out)
	}
	w.deliver = deliver
	w.readFeedback(context.Background(), w.app)
	if len(*sent) != 2 {
		t.Errorf("sent %v once delivery works", *sent)
	}
}

func TestFeedbackUnreadableIsSaidOnce(t *testing.T) {
	w, slack, _, out := feedbackWatch(t)
	slack.fail = "execution_failed: missing_scope"
	w.readFeedback(context.Background(), w.app)
	w.readFeedback(context.Background(), w.app)
	if n := strings.Count(out.String(), "FEEDBACK UNREADABLE gazelle: x_slack_read_thread: execution_failed: missing_scope"); n != 1 {
		t.Errorf("said %d times; out:\n%s", n, out)
	}
	slack.fail = ""
	w.readFeedback(context.Background(), w.app)
	if !strings.Contains(out.String(), "ENDED") {
		t.Errorf("out:\n%s", out)
	}
}

func TestReporterPostRecordsItsThread(t *testing.T) {
	dir := t.TempDir()
	w, _, _ := reportingWatch(t, dir)
	w.cfg.Feedback.Context = gazelle
	ctx := context.Background()
	w.tendReporter(ctx, nil)
	st, _ := w.store.Read()
	proj := filepath.Join(w.cfg.Claude.ProjectsDir, "p")
	if err := os.MkdirAll(proj, 0o700); err != nil {
		t.Fatal(err)
	}
	post := `{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"mcp__claude_ai_Slack__slack_send_message","input":{}}]}}
{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":[{"type":"text","text":"{\"message_link\":\"https://x\",\"message_context\":{\"message_ts\":\"100.000001\",\"channel_id\":\"D1\"}}"}]}]}}
`
	if err := os.WriteFile(filepath.Join(proj, st.Report.Session+".jsonl"), []byte(post), 0o600); err != nil {
		t.Fatal(err)
	}
	w.now = reportNow.Add(3 * time.Minute)
	w.tendReporter(ctx, nil)
	st, _ = w.store.Read()
	if len(st.ReportThreads) != 1 || st.ReportThreads[0].Channel != "D1" || st.ReportThreads[0].TS != reportTS || st.ReportThreads[0].Report != st.Report.Name {
		t.Errorf("threads %+v", st.ReportThreads)
	}
}
