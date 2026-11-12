package state

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gofrs/flock"

	"github.com/giantswarm/beekeeper/internal/execution"
)

// A lease renewal waits commitWait at most for the commit lock. A transaction
// reads, decodes and copies the whole state and runs its callback without that
// lock; only its commit waits for it.
func TestUpdateReadsStateWithoutCommitLock(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Update(func(st *State) ([]Event, error) { st.NextNote = 1; return nil, nil }); err != nil {
		t.Fatal(err)
	}
	renewal := flock.New(s.path("state.commit.lock"))
	if err := renewal.Lock(); err != nil {
		t.Fatal(err)
	}
	ran := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- s.Update(func(st *State) ([]Event, error) {
			close(ran)
			st.NextNote = 2
			return []Event{{Verb: "test.commit-lock"}}, nil
		})
	}()
	select {
	case <-ran:
	case err := <-done:
		_ = renewal.Unlock()
		t.Fatalf("the transaction ended before its callback: %v", err)
	case <-time.After(commitWait / 2):
		_ = renewal.Unlock()
		t.Fatal("the transaction waited for the commit lock before its callback")
	}
	select {
	case err := <-done:
		_ = renewal.Unlock()
		t.Fatalf("the transaction committed while a renewal held the commit lock: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	if err := renewal.Unlock(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	st, err := s.Read()
	if err != nil || st.NextNote != 2 || st.EventOutbox != nil {
		t.Fatalf("committed state %+v: %v", st, err)
	}
	evs, err := s.Events(0, nil)
	if err != nil || len(evs) != 1 || evs[0].Verb != "test.commit-lock" {
		t.Fatalf("events %+v: %v", evs, err)
	}
}

// The commit no longer reads the whole state again. A renewal that replaced it
// during the transaction is still judged: a newer release's stamp refuses the
// older writer's commit, and the renewal survives.
func TestUpdateRefusesNewerWriterOfARenewal(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenVersion(dir, recoveryRelease)
	if err != nil {
		t.Fatal(err)
	}
	newer, err := OpenVersion(dir, recoveryNewer)
	if err != nil {
		t.Fatal(err)
	}
	ref := execution.Ref{Host: "renewal-host", Execution: "renewal-execution", Generation: 1}
	if err := s.Update(func(st *State) ([]Event, error) {
		st.Starts = []Start{{Execution: &ExecutionRecord{Request: execution.StartRequest{Ref: ref}}}}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	err = s.Update(func(st *State) ([]Event, error) {
		if _, err := newer.RenewExecutionLease(context.Background(), ref, func(*State) (*execution.LeaseGrant, error) {
			return &execution.LeaseGrant{Ref: ref, Sequence: 1}, nil
		}); err != nil {
			return nil, err
		}
		st.NextNote = 9
		return nil, nil
	})
	var stale *StaleWriterError
	if !errors.As(err, &stale) || stale.Newer != recoveryNewer {
		t.Fatalf("an older writer committed over a newer release's renewal: %v", err)
	}
	st, err := newer.Read()
	if err != nil || st.NextNote == 9 || len(st.Starts) != 1 || st.Starts[0].Execution.Lease == nil || st.Starts[0].Execution.Lease.Sequence != 1 {
		t.Fatalf("state after the refused commit %+v: %v", st, err)
	}
}

// The staged document is the final encoding with the outbox added, not a
// second encoding of the whole state: it must decode as the state encoded
// with its outbox does.
func TestStagedOutboxDecodesAsStateWithOutbox(t *testing.T) {
	var future State
	if err := json.Unmarshal([]byte(`{"nextNote":3,"notes":[{"id":1,"text":"kept","future":{"keep":true}}],"futureState":[1,{"a":"}"}]}`), &future); err != nil {
		t.Fatal(err)
	}
	for name, st := range map[string]*State{"empty": {}, "future": &future} {
		t.Run(name, func(t *testing.T) {
			outbox := &EventOutbox{Offset: 42, Data: []byte("{\"verb\":\"answer.recorded\"}\n")}
			staged, final, err := encodeCommit(st, outbox)
			if err != nil {
				t.Fatal(err)
			}
			if st.EventOutbox != nil {
				t.Fatal("encoding left the outbox in the state")
			}
			plain, err := encodeState(st)
			if err != nil || !bytes.Equal(final, plain) {
				t.Fatalf("final %s, want %s: %v", final, plain, err)
			}
			st.EventOutbox = outbox
			encoded, err := encodeState(st)
			st.EventOutbox = nil
			if err != nil {
				t.Fatal(err)
			}
			var got, want State
			if err := json.Unmarshal(staged, &got); err != nil {
				t.Fatalf("staged %s: %v", staged, err)
			}
			if err := json.Unmarshal(encoded, &want); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("staged %s\nwant %s", staged, encoded)
			}
		})
	}
}

// unknownMembers replaces decoding every object again into a map of raw
// messages. It must keep exactly what that map kept.
func TestUnknownMembersMatchesRawMessageMap(t *testing.T) {
	known := map[string]bool{"id": true, "text": true, "abc": true}
	for _, raw := range []string{
		`null`,
		` null `,
		`{}`,
		" {\n} ",
		`{"id":1,"text":"a"}`,
		`{"ID":1,"Text":"a","future":{"keep":true}}`,
		`{"\u0061bc":1,"x\"y":"q\\\"}","n":[1,{"}":"]"},[]],"t":true,"f":false,"z":null,"num":-1.5e+3}`,
		"{\n\t\"later\" :\r\n [ \"a\" , { \"b\" : \"\\\\\" } ] ,\"id\":2 }",
		`{"dup":1,"dup":{"a":2},"id":0}`,
		`{"é":"ü","\ud83d\ude00":"x","s":""}`,
		"{\"bad\xff\":1}",
	} {
		var want map[string]json.RawMessage
		if err := json.Unmarshal([]byte(raw), &want); err != nil {
			t.Fatalf("%s: %v", raw, err)
		}
		for k := range want {
			if known[strings.ToLower(k)] {
				delete(want, k)
			}
		}
		if len(want) == 0 {
			want = nil
		}
		got, err := unknownMembers([]byte(raw), known)
		if err != nil {
			t.Fatalf("%s: %v", raw, err)
		}
		if !reflect.DeepEqual(map[string]json.RawMessage(got), want) {
			t.Fatalf("%s: kept %s, want %s", raw, got, want)
		}
	}
}

// The trace names the holder's command, never its arguments, times the wait
// as well as the hold, covers the commit lock, and records a wait that ended
// without the lock.
func TestLockTraceNamesHolderWaitsAndCommits(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "holds.jsonl")
	t.Setenv(lockTraceEnv, path)
	previous := processCommand.Load()
	t.Cleanup(func() { processCommand.Store(previous) })
	const name = "beekeeper supervisor loop --once"
	SetCommand(name)
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Update(func(st *State) ([]Event, error) {
		st.NextNote = 1
		return []Event{{Verb: "test.trace"}}, nil
	}); err != nil {
		t.Fatal(err)
	}
	held := flock.New(s.path("state.lock"))
	if err := held.Lock(); err != nil {
		t.Fatal(err)
	}
	const wait = 50 * time.Millisecond
	_, err = s.lock(false, wait)
	_ = held.Unlock()
	var busy *LockBusyError
	if !errors.As(err, &busy) {
		t.Fatalf("a held lock was acquired: %v", err)
	}
	raw, err := os.ReadFile(path) //nolint:gosec // trace path under t.TempDir
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	for _, line := range bytes.Split(bytes.TrimSpace(raw), []byte("\n")) {
		var h lockTiming
		if err := json.Unmarshal(line, &h); err != nil {
			t.Fatal(err)
		}
		if h.PID != os.Getpid() || h.Command != name || h.WaitNS < 0 {
			t.Fatalf("row %s", line)
		}
		switch {
		case h.Busy:
			counts["busy"]++
			if h.HoldNS != 0 || !h.Acquired.IsZero() || h.WaitNS < wait.Nanoseconds() || bytes.Contains(line, []byte(`"acquired"`)) {
				t.Fatalf("busy row %s", line)
			}
		case h.Released.Before(h.Acquired) || h.HoldNS < 0:
			t.Fatalf("hold row %s", line)
		case h.Lock == commitLockName:
			counts[commitLockName]++
		case h.Lock == "" && !h.Shared:
			counts["state"]++
		}
	}
	if want := map[string]int{"busy": 1, commitLockName: 1, "state": 1}; !reflect.DeepEqual(counts, want) {
		t.Fatalf("trace rows %v, want %v:\n%s", counts, want, raw)
	}
}
