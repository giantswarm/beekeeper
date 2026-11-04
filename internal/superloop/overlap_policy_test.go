package superloop

import (
	"strings"
	"testing"
	"time"
)

// A mutual-wait handoff must check the ownership policy of its target.
func TestMutualOverlapChecksTargetOwnership(t *testing.T) {
	const first, second = "o/a#1", "o/z#2"
	for _, scenario := range []string{"another team", "another author", "never named by changes", "missing never repository", "missing outside organisation", "closed target", "headless target", "unknown ownership"} {
		t.Run(scenario, func(t *testing.T) {
			target := first
			d := desk()
			p := Pull{Ref: first, URL: "https://github.com/o/a/pull/1", State: Open, Head: head1, Author: me, Team: ourTeam}
			present := true
			switch scenario {
			case "another team":
				p.Team = teamB
			case "another author":
				p.Author = "colleague"
			case "never named by changes":
				p.Files = []string{"tool/values.yaml"}
			case "closed target":
				p.State = Closed
			case "headless target":
				p.Head = ""
			case "unknown ownership":
				d.TeamsUnknown = true
			case "missing never repository":
				d.Never = append(d.Never, "o/a")
				present = false
			case "missing outside organisation":
				target = "a/a#1"
				present = false
			}
			in := Input{
				Desk: d, Now: t0.Add(time.Minute),
				Workers: []Worker{
					{Name: "worker A", Serves: target, ParkedOn: second, ParkedAt: t0, Waits: "overlapping PR"},
					{Name: "worker B", Serves: second, ParkedOn: target, ParkedAt: t0, Waits: "overlapping PR"},
				},
				Pulls: map[string]Pull{
					second: {Ref: second, URL: "https://github.com/o/z/pull/2", State: Open, Head: head2, Author: me, Team: ourTeam},
				},
			}
			if present {
				in.Pulls[first] = p
			}
			for _, a := range Plan(in) {
				if strings.EqualFold(a.Ref, target) && a.Kind == KindOwner {
					t.Errorf("assigned excluded or unread target through mutual overlap: %+v", a)
				}
			}
		})
	}
}
