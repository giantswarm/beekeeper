package merge

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/state"
)

var (
	keyDetail  = regexp.MustCompile(`^([\w.-]+/[\w.-]+)#(\d+)`)
	forDetail  = regexp.MustCompile(` for "(.*)"$`)
	exitDetail = regexp.MustCompile(` exit (\d+),`)
)

// decision is what the queue rule says when a session's merge arrives.
type decision struct {
	at      string
	key     string
	behind  string   // the merge it waits behind, "" when it runs
	waiting []string // the lane's waiting merges, in turn order
}

// replay drives the queue rule with an event trail: seeds (merge.queued …
// for), drops, a session's merge arriving (merge.queued, merging), its
// failure and its merge. It returns the rule's decision at every arrival.
func replay(t *testing.T, raw []byte) []decision {
	t.Helper()
	const ttl, seedTTL = 15 * time.Minute, 12 * time.Hour
	st := &state.State{}
	var out []decision
	pid := 100
	sc := bufio.NewScanner(bytes.NewReader(raw))
	for sc.Scan() {
		var e state.Event
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			t.Fatal(err)
		}
		k := keyDetail.FindStringSubmatch(e.Detail)
		if k == nil {
			continue
		}
		repo, pr := k[1], atoi(t, k[2])
		running := 0
		for _, m := range st.Merges {
			if m.Phase == state.Running {
				running = m.PID
			}
		}
		Prune(st, e.At, ttl, seedTTL, func(p int) bool { return p != 0 && p == running })
		find := func(phase string) int {
			return slices.IndexFunc(st.Merges, func(m state.Merge) bool { return m.Repo == repo && m.PR == pr && m.Phase == phase })
		}
		switch f := forDetail.FindStringSubmatch(e.Detail); {
		case e.Verb == "merge.queued" && f != nil:
			if find(state.Waiting) < 0 {
				st.Merges = append(st.Merges, state.Merge{Repo: repo, PR: pr, Lane: "agent-platform", By: state.Party{Name: f[1]},
					Phase: state.Waiting, Seeded: true, Joined: e.At, Seen: e.At})
			}
		case e.Verb == "merge.dropped", e.Verb == "merge.refused":
			st.Merges = slices.DeleteFunc(st.Merges, func(m state.Merge) bool {
				return m.Repo == repo && m.PR == pr && m.Phase == state.Waiting && (e.Verb == "merge.dropped" || !m.Seeded)
			})
		case e.Verb == "merge.queued" || e.Verb == "merging":
			if find(state.Running) >= 0 {
				continue
			}
			pid++
			i := find(state.Waiting)
			if i < 0 {
				st.Merges = append(st.Merges, state.Merge{Repo: repo, PR: pr, Lane: "agent-platform", Phase: state.Waiting, Joined: e.At})
				i = len(st.Merges) - 1
			}
			st.Merges[i].PID, st.Merges[i].By, st.Merges[i].Seen = pid, e.By, e.At
			q := Queue(st, "agent-platform")
			d := decision{at: e.At.UTC().Format("15:04:05"), key: fmt.Sprintf("%s#%d", repo, pr)}
			for _, m := range q.Waiting {
				d.waiting = append(d.waiting, m.Key())
			}
			present := func(m state.Merge) bool {
				return Present(m, e.At, ttl, func(p int) bool { return p != 0 && (p == pid || p == running) })
			}
			if ahead, ok := q.Ahead(repo, pr, present); ok {
				d.behind = ahead.Key()
			} else if q.Running != nil {
				d.behind = q.Running.Key()
			}
			out = append(out, d)
			if e.Verb == "merging" {
				m := &st.Merges[i]
				m.Phase, m.Seeded, m.Finished, m.Exit = state.Running, false, time.Time{}, 0
			}
		case e.Verb == "merge.failed":
			if i := find(state.Running); i >= 0 && !Failed(&st.Merges[i], atoi(t, exitDetail.FindStringSubmatch(e.Detail)[1]), e.At) {
				st.Merges = slices.Delete(st.Merges, i, i+1)
			}
		case e.Verb == "merged":
			st.Merges = slices.DeleteFunc(st.Merges, func(m state.Merge) bool { return m.Repo == repo && m.PR == pr && m.Phase == state.Running })
		}
	}
	return out
}

func atoi(t *testing.T, s string) int {
	t.Helper()
	n, err := strconv.Atoi(s)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// testdata/events-agent-platform-678.jsonl is the agent-platform lane's
// merge trail of the night of 2026-09-24 as beekeeper log --verb merge
// shows it, the sessions' ids left out: the supervisor seeds 672, 676, 678,
// 679 and 671; 678 fails with exit 3 at 00:35:33 and its session retries at
// 00:36:19. The old queue sent the retry behind the seeds 679 and 671 until
// the supervisor rebuilt the queue by hand at 01:00:50.
func TestReplayAgentPlatformNight(t *testing.T) {
	ds := replay(t, testdata(t, "events-agent-platform-678.jsonl"))
	at := func(clock, key string) decision {
		for _, d := range ds {
			if d.at == clock && d.key == key {
				return d
			}
		}
		t.Fatalf("no arrival of %s at %s: %+v", key, clock, ds)
		return decision{}
	}
	ap := func(n ...int) string {
		var keys []string
		for _, i := range n {
			keys = append(keys, fmt.Sprintf("giantswarm/agent-platform#%d", i))
		}
		return strings.Join(keys, " ")
	}
	retry := at("00:36:19", ap(678))
	if retry.behind != "" || strings.Join(retry.waiting, " ") != ap(678, 679, 671) {
		t.Errorf("the retry of the failed 678 waits behind %q; waiting %v", retry.behind, retry.waiting)
	}
	late := at("23:03:57", ap(684))
	if late.behind != "" || strings.Join(late.waiting, " ") != ap(676, 678, 679, 671, 684) {
		t.Errorf("684, arrived in the free lane, waits behind %q, the absent seeds' order %v", late.behind, late.waiting)
	}
	for _, d := range ds {
		if d.behind != "" {
			t.Errorf("%s: %s waits behind %s, though the lane was free", d.at, d.key, d.behind)
		}
	}
}

func TestAheadSeedOrder(t *testing.T) {
	const repo, first = "o/r", "o/r#1"
	now := time.Now()
	present := func(m state.Merge) bool { return Present(m, now, 15*time.Minute, func(p int) bool { return p == 9 }) }
	// at is a waiting merge that joined ago; pid 9 is in the gate, pid 7 left it a minute ago.
	at := func(pr int, by string, ago time.Duration, pid int, seeded bool) state.Merge {
		m := state.Merge{Repo: repo, PR: pr, Lane: "l", By: state.Party{Name: by}, PID: pid, Phase: state.Waiting, Seeded: seeded,
			Joined: now.Add(-ago), Seen: now.Add(-ago)}
		if pid == 7 {
			m.Seen = now.Add(-time.Minute)
		}
		return m
	}
	outside := at(1, "x", 0, 0, true)
	outside.Outside = true
	for _, c := range []struct {
		name   string
		merges []state.Merge
		pr     int
		behind string
	}{
		{"an arrived seed keeps behind an absent seed of another session", []state.Merge{at(1, "a", 3*time.Hour, 0, true), at(2, "b", 2*time.Hour, 9, true)}, 2, first},
		{"a seed never blocks an earlier pull request of its own session", []state.Merge{at(3, "s", 3*time.Hour, 0, true), at(2, "s", 2*time.Hour, 9, true)}, 2, ""},
		{"an arrived unseeded merge passes absent seeds", []state.Merge{at(1, "a", 3*time.Hour, 0, true), at(5, "b", 0, 9, false)}, 5, ""},
		{"a rerun within the queue TTL holds its place", []state.Merge{at(1, "a", time.Hour, 7, false), at(5, "b", 0, 9, false)}, 5, first},
		{"a merge run outside the gate heads its lane", []state.Merge{outside, at(5, "b", time.Hour, 9, false)}, 5, first},
	} {
		ahead, ok := Queue(&state.State{Merges: c.merges}, "l").Ahead(repo, c.pr, present)
		if got := map[bool]string{true: ahead.Key(), false: ""}[ok]; got != c.behind {
			t.Errorf("%s: behind %q, want %q", c.name, got, c.behind)
		}
	}
}

func TestFailedKeepsItsPlace(t *testing.T) {
	now := time.Now()
	m := state.Merge{Repo: "o/r", PR: 678, Lane: "l", PID: 3, Phase: state.Running, Joined: now.Add(-2 * time.Hour), Roll: []string{"x"}}
	if !Failed(&m, 3, now) || !m.Retrying() || m.Exit != 3 || !m.Seen.Equal(now.UTC()) || m.Roll != nil {
		t.Fatalf("a failed attempt does not keep its place: %+v", m)
	}
	st := &state.State{Merges: []state.Merge{m}}
	Prune(st, now.Add(11*time.Hour), 15*time.Minute, 12*time.Hour, func(int) bool { return false })
	if len(st.Merges) != 1 {
		t.Error("a failed attempt's place goes before the seed TTL")
	}
	if Present(st.Merges[0], now.Add(16*time.Minute), 15*time.Minute, func(int) bool { return false }) {
		t.Error("a failed attempt not retried holds up a free lane past the queue TTL")
	}
	r := state.Merge{Phase: state.Running}
	if Failed(&r, ExitRefused, now) || r.Retrying() {
		t.Error("devctl's refusal keeps a place")
	}
}
