package cmd

import (
	"context"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"

	"github.com/giantswarm/beekeeper/internal/central/centraltest"
	"github.com/giantswarm/beekeeper/internal/state"
)

// noteAddStandIn is beekeeper serve's note_add: it files each call's
// decision as note 41 on, and keeps the arguments.
type noteAddStandIn struct {
	mu    sync.Mutex
	calls []map[string]any
}

func (s *noteAddStandIn) url(t *testing.T) string {
	srv := mcpserver.NewMCPServer(mailServer, "test")
	srv.AddTool(mcp.NewTool("note_add"), func(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.calls = append(s.calls, req.GetArguments())
		n := state.Note{ID: 40 + len(s.calls), For: req.GetString(paramFor, ""), Text: req.GetString(paramText, "")}
		return mcp.NewToolResultStructured(n, "note #41"), nil
	})
	h := httptest.NewServer(mcpserver.NewStreamableHTTPServer(srv, mcpserver.WithStateLess(true)))
	t.Cleanup(h.Close)
	return h.URL + "/mcp"
}

// centralNoteApp is noteApp on a machine whose central instance is the
// stand-in s behind a test muster.
func centralNoteApp(t *testing.T, s *noteAddStandIn) (*app, *strings.Builder, *centraltest.Muster) {
	t.Helper()
	a, _ := noteApp(t)
	var out strings.Builder
	a.out = &out
	m := centraltest.New(t, s.url(t), mailServer)
	bin, _ := centraltest.Binary(t, m.URL, "token")
	a.cfg.Central.Context, a.cfg.Central.Server = lab, mailServer
	a.cfg.Central.Muster, a.cfg.Central.Timeout.Duration = bin, 10*time.Second
	a.cfg.Identity.Host = mailHost
	return a, &out, m
}

func TestNoteAddFilesDecisionsCentrally(t *testing.T) {
	s := &noteAddStandIn{}
	a, out, _ := centralNoteApp(t, s)
	if err := addNote(a, decisionNote("--recommend", "1", "approve "+prURL+"?")...); err != nil {
		t.Fatal(err)
	}
	if len(s.calls) != 1 {
		t.Fatalf("note_add called %d times, want once", len(s.calls))
	}
	c := s.calls[0]
	due := relayNow.Add(3 * time.Hour).UTC().Format(time.RFC3339)
	if c[paramText] != "approve "+prURL+"?" || c[paramFor] != notePerson || c[paramKind] != noteDecision || c["due"] != due ||
		c["default"] != dfltOK || c["status_quo"] != sqNow || c["why"] != whyNow || c["recommend"] != float64(1) ||
		c[paramAgent] != agentOne || c[paramHost] != mailHost {
		t.Errorf("note_add arguments %v", c)
	}
	if opts, _ := c["options"].([]any); len(opts) != 2 || opts[0] != optMerge {
		t.Errorf("options %v", c["options"])
	}
	if refs, _ := c[paramRefs].([]any); len(refs) != 1 || refs[0] != decisionRef {
		t.Errorf("refs %v", c[paramRefs])
	}
	if !strings.HasPrefix(out.String(), "note #41 (central "+lab) {
		t.Errorf("out %q, want the central note's number", out.String())
	}
	st, _ := a.store.Read()
	if len(st.Notes) != 0 {
		t.Errorf("the machine keeps %v: the decision is the central instance's", st.Notes)
	}
	evs, _ := a.store.Events(0, nil)
	if !slices.ContainsFunc(evs, func(e state.Event) bool {
		return e.Verb == "note.add" && strings.HasPrefix(e.Detail, "#41 (central "+lab+")")
	}) {
		t.Errorf("no note.add event of the central note in %v", evs)
	}
}

func TestNoteAddKeepsLoginsHoldsAndFoldsLocal(t *testing.T) {
	s := &noteAddStandIn{}
	a, _, _ := centralNoteApp(t, s)
	if err := addNote(a, flagFor, samName, "--kind", noteLogin, "--until", "true", "sign in to muster"); err != nil {
		t.Fatal(err)
	}
	if err := addNote(a, append(slices.Clone(samArgs), "--fold-into", "1", "which lane?")...); err != nil {
		t.Fatal(err)
	}
	if len(s.calls) != 0 {
		t.Errorf("note_add called with %v: sign-ins and folds are the machine's", s.calls)
	}
}

func TestNoteAddFilesLocallyWhileCentralIsUnreachable(t *testing.T) {
	s := &noteAddStandIn{}
	a, out, m := centralNoteApp(t, s)
	m.Down(true)
	if err := addNote(a, decisionNote("approve "+prURL+"?")...); err != nil {
		t.Fatal(err)
	}
	st, _ := a.store.Read()
	if len(st.Notes) != 1 || !strings.Contains(out.String(), "filed on this machine instead") || !strings.Contains(out.String(), "note #1") {
		t.Errorf("notes %v, out %q: want the decision filed on the machine, as stderr says", st.Notes, out.String())
	}
}

// A failed central read prints a warning even when the caller suppresses
// Cobra's error printing, while the local notes remain available.
func TestCentralNoteListWarns(t *testing.T) {
	const listVerb = "list"
	for _, args := range [][]string{{listVerb}, {listVerb, "--full"}, {}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			a, out, m := centralNoteApp(t, &noteAddStandIn{})
			m.Down(true)
			if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
				st.Notes = []state.Note{{ID: 1, Text: "local decision"}}
				return nil, nil
			}); err != nil {
				t.Fatal(err)
			}
			c := a.noteCmd()
			c.SetArgs(args)
			c.SetOut(out)
			c.SetErr(out)
			c.SilenceErrors = true
			c.SilenceUsage = true
			err := c.Execute()
			if Code(err) != ExitCentral || !strings.Contains(out.String(), "warning: ") || !strings.Contains(out.String(), "local decision") || !strings.Contains(out.String(), "central instance's notes (lab) are not listed") {
				t.Fatalf("list: %v: %s", err, out.String())
			}
		})
	}
}
