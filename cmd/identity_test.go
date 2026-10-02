package cmd

import (
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/lease"
	"github.com/giantswarm/beekeeper/internal/state"
)

var testIdentity = config.Identity{Person: "pat@example.com", Team: "bumblebee", Host: "lab"}

const testOwner = "pat@example.com, team bumblebee, on lab"

// A party the machine's commands record carries the configured person, team
// and host, and note list names them.
func TestCallerCarriesTheIdentity(t *testing.T) {
	a, out := noteApp(t)
	a.cfg.Identity = testIdentity
	me, err := a.caller()
	if err != nil {
		t.Fatal(err)
	}
	if me.Name != agentOne || me.Owner() != testOwner {
		t.Fatalf("caller = %+v", me)
	}
	if err := addNote(a, "memo: the lab runs"); err != nil {
		t.Fatal(err)
	}
	st, err := a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	out.Reset()
	a.printNotes(st.Notes)
	if want := `by "` + agentOne + `" (` + testOwner + `)`; !strings.Contains(out.String(), want) {
		t.Errorf("note list %q lacks %q", out.String(), want)
	}
}

func TestLeaseListNamesTheHolderOwner(t *testing.T) {
	a, out := noteApp(t)
	h := lease.Holder{Env: "graveler", Name: agentOne, Person: testIdentity.Person, Team: testIdentity.Team, Host: testIdentity.Host, Since: "2026-10-02T10:00:00Z", Purpose: "lab proof"}
	a.printLeases(&leaseList{Held: []leaseView{{Holder: h, State: holderLive}}, Queues: map[string][]state.Grant{}})
	if want := agentOne + " (" + testOwner + ")"; !strings.Contains(out.String(), want) {
		t.Errorf("lease list %q lacks %q", out.String(), want)
	}
	if got := h.Party().Owner(); got != testOwner {
		t.Errorf("holder party owner %q", got)
	}
}
