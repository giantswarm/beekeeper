package post

import (
	"strings"
	"testing"
	"time"
)

func TestCheckPasses(t *testing.T) {
	msg := "**Lab status, 23:00–00:00 EEST**\n" +
		"- [beekeeper#109](https://github.com/giantswarm/beekeeper/pull/109) v0.26.0\n" +
		"- [giantswarm/giantswarm#37853](https://github.com/giantswarm/giantswarm/issues/37853) epic\n" +
		"- note 84 and timer 53, see [the docs](https://example.com/x)\n" +
		"- `#12 in code` and &#35; and C# and a -> b\n" +
		"— written by Timo's agents (hourly status)"
	if p := Check(msg); len(p) != 0 {
		t.Errorf("Check = %q", p)
	}
}

func TestCheckRefuses(t *testing.T) {
	for _, c := range []struct{ name, msg, want string }{
		{"empty", "  \n", "empty"},
		{"a bare number", "merged #109 today", "#109 is not a link"},
		{"a note with a hash", "- note #84: the refine", `a beekeeper note or timer is "note 84"`},
		{"a ref in a session name", "- #37941 flash next — benchmarking", "#37941 is not a link"},
		{"a bare repo ref", "merged beekeeper#109 (epic giantswarm/giantswarm#37853)", "beekeeper#109 is not a link"},
		{"Slack's link syntax", "<https://github.com/giantswarm/beekeeper/pull/109|beekeeper#109>", "Slack's link syntax"},
		{"a wrong repository", "[beekeeper#109](https://github.com/giantswarm/devctl/pull/109)", "labelled beekeeper#109 but links giantswarm/devctl#109"},
		{"a wrong number", "[beekeeper#109](https://github.com/giantswarm/beekeeper/pull/108)", "labelled beekeeper#109 but links giantswarm/beekeeper#108"},
		{"a wrong owner", "[giantswarm/beekeeper#109](https://github.com/teemow/beekeeper/pull/109)", "labelled giantswarm/beekeeper#109"},
		{"a bare number as label", "[#109](https://github.com/giantswarm/beekeeper/pull/109)", "labelled #109"},
		{"a label without a ref", "[the reporter](https://github.com/giantswarm/beekeeper/pull/109)", "needs the label beekeeper#109"},
		{"a ref label on another link", "[beekeeper#109](https://example.com)", "links no GitHub pull request"},
	} {
		if p := strings.Join(Check(c.msg), "\n"); !strings.Contains(p, c.want) {
			t.Errorf("%s: Check(%q) = %q, want %q", c.name, c.msg, p, c.want)
		}
	}
}

func TestCheckTheFirstReport(t *testing.T) {
	// The shape of the 22:52 report: linked and bare references mixed.
	msg := "*Lab status — last hour (21:52–22:52 EEST)*\n\nMerged & shipped:\n" +
		"• [agent-platform#713](https://github.com/giantswarm/agent-platform/pull/713) v1.2.3, gpm#313\n" +
		"Waiting on Timo:\n• #84 /refine of #37817"
	if p := Check(msg); len(p) != 3 {
		t.Errorf("Check = %q, want the three bare references", p)
	}
}

func TestZone(t *testing.T) {
	athens, err := time.LoadLocation("Europe/Athens")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 26, 21, 0, 0, 0, time.UTC)
	if p := Zone("**Lab status, 23:00–00:00 EEST**\nmerged at 23:10", athens, now); len(p) != 0 {
		t.Errorf("Zone = %q", p)
	}
	for _, c := range []struct{ msg, want string }{
		{"*Hourly status (19:01–20:01 UTC)*", "first line names no EEST"},
		{"**Lab status, 23:00–00:00 EEST**\nmerged at 20:10 UTC", "a time is in UTC"},
	} {
		if p := strings.Join(Zone(c.msg, athens, now), "\n"); !strings.Contains(p, c.want) {
			t.Errorf("Zone(%q) = %q, want %q", c.msg, p, c.want)
		}
	}
	if p := Zone("*Lab status, 20:00–21:00 UTC*", time.UTC, now); len(p) != 0 {
		t.Errorf("a UTC person: %q", p)
	}
}

func TestUnlinked(t *testing.T) {
	for _, c := range []struct {
		name, text, here, want string
	}{
		{"bare #N without repo", "merged #7 today", "", "#7 without its full URL (https://github.com/<owner>/<repo>/issues/7)"},
		{"bare #N with here", "merged #7 today", "o/r", "#7 without its full URL (https://github.com/o/r/issues/7)"},
		{"bare #N with single URL repo", "merged https://github.com/o/r/pull/9 for #7", "", "#7 without its full URL (https://github.com/o/r/issues/7)"},
		{"owner/repo#N", "x/y#7 is open", "o/r", "x/y#7 without its full URL (https://github.com/x/y/issues/7)"},
		{"linked owner/repo#N", "approve o/r#7 (https://github.com/o/r/pull/7)", "", ""},
		{"linked bare #N", "approve #7 (https://github.com/o/r/issues/7)", "", ""},
		{"note and timer", "see note #3 and timer #4", "", ""},
		{"park --on", "beekeeper agents park --on o/r#7 and --on '#9'", "", ""},
		{"session name repo#N", "session beekeeper#1682: agy harness", "", ""},
		{"code span and fenced block", "in `#7` and ``o/r#8`` and:\n```sh\ngh pr view #9 o/r#10\n```\nand:\n~~~\n#11\n~~~", "", ""},
		{"URL anchor fragment", "see https://github.com/o/r/pull/7#issuecomment-12 and https://example.com/x#99", "", ""},
		{"HTML entity", "char &#35; and &#123;", "", ""},
	} {
		got := strings.Join(Unlinked(c.text, c.here), "; ")
		if got != c.want {
			t.Errorf("%s: Unlinked(%q, %q) = %q, want %q", c.name, c.text, c.here, got, c.want)
		}
	}
}
