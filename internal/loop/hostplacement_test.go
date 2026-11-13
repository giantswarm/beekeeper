package loop

import (
	"strings"
	"testing"
)

// paceAnswer is a capacity --json answer whose pick is harness/model on
// provider, its candidate behind pace by behind ("" for no pace reading).
func paceAnswer(harness, model, provider, behind string) string {
	pace := ""
	if behind != "" {
		pace = `,"behindPace":` + behind
	}
	return `{"room":1,"choice":{"harness":"` + harness + `","model":"` + model + `","provider":"` + provider +
		`","candidates":[{"harness":"` + harness + `","model":"` + model + `","provider":"` + provider + `","ok":true` + pace + `}]}}`
}

// TestPaceYield: a host passes the start on only while the coordinator's
// pick is behind its weekly pace and the host's pick spends another
// provider; a host on the same provider, a pick on or ahead of its pace, a
// self-hosted pick or an unreadable answer keeps hosts-first placement.
func TestPaceYield(t *testing.T) {
	claudeHost := paceAnswer("claude", "sonnet", "claude", "17")
	for _, tc := range []struct {
		name, local, host string
		want              []string
	}{
		{"agy behind, host on claude", paceAnswer("agy", "gemini-3-pro", "agy", "52.1"), claudeHost,
			[]string{"the coordinator's pick agy/gemini-3-pro is 52.1 points behind its weekly pace", "this host's pick is claude/sonnet"}},
		{"stderr after the answer", paceAnswer("agy", "gemini-3-pro", "agy", "52.1") + "\nwarning: usage probe slow\n", claudeHost,
			[]string{"agy/gemini-3-pro is 52.1 points behind"}},
		{"host on the same provider", paceAnswer("agy", "gemini-3-pro", "agy", "52.1"), paceAnswer("agy", "gemini-3-flash", "AGY", "52.1"), nil},
		{"pick on its pace", paceAnswer("agy", "gemini-3-pro", "agy", "0"), claudeHost, nil},
		{"pick ahead of its pace", paceAnswer("agy", "gemini-3-pro", "agy", "-8.5"), claudeHost, nil},
		{"self-hosted pick", paceAnswer("omp", "local/qwen", "local", ""), claudeHost, nil},
		{"unreadable coordinator answer", "capacity: state unreadable", claudeHost, nil},
		{"pick not among its candidates", `{"room":1,"choice":{"harness":"agy","model":"gemini-3-pro","provider":"agy","candidates":[{"harness":"agy","model":"gemini-3-flash","provider":"agy","ok":true,"behindPace":52.1}]}}`, claudeHost, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := PaceYield(tc.local, tc.host)
			if len(tc.want) == 0 {
				if got != "" {
					t.Fatalf("PaceYield = %q, want none", got)
				}
				return
			}
			for _, want := range tc.want {
				if !strings.Contains(got, want) {
					t.Errorf("PaceYield = %q, want it to contain %q", got, want)
				}
			}
		})
	}
}
