package state

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// A state decoded reusing the last snapshot's elements is the state a whole
// decode gives, for documents the writer writes and for the ones it does not:
// unknown members at the top and in an element, retired keys, null and empty
// arrays, a member in another case or twice, and broken documents, which fail
// as a whole decode fails.
//
// control: none: decodeReusing is new, so the merge base does not build this
// test; the scenario busy-desk-guide-watch-reads-what-changed fails there
func TestDecodeReusingEqualsWholeDecode(t *testing.T) {
	s, err := OpenVersion(t.TempDir(), "dev")
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC)
	big := &State{NextNote: 3, Budget: &Budget{}}
	for i := range 50 {
		p := Party{Session: fmt.Sprintf("s-%d", i), Name: fmt.Sprintf("worker %d", i), Person: "p"}
		big.Agents = append(big.Agents, Agent{Party: p, Registered: at, LastTask: "task"})
		big.Starts = append(big.Starts, Start{Party: p, Mode: ModeBypass, Dir: "/w", By: p, At: at, Task: fmt.Sprintf("t\\\"%d\"", i), Successor: &Party{Name: "next"}})
		big.Records = append(big.Records, Record{Session: p, Issue: fmt.Sprintf("o/r#%d", i), At: at, Ended: at})
		big.GuideReports = append(big.GuideReports, fmt.Sprintf("report %d", i))
	}
	encoded, err := encodeState(big)
	if err != nil {
		t.Fatal(err)
	}
	compact, err := json.Marshal(big)
	if err != nil {
		t.Fatal(err)
	}
	docs := map[string]string{
		"pretty":   string(encoded),
		"compact":  string(compact),
		"unknown":  `{"future":{"a":[1,2]},"notes":[{"id":1,"text":"x","later":true},null,{"id":2}],"nextNote":3,"supervisor":{"x":1}}`,
		"empty":    `{"notes":[],"starts":null,"agents":[ ],"guideReports":["a", "b" ,"c"]}`,
		"spaces":   " \n{ \"notes\" : [ { \"id\" : 1 } , { \"id\" : 2 } ] , \"nextNote\" : 1 } \n",
		"case":     `{"Notes":[{"id":1}],"notes":[{"id":2}]}`,
		"twice":    `{"notes":[{"id":1}],"notes":[{"id":2}]}`,
		"string":   `{"notes":"not an array"}`,
		"element":  `{"notes":[{"id":"one"}]}`,
		"trailing": `{"notes":[{"id":1}]} x`,
		"cut":      `{"notes":[{"id":1}`,
		"null":     `null`,
		"array":    `[]`,
		"nothing":  ``,
	}
	for name, doc := range docs {
		t.Run(name, func(t *testing.T) {
			want, wantErr := s.decode([]byte(doc))
			for _, prev := range []*snapshotElements{nil, mustElements(t, s, string(encoded))} {
				got, _, err := s.decodeReusing([]byte(doc), prev)
				if (err == nil) != (wantErr == nil) || (err != nil && err.Error() != wantErr.Error()) {
					t.Fatalf("error %v, a whole decode's %v", err, wantErr)
				}
				if err == nil && !reflect.DeepEqual(got, want) {
					t.Fatalf("decoded %+v, a whole decode %+v", got, want)
				}
			}
		})
	}
}

func mustElements(t *testing.T, s *FileStore, doc string) *snapshotElements {
	t.Helper()
	_, elems, err := s.decodeReusing([]byte(doc), nil)
	if err != nil || elems == nil {
		t.Fatalf("no elements from %d bytes: %v", len(doc), err)
	}
	return elems
}

// A reader of a replaced state decodes only the elements whose bytes changed
// and shares the others with its last snapshot; what it is handed is still
// its own copy, and equals what a whole decode of the replacement gives.
//
// control: none: decodeReusing is new, so the merge base does not build this
// test; the scenario busy-desk-guide-watch-reads-what-changed fails there
func TestReadReusesUnchangedElements(t *testing.T) {
	dir := t.TempDir()
	writer, err := OpenVersion(dir, "dev")
	if err != nil {
		t.Fatal(err)
	}
	reader, err := OpenVersion(dir, "dev")
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC)
	if err := writer.Update(func(st *State) ([]Event, error) {
		for i := range 3 {
			st.Starts = append(st.Starts, Start{Party: Party{Name: fmt.Sprintf("w%d", i)}, Mode: ModeBypass, At: at, Successor: &Party{Name: fmt.Sprintf("n%d", i)}})
		}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	first, err := reader.Read()
	if err != nil {
		t.Fatal(err)
	}
	before := reader.snapshot
	first.Starts[0].Successor.Name = "changed by a caller"
	if err := writer.Update(func(st *State) ([]Event, error) {
		st.Starts[1].Task = "a new task"
		return []Event{{At: at, Verb: "test.rewrite"}}, nil
	}); err != nil {
		t.Fatal(err)
	}
	second, err := reader.Read()
	if err != nil {
		t.Fatal(err)
	}
	after := reader.snapshot
	if after == before {
		t.Fatal("the replacement was not read")
	}
	if after.Starts[0].Successor != before.Starts[0].Successor || after.Starts[2].Successor != before.Starts[2].Successor {
		t.Error("unchanged elements were decoded again")
	}
	if after.Starts[1].Successor == before.Starts[1].Successor {
		t.Error("the changed element was not decoded again")
	}
	if second.Starts[0].Successor == after.Starts[0].Successor || second.Starts[0].Successor.Name != "n0" {
		t.Errorf("the snapshot was lent or changed by a caller: %+v", second.Starts[0].Successor)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	whole, err := reader.decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(second, whole) {
		t.Fatalf("read %+v, a whole decode %+v", second, whole)
	}
	if !strings.Contains(string(raw), "a new task") || second.Starts[1].Task != "a new task" {
		t.Fatalf("the rewrite was not read: %+v", second.Starts[1])
	}
}

// A clone shares nothing a caller can change with the snapshot: every map,
// slice and pointer at any depth is its own, while values that hold none of
// them are copied whole.
//
// control: none: holdsReferences is new, so the merge base does not build
// this test; the clone's result is the base's, only its cost changed
func TestCloneSnapshotSharesNothingMutable(t *testing.T) {
	at := time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC)
	src := &State{
		Starts:          []Start{{Party: Party{Name: "w"}, At: at, Successor: &Party{Name: "n"}}},
		GuideReports:    []string{"a"},
		ProductionNotes: map[int]time.Time{1: at},
		EventOutbox:     &EventOutbox{Data: []byte("x")},
		Notes:           []Note{{ID: 1, Text: "t"}},
	}
	c := cloneSnapshot(reflect.ValueOf(src)).Interface().(*State)
	if !reflect.DeepEqual(c, src) {
		t.Fatalf("clone %+v differs from %+v", c, src)
	}
	c.Starts[0].Successor.Name = "x"
	c.Starts[0].Name = "x"
	c.GuideReports[0] = "x"
	c.ProductionNotes[1] = at.Add(time.Hour)
	c.EventOutbox.Data[0] = 'y'
	c.Notes[0].Text = "x"
	if src.Starts[0].Successor.Name != "n" || src.Starts[0].Name != "w" || src.GuideReports[0] != "a" ||
		!src.ProductionNotes[1].Equal(at) || string(src.EventOutbox.Data) != "x" || src.Notes[0].Text != "t" {
		t.Fatalf("a change of the clone reached the snapshot: %+v", src)
	}
	if holdsReferences(reflect.TypeFor[time.Time]()) || holdsReferences(reflect.TypeFor[Party]()) || !holdsReferences(reflect.TypeFor[Start]()) {
		t.Fatal("holdsReferences misjudges a type")
	}
}
