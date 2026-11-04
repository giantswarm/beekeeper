package cmd

import (
	"bytes"
	"reflect"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/identity"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/state/kube"
)

const noteListTool = "note_list"

func TestNoteToolsScope(t *testing.T) {
	scheme, err := kube.Scheme()
	if err != nil {
		t.Fatal(err)
	}
	srv := &server{store: kube.New(fake.NewClientBuilder().WithScheme(scheme).Build())}
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	notes := []state.Note{
		{ID: 1, For: "READER@example.com", Text: "personal decision"},
		{ID: 2, For: "team:readers", Text: "team decision"},
		{ID: 3, For: "team:others", By: state.Party{Person: "READER@example.com"}, Text: "filed decision"},
		{ID: 4, For: "team:others", By: state.Party{Person: "other@example.com", Team: "others"}, Text: "private decision", Default: "private default", StatusQuo: "private context"},
		{ID: 5, For: "colleague@example.com", By: state.Party{Team: "readers"}, Text: "colleague decision"},
		{ID: 6, Text: "unaddressed decision"},
	}
	if err := store.Update(func(st *state.State) ([]state.Event, error) {
		st.Notes = notes
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		who  identity.Caller
		want []state.Note
	}{
		{"person and team", identity.Caller{Email: "reader@example.com", Team: "readers"}, notes[:3]},
		{"person without team", identity.Caller{Email: "reader@example.com"}, []state.Note{notes[0], notes[2]}},
		{"unrelated caller", identity.Caller{Email: "stranger@example.com"}, []state.Note{}},
	} {
		for _, tool := range []serveTool{{tool: mcp.NewTool(noteListTool), run: toolNoteList}, {tool: mcp.NewTool("snapshot"), run: toolSnapshot}} {
			t.Run(tool.tool.Name+"/"+tc.name, func(t *testing.T) {
				out := &bytes.Buffer{}
				c := &call{ctx: t.Context(), s: srv, who: tc.who, out: out, app: &app{cfg: &config.Config{}, store: store, out: out}}
				got, err := tool.run(c, mcp.CallToolRequest{})
				if err != nil {
					t.Fatal(err)
				}
				var gotNotes []state.Note
				switch v := got.(type) {
				case map[string]any:
					gotNotes = v[keyNotes].([]state.Note)
				case snapshotView:
					gotNotes = v.Notes
				}
				if !reflect.DeepEqual(gotNotes, tc.want) {
					t.Errorf("notes = %#v, want %#v", got, tc.want)
				}
				for _, n := range notes {
					visible := false
					for _, want := range tc.want {
						visible = visible || n.ID == want.ID
					}
					if strings.Contains(out.String(), n.Text) != visible {
						t.Errorf("note %d: unexpected visibility in %q", n.ID, out.String())
					}
				}
				if len(tc.want) == 0 && !strings.Contains(out.String(), "no open notes") {
					t.Errorf("empty list: %q", out.String())
				}
			})
		}
	}
}
