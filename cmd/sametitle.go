package cmd

import (
	"cmp"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/state"
)

// A title is the name a session's CLI takes messages under. Two live
// sessions carrying one title make a message by that name reach neither for
// sure, so a role resolves from beekeeper's holder record and the holder's
// own socket, the doctor and the watch say each shared title, and a relay or
// a start never hands out a title another live session holds.

// sameTitle is one title more than one live session carries.
type sameTitle struct {
	title    string
	sessions []*claude.Session
}

// sameTitles are the titles more than one live session carries (ignoring
// case), sorted by title, each session by PID.
func sameTitles(sessions []*claude.Session) []sameTitle {
	by := map[string][]*claude.Session{}
	for _, s := range sessions {
		if k := titleKey(s.Name); k != "" {
			by[k] = append(by[k], s)
		}
	}
	var out []sameTitle
	for _, k := range slices.Sorted(maps.Keys(by)) {
		if ss := by[k]; len(ss) > 1 {
			slices.SortFunc(ss, func(a, b *claude.Session) int { return cmp.Compare(a.PID, b.PID) })
			out = append(out, sameTitle{title: ss[0].Name, sessions: ss})
		}
	}
	return out
}

// titleKey is the form two titles compare in: case and runs of spaces
// ignored.
func titleKey(title string) string {
	return strings.ToLower(strings.Join(strings.Fields(title), " "))
}

// String says the shared title and the sessions carrying it, the doctor's
// line and the watch's SAME TITLE line.
func (d sameTitle) String() string {
	var each []string
	for _, s := range d.sessions {
		each = append(each, fmt.Sprintf("PID %d session %s", s.PID, s.ID))
	}
	return fmt.Sprintf("%d live sessions are titled %q (%s): a message by that name reaches neither for sure (a role still reaches its holder); retitle or stop the one that should not carry it",
		len(d.sessions), d.title, strings.Join(each, ", "))
}

// sameTitleKey starts the condition key of a title more than one live
// session carries.
const sameTitleKey = "same-title "

// sameTitles says one SAME TITLE line per title more than one live session
// carries, and its ENDED line once one session is left with it.
func (w *watcher) sameTitles(sessions []*claude.Session) {
	found := map[string]bool{}
	for _, d := range sameTitles(sessions) {
		key := sameTitleKey + titleKey(d.title)
		found[key] = true
		w.emit(key, "SAME TITLE: %s", d)
	}
	w.clearMissing(sameTitleKey, found)
}

// titleTaken refuses to give title to me while another live session
// carries it, naming that session; nil when none does.
func titleTaken(sessions []*claude.Session, title string, me state.Party) error {
	k := titleKey(title)
	if k == "" {
		return nil
	}
	for _, s := range sessions {
		if titleKey(s.Name) == k && !me.Is(s.Party()) {
			return refused("the title %q is taken: the live session %s (PID %d) carries it, and a second one would make a message by that name reach neither for sure; retitle or stop that session first",
				title, s.ID, s.PID)
		}
	}
	return nil
}
