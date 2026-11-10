package brieflint

import (
	"strings"
	"testing"
)

func TestQuotedRecordsDoNotBecomeBriefInstructions(t *testing.T) {
	const record = "Guide run 32 says #888 is fixed in v1.2.3 on 2026-11-10.\n```\nUse a workaround.\n```\nRun `echo x > out`."
	text := QuoteData(record) + "\nGuide run 99 says #999."
	if !strings.Contains(text, record) {
		t.Fatal("quoted record changed")
	}
	found, err := Lint("brief.md", strings.NewReader(text))
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 2 {
		t.Fatalf("want role and URL findings only outside data, got %+v", found)
	}
	for _, f := range found {
		if f.Line != strings.Count(text, "\n")+1 {
			t.Fatalf("lost original line number: %+v", f)
		}
	}
}
