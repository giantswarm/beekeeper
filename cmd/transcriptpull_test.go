package cmd

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/internal/execution"
	"github.com/giantswarm/beekeeper/internal/guard"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/transcripts"
)

// pagedLogTransport serves log as a host does, in pages of size bytes
// regardless of the requested limit, so records straddle page boundaries.
func pagedLogTransport(log *string, size int, fail *error, calls *int) *testExecutionTransport {
	return &testExecutionTransport{logs: func(_ context.Context, _ execution.Ref, q execution.LogRequest) (execution.LogPage, error) {
		*calls++
		if *fail != nil {
			return execution.LogPage{}, *fail
		}
		off, err := strconv.Atoi(q.After)
		if err != nil || off > len(*log) {
			return execution.LogPage{}, errors.New("invalid log cursor")
		}
		end := min(off+size, len(*log))
		return execution.LogPage{Data: []byte((*log)[off:end]), Next: strconv.Itoa(end), Truncated: end < len(*log)}, nil
	}}
}

func TestExecutionTranscriptRedactsAcrossPagesAndCollectsAfterSettle(t *testing.T) {
	a, r := executionTestApp(t)
	ctx := context.Background()
	const secret = "fixture-indexed-secret-value-0123456789"
	ix, err := guard.OpenIndex(a.scanDir())
	if err != nil {
		t.Fatal(err)
	}
	ix.Add("op://vault/item/FIXTURE_TOKEN", secret)
	if err := ix.Save(a.now); err != nil {
		t.Fatal(err)
	}
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Starts[0].Execution.Request.Harness = execution.HarnessCodex
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	first := `{"type":"item.completed","item":{"id":"i1","type":"agent_message","text":"Token is ` + secret + ` for the proof"}}` + "\n"
	second := `{"type":"item.completed","item":{"id":"i2","type":"agent_message","text":"Proof finished"}}` + "\n"
	final := `{"type":"item.completed","item":{"id":"i3","type":"agent_message","text":"Final words of the worker"}}`
	split := strings.Index(first, secret) + len(secret)/2
	// The process is still writing: the host holds the first record up to
	// the middle of the secret.
	log := first[:split]
	var fail error
	calls := 0
	a.executionTransport = func(execution.Host) execution.Transport { return pagedLogTransport(&log, 32, &fail, &calls) }
	rawFile := func() string {
		t.Helper()
		st, err := a.store.Read()
		if err != nil {
			t.Fatal(err)
		}
		meta, _ := a.executionTranscriptMeta(st, st.Starts[0], st.Starts[0].Execution)
		b, err := os.ReadFile(filepath.Join(a.transcriptStore().Root, meta.Key+".raw"))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		return string(b)
	}
	if err := a.syncRemoteExecutionTranscriptByRef(ctx, r.Ref); err != nil {
		t.Fatal(err)
	}
	if raw := rawFile(); raw != "" {
		t.Fatalf("incomplete record persisted: %q", raw)
	}
	// The record completes; pages of 32 bytes split the secret again.
	log = first + second
	if err := a.syncRemoteExecutionTranscriptByRef(ctx, r.Ref); err != nil {
		t.Fatal(err)
	}
	raw := rawFile()
	if strings.Contains(raw, secret[:len(secret)/2]) || strings.Contains(raw, secret[len(secret)/2:]) || !strings.Contains(raw, "[redacted: ") {
		t.Fatalf("raw stream not redacted across page boundaries: %q", raw)
	}

	// The worker writes a last record of more than four pages and exits;
	// the execution settles while the log cannot be read.
	log = first + second + strings.Repeat(`{"type":"noise"}`+"\n", 20) + final
	fail = errors.New("fixture log read failed")
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		x := st.Starts[0].Execution
		x.Observation.ProcessState = execution.Exited
		x.Settled = true
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	// A failed pull costs the host's reconciliation nothing; it is
	// recorded and reported once, and the transcript stays pending.
	for range 2 {
		if err := a.syncExecutionHost(ctx, sshTestSelector); err != nil {
			t.Fatal(err)
		}
	}
	st, err := a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if x := st.Starts[0].Execution; !executionTranscriptPending(x) || !strings.Contains(x.TranscriptError, "fixture log read failed") {
		t.Fatalf("failed transcript pull of a settled execution not kept pending and recorded: %q", x.TranscriptError)
	}
	events, err := a.store.Events(0, func(e state.Event) bool { return e.Verb == "execution.transcript-unsaved" })
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || !strings.Contains(events[0].Detail, "fixture log read failed") {
		t.Fatalf("failed transcript pull reported %d times, want once: %+v", len(events), events)
	}
	// Each round persists at most four pages once it holds a complete
	// record: the rest stays pending for the next rounds, up to EOF.
	fail = nil
	rounds := 0
	for executionTranscriptPending(st.Starts[0].Execution) && rounds < 10 {
		rounds++
		if err := a.syncExecutionHost(ctx, sshTestSelector); err != nil {
			t.Fatal(err)
		}
		if st, err = a.store.Read(); err != nil {
			t.Fatal(err)
		}
	}
	if rounds < 2 || executionTranscriptPending(st.Starts[0].Execution) || st.Starts[0].Execution.TranscriptError != "" {
		t.Fatalf("transcript of the ended execution not collected to EOF over rounds: rounds=%d", rounds)
	}
	meta, _ := a.executionTranscriptMeta(st, st.Starts[0], st.Starts[0].Execution)
	turns, err := a.transcriptStore().ReadTurns(meta.Key, transcripts.ReadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) == 0 || turns[len(turns)-1].Text != "Final words of the worker" {
		t.Fatalf("final record of the ended execution missing: %+v", turns)
	}
	calls = 0
	if err := a.syncExecutionHost(ctx, sshTestSelector); err != nil || calls != 0 {
		t.Fatalf("collected transcript pulled again: calls=%d err=%v", calls, err)
	}
}
