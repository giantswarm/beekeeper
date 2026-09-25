package claude

import (
	"math"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/config"
)

var prices = config.Metrics{Models: config.DefaultModels}

// window.jsonl is the last 512 KiB of a real transcript of this machine,
// its user and assistant lines stripped to their type, timestamps, usage
// and block types (ids renumbered, text replaced by "x"). The expected
// figures are jq's over the same file: usage summed once per message id.
func TestScanActivityRealWindow(t *testing.T) {
	buf, err := os.ReadFile("testdata/transcript/window.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	last := time.Date(2026, 9, 24, 15, 8, 34, 689e6, time.UTC)
	a := scanActivity(buf, true, last.Add(10*time.Minute))
	a.Price(prices)

	want := Tokens{Input: 90, CacheWrite1h: 66155, CacheRead: 5855326, Output: 34750}
	if a.Total.Tokens != want {
		t.Errorf("tokens %+v, want %+v", a.Total.Tokens, want)
	}
	if a.Total.ToolCalls != 45 || a.Total.ToolErrors != 4 || a.Total.Turns != 1 {
		t.Errorf("calls %d errors %d turns %d, want 45 4 1", a.Total.ToolCalls, a.Total.ToolErrors, a.Total.Turns)
	}
	if a.Context != 163018 || a.ContextWindow != 1_000_000 || a.Model != "claude-opus-5-5" {
		t.Errorf("context %d of %d (%s)", a.Context, a.ContextWindow, a.Model)
	}
	if a.Total.CostUSD == nil || math.Abs(*a.Total.CostUSD-2.3956652) > 1e-9 {
		t.Errorf("cost %v, want 2.3956652", a.Total.CostUSD)
	}
	if !a.Since.Equal(time.Date(2026, 9, 24, 14, 49, 4, 700e6, time.UTC)) {
		t.Errorf("since %s", a.Since)
	}
	if a.Total.Busy <= 0 || a.Total.Busy > last.Sub(a.Since) {
		t.Errorf("busy %s outside (0, %s]", a.Total.Busy, last.Sub(a.Since))
	}
	if a.LastHour.Tokens != a.Total.Tokens || a.LastHour.ToolCalls != a.Total.ToolCalls {
		t.Errorf("the whole window is in the last hour: %+v", a.LastHour)
	}

	later := scanActivity(buf, true, last.Add(2*time.Hour))
	later.Price(prices)
	if later.LastHour.Tokens.Sum() != 0 || later.LastHour.ToolCalls != 0 || *later.LastHour.CostUSD != 0 {
		t.Errorf("two hours on, the last hour is empty: %+v", later.LastHour)
	}
}

func TestScanActivityDropsThePartialFirstLine(t *testing.T) {
	buf := []byte(`sage":{"input_tokens":5}}}` + "\n" + assistant("m1", "claude-opus-5-5", `"input_tokens":1,"output_tokens":2`, ""))
	a := scanActivity(buf, false, testNow)
	if a.Whole || a.Total.Tokens != (Tokens{Input: 1, Output: 2}) {
		t.Errorf("whole %v tokens %+v", a.Whole, a.Total.Tokens)
	}
}

var testNow = time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)

func assistant(id, model, usage, content string) string {
	if content == "" {
		content = `[{"type":"text","text":"x"}]`
	}
	return `{"type":"assistant","timestamp":"2026-09-25T11:50:00Z","message":{"id":"` + id + `","model":"` + model +
		`","usage":{` + usage + `},"content":` + content + "}}\n"
}

func toolUse(id, name, input string) string {
	return assistant("m-"+id, "claude-opus-5-5", `"output_tokens":1`,
		`[{"type":"tool_use","id":"`+id+`","name":"`+name+`","input":`+input+`}]`)
}

func toolResult(id string, isError bool) string {
	e := ""
	if isError {
		e = `,"is_error":true`
	}
	return `{"type":"user","timestamp":"2026-09-25T11:50:01Z","message":{"content":[{"type":"tool_result","tool_use_id":"` + id + `"` + e + `}]}}` + "\n"
}

func TestScanActivityRepeatsAndGitHubCalls(t *testing.T) {
	var b strings.Builder
	for _, id := range []string{"a", "b", "c"} {
		b.WriteString(toolUse(id, "Bash", `{"command":"go test ./... -run X"}`))
		b.WriteString(toolResult(id, true))
	}
	b.WriteString(toolUse("d", "Bash", `{"command":"cd x && gh pr view 1 --repo o/r | jq ."}`))
	b.WriteString(toolResult("d", false))
	b.WriteString(toolUse("e", "Bash", `{"command":"/home/u/.go/bin/devctl pr wait o/r 2"}`))
	b.WriteString(toolUse("f", "mcp__github__get_issue", `{"issue":3}`))
	b.WriteString(toolUse("g", "Bash", `{"command":"echo ghost; ls devctl-wt"}`))
	b.WriteString(toolResult("g", true))
	b.WriteString(`{"type":"user","timestamp":"2026-09-25T11:51:00Z","message":{"content":"x"}}` + "\n")
	b.WriteString(`{"type":"user","timestamp":"2026-09-25T11:51:00Z","isMeta":true,"message":{"content":"x"}}` + "\n")
	b.WriteString(`{"type":"user","timestamp":"2026-09-25T11:51:00Z","message":{"content":"<system-reminder>x"}}` + "\n")
	b.WriteString(`{"type":"user","timestamp":"2026-09-25T11:51:00Z","message":{"content":"<bash-input>ls</bash-input>"}}` + "\n")

	a := scanActivity([]byte(b.String()), true, testNow)
	c := a.LastHour
	if c.ToolCalls != 7 || c.ToolErrors != 4 || c.GitHubCalls != 3 || c.Turns != 1 {
		t.Errorf("calls %d errors %d github %d turns %d, want 7 4 3 1", c.ToolCalls, c.ToolErrors, c.GitHubCalls, c.Turns)
	}
	if c.SameErrors != 3 || c.SameError != "Bash: go test ./... -run" {
		t.Errorf("same errors %d %q", c.SameErrors, c.SameError)
	}
}

func TestPriceUnknownModelsAndFastMode(t *testing.T) {
	buf := assistant("m1", "claude-opus-5-5", `"input_tokens":1000000`, "") +
		assistant("m2", "claude-opus-9", `"input_tokens":10`, "") +
		assistant("m3", "claude-opus-5-5", `"input_tokens":10,"speed":"fast"`, "") +
		assistant("m4", "<synthetic>", `"input_tokens":0`, "")
	a := scanActivity([]byte(buf), true, testNow)
	a.Price(prices)
	if a.Total.CostUSD != nil || strings.Join(a.Total.CostUnknown, ",") != "claude-opus-9,claude-opus-5-5 (fast)" {
		t.Errorf("cost %v unknown %v", a.Total.CostUSD, a.Total.CostUnknown)
	}

	known := scanActivity([]byte(assistant("m1", "claude-haiku-4-5-20251001", `"input_tokens":1000000,"cache_creation_input_tokens":1000000`, "")), true, testNow)
	known.Price(prices)
	// A usage without the TTL split wrote the 5-minute TTL: 1 + 1.25.
	if known.Total.CostUSD == nil || math.Abs(*known.Total.CostUSD-2.25) > 1e-9 || known.ContextWindow != 200_000 {
		t.Errorf("cost %v window %d", known.Total.CostUSD, known.ContextWindow)
	}
}

func TestCountsAddKeepsAnUnknownCost(t *testing.T) {
	one, two := 1.0, 2.0
	var c Counts
	c.Add(Counts{ToolCalls: 1, CostUSD: &one})
	c.Add(Counts{ToolCalls: 2, CostUSD: &two})
	if *c.CostUSD != 3 || c.ToolCalls != 3 || one != 1 {
		t.Fatalf("sum %+v", c)
	}
	c.Add(Counts{CostUnknown: []string{"claude-x"}})
	c.Add(Counts{CostUSD: &one})
	if c.CostUSD != nil || len(c.CostUnknown) != 1 {
		t.Errorf("an unknown cost stays unknown: %+v", c)
	}
}
