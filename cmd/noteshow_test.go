package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"

	"github.com/giantswarm/beekeeper/internal/central/centraltest"
	"github.com/giantswarm/beekeeper/internal/state"
)

const noteShowName = "show"

func TestNoteShow(t *testing.T) {
	body := strings.Repeat("A long line with unicode 🐝\n", 50000) + "last line"
	answer := strings.Repeat("The complete answer\n", 100) + "answer tail"
	for _, tc := range []struct {
		name, id, status          string
		json, defaulted, answered bool
	}{
		{name: "open text", id: "1", status: noteStatusOpen},
		{name: "open JSON", id: "#1", status: noteStatusOpen, json: true},
		{name: "defaulted text", id: "1", status: "defaulted", defaulted: true},
		{name: "defaulted JSON", id: "1", status: "defaulted", json: true, defaulted: true},
		{name: "answered text", id: "1", status: closedAnswered, answered: true},
		{name: "answered JSON", id: "1", status: closedAnswered, json: true, answered: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, out := noteApp(t)
			n := state.Note{ID: 1, For: notePerson, Text: body, Default: dfltOK}
			if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
				switch {
				case tc.answered:
					return []state.Event{{Verb: "note-show.unrelated", Detail: strings.Repeat("z", 1_100_000)}, answerEvent(state.Party{}, &recordedAnswer{About: refTwo, By: notePerson, Via: viaCLI, Answer: answer}, &n)}, nil
				case tc.defaulted:
					st.Defaulted = []state.Note{n}
				default:
					st.Notes = []state.Note{n}
				}
				return nil, nil
			}); err != nil {
				t.Fatal(err)
			}
			a.json = tc.json
			c := a.noteCmd()
			c.SetArgs([]string{noteShowName, tc.id})
			if err := c.Execute(); err != nil {
				t.Fatal(err)
			}
			if tc.json {
				var got struct {
					ID                        int
					Text, For, Status, Answer string
				}
				if err := json.Unmarshal(out.Bytes(), &got); err != nil {
					t.Fatal(err)
				}
				if got.ID != n.ID || got.Text != body || got.For != notePerson || got.Status != tc.status {
					t.Fatal("JSON lost note fields")
				}
				if tc.answered && got.Answer != answer {
					t.Fatal("JSON lost answer")
				}
			} else {
				for _, want := range []string{body, "[for " + notePerson + "]", "[status: " + tc.status + "]"} {
					if !strings.Contains(out.String(), want) {
						t.Fatalf("output lost %d-byte field", len(want))
					}
				}
				if tc.answered && !strings.Contains(out.String(), answer) {
					t.Fatal("output lost answer")
				}
			}
			st, err := a.store.Read()
			if err != nil {
				t.Fatal(err)
			}
			if !tc.answered && len(st.Notes)+len(st.Defaulted) != 1 {
				t.Fatal("show changed state")
			}
		})
	}
}

func TestNoteShowRejectsMissingAndInvalidIDs(t *testing.T) {
	for _, args := range [][]string{{}, {"1", "2"}, {"0"}, {"-1"}, {"bad"}, {"23"}, {"elsewhere#1"}} {
		t.Run(strings.Join(args, ","), func(t *testing.T) {
			a, out := noteApp(t)
			c := a.noteCmd()
			c.SilenceUsage, c.SilenceErrors = true, true
			c.SetArgs(append([]string{"show"}, args...))
			if err := c.Execute(); err == nil {
				t.Fatal("expected error")
			}
			if out.Len() != 0 {
				t.Fatal("failed lookup printed a note")
			}
		})
	}
}

func TestNoteShowAnsweredAudience(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		for _, asJSON := range []bool{false, true} {
			t.Run(fmt.Sprintf("legacy=%t/json=%t", legacy, asJSON), func(t *testing.T) {
				a, out := noteApp(t)
				const audience = "team:ops"
				const words = "approved"
				n := state.Note{ID: 1, For: audience, Text: laneQuestion, By: state.Party{Name: agentOne}}
				e := answerEvent(state.Party{}, &recordedAnswer{About: refTwo, By: notePerson, Via: viaCLI, Answer: words}, &n)
				if legacy {
					e.NoteSnapshot = nil // Older events retained only the textual detail.
					e.Detail = strings.Replace(e.Detail, ` for "team:ops"`, "", 1)
				}
				if err := a.store.Log(state.Event{Verb: "note-show.unrelated", Detail: strings.Repeat("z", 1_100_000)}, e); err != nil {
					t.Fatal(err)
				}
				a.json = asJSON
				c := a.noteCmd()
				c.SetArgs([]string{noteShowName, "#1"})
				if err := c.Execute(); err != nil {
					t.Fatal(err)
				}
				wantFor := audience
				if asJSON {
					var got struct{ For, Text, Answer string }
					if err := json.Unmarshal(out.Bytes(), &got); err != nil {
						t.Fatal(err)
					}
					if legacy {
						wantFor = ""
					}
					if got.For != wantFor || got.Text != n.Text || got.Answer != words {
						t.Fatalf("recovered note: %+v", got)
					}
				} else {
					if legacy {
						wantFor = "unknown (not retained)"
					}
					if !strings.Contains(out.String(), "[for "+wantFor+"]") {
						t.Fatalf("audience: %s", out)
					}
				}
			})
		}
	}
}

func TestNoteShowCentralIDs(t *testing.T) {
	for _, tc := range []struct {
		name, id             string
		local, fail, wantErr bool
	}{
		{name: "central qualified", id: lab + "#41"},
		{name: "central bare", id: "41"},
		{name: "ambiguous", id: "41", local: true, wantErr: true},
		{name: "explicit local", id: "#41", local: true},
		{name: "local during outage", id: "#41", local: true, fail: true},
		{name: "central outage", id: lab + "#41", fail: true, wantErr: true},
		{name: "central missing", id: lab + "#42", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := mcpserver.NewMCPServer(mailServer, "test")
			srv.AddTool(mcp.NewTool("note_list"), func(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				if tc.fail {
					return mcp.NewToolResultError("central unavailable"), nil
				}
				return mcp.NewToolResultStructured(map[string]any{keyNotes: []state.Note{{ID: 41, For: notePerson, Text: fullMemo}}}, keyNotes), nil
			})
			h := httptest.NewServer(mcpserver.NewStreamableHTTPServer(srv, mcpserver.WithStateLess(true)))
			defer h.Close()
			a, out := noteApp(t)
			m := centraltest.New(t, h.URL+"/mcp", mailServer)
			bin, _ := centraltest.Binary(t, m.URL, "token")
			a.cfg.Central.Context, a.cfg.Central.Server, a.cfg.Central.Muster = lab, mailServer, bin
			a.cfg.Central.Timeout.Duration = 10 * time.Second
			if tc.local {
				if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
					st.Notes = []state.Note{{ID: 41, For: samName, Text: fullMemo}}
					return nil, nil
				}); err != nil {
					t.Fatal(err)
				}
			}
			c := a.noteCmd()
			c.SilenceUsage, c.SilenceErrors = true, true
			c.SetArgs([]string{noteShowName, tc.id})
			err := c.Execute()
			if (err != nil) != tc.wantErr {
				t.Fatalf("error = %v, want error %v", err, tc.wantErr)
			}
			if !tc.wantErr && !strings.Contains(out.String(), fullMemo) {
				t.Fatal("lost central/local body")
			}
			if !tc.wantErr && strings.HasPrefix(tc.id, "#") && !strings.Contains(out.String(), "[for "+samName+"]") {
				t.Fatal("did not select explicit local note")
			}
		})
	}
}
