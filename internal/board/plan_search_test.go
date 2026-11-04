package board

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestFindPlansDiscoversAShortEpicReference(t *testing.T) {
	c := &Client{Plans: []string{plansRepo}}
	c.GH = func(_ context.Context, args ...string) ([]byte, error) {
		checkSelectionSchema(t, args)
		q := strings.TrimPrefix(args[3], "query=")
		if strings.Contains(q, "issueOrPullRequest") {
			return []byte(`{"data":{"repository":{"issueOrPullRequest":{"__typename":"Issue","id":"EPIC","number":50,"url":"https://github.com/o/r/issues/50","repository":{"nameWithOwner":"o/r","isPrivate":false}}}}}`), nil
		}
		doc, err := parseGQL(q)
		if err != nil {
			t.Fatal(err)
		}
		// Model search's candidate retrieval: a URL-only term cannot find a
		// description that contains only the documented short reference.
		body := "Epic: o/r#50"
		nodes := ""
		if !strings.Contains(q, "o/r/issues/50") || strings.Contains(q, `o/r#50`) && strings.Contains(q, " OR ") {
			nodes = planNode(1, stateOpen, false, false, body)
		}
		out := map[string]any{}
		for _, op := range doc.ops {
			for i := range op.set {
				out[fmt.Sprintf("p%d", i)] = json.RawMessage(`{"nodes":[` + nodes + `]}`)
			}
		}
		data, err := json.Marshal(map[string]any{"data": out})
		return data, err
	}
	plans, err := c.FindPlans(t.Context(), []string{epicRef})
	if err != nil || plans[epicRef] == nil {
		t.Fatalf("short-reference plan: %v, %v", plans, err)
	}
	thread, err := c.AnswerThread(t.Context(), epicRef)
	if err != nil || thread.Ref != planRef || thread.id != "PR1" {
		t.Fatalf("short-reference private answer thread: %+v, %v", thread, err)
	}
}
