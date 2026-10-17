package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"slices"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/pkg/apis/beekeeper/v1alpha1"
)

// kubePinApp is noteApp over a Kubernetes store, with a seed that writes
// pinnedNotes' notes, each filed by ourTeam, as the Note objects an older
// beekeeper left: the pinned ones carry spec.pinned.
func kubePinApp(t *testing.T) (*app, *bytes.Buffer, func()) {
	t.Helper()
	_, store, c := envtestKube(t)
	ctx := context.Background()
	ns := "beekeeper-" + ourTeam
	if err := c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}); err != nil {
		t.Fatal(err)
	}
	var older state.State
	if err := json.Unmarshal([]byte(pinnedNotes), &older); err != nil {
		t.Fatal(err)
	}
	// seeded one note per update
	seed := func() {
		for _, n := range slices.Concat(older.Notes, older.Memos) {
			n.By.Team = ourTeam
			if err := store.Update(func(st *state.State) ([]state.Event, error) {
				drop := func(m state.Note) bool { return m.ID == n.ID }
				st.Notes, st.Memos = slices.DeleteFunc(st.Notes, drop), slices.DeleteFunc(st.Memos, drop)
				if n.Memo() {
					st.Memos = append(st.Memos, n)
				} else {
					st.Notes = append(st.Notes, n)
				}
				return nil, nil
			}); err != nil {
				t.Fatal(err)
			}
		}
		for id, name := range []string{"note-1", "note-2", "note-3"} {
			n := &v1alpha1.Note{}
			if err := c.Get(ctx, ctrlclient.ObjectKey{Namespace: ns, Name: name}, n); err != nil || !n.Spec.Pinned {
				t.Fatalf("note #%d is no pinned Note object: %v", id+1, err)
			}
		}
	}
	a, out := noteApp(t)
	a.store = store
	return a, out, seed
}

// A Kubernetes-backed desk carries an older beekeeper's pins through both
// hand-overs and `note migrate` as the file store does.
func TestEnvtestPinnedNotesReachBothHandoversBeforeAndAfterTheMigration(t *testing.T) {
	a, out, seed := kubePinApp(t)
	pinsSurviveTheMigration(t, a, out, seed)
}

func TestEnvtestNoteMigrateClosesNoPinItCannotMove(t *testing.T) {
	a, out, seed := kubePinApp(t)
	migrateClosesNoUnmovedPin(t, a, out, seed)
}
