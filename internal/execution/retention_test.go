package execution

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestResourceAuditAndRetention(t *testing.T) {
	f := testFixture(t)
	ctx := context.Background()
	h, err := f.s.Start(ctx, f.r)
	if err != nil {
		t.Fatal(err)
	}
	id := string(f.r.Ref.Execution)
	log := filepath.Join(f.s.Root, id+".log")
	if _, err := f.s.git(ctx, h.Workspace, "update-ref", "refs/heads/beekeeper-worker/orphan", h.Commit); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(log, []byte("review evidence"), 0600); err != nil {
		t.Fatal(err)
	}
	orphan := filepath.Join(f.s.Config.WorkspaceRoot, "orphan")
	if err := os.MkdirAll(orphan, 0700); err != nil {
		t.Fatal(err)
	}
	unrelated := filepath.Join(f.s.Root, "unrelated")
	if err := os.WriteFile(unrelated, []byte("private"), 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(f.s.Root, "symlink")
	if err := os.Symlink(unrelated, link); err != nil {
		t.Fatal(err)
	}
	audit := func() map[string]ResourceFinding {
		t.Helper()
		rows, err := f.s.AuditResources(ctx)
		if err != nil {
			t.Fatal(err)
		}
		got := map[string]ResourceFinding{}
		for _, r := range rows {
			got[r.Path] = r
		}
		return got
	}
	before, err := os.ReadFile(filepath.Clean(filepath.Join(f.s.Root, id+".json")))
	if err != nil {
		t.Fatal(err)
	}
	rows := audit()
	orphanRef := false
	for _, r := range rows {
		if strings.HasSuffix(r.Path, ":refs/heads/beekeeper-worker/orphan") {
			orphanRef = true
			if r.State != Unknown {
				t.Fatal("orphan ref claimed")
			}
		}
	}
	if !orphanRef {
		t.Fatal("orphan ref absent")
	}
	f.launcher.states[h.Unit] = unitActive
	if err := f.s.update(ctx, f.r.Ref, func(j *Journal) error { j.InTurn = true; return nil }); err != nil {
		t.Fatal(err)
	}
	if audit()[log].State != unitActive {
		t.Fatal("active execution misclassified")
	}
	if err := f.s.update(ctx, f.r.Ref, func(j *Journal) error { j.InTurn = false; return nil }); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{orphan, unrelated, link} {
		if rows[p].State != Unknown {
			t.Fatalf("unowned entry classified as safe: %+v", rows[p])
		}
	}
	after, err := os.ReadFile(filepath.Clean(filepath.Join(f.s.Root, id+".json")))
	if err != nil || string(before) != string(after) {
		t.Fatal("audit mutated journal")
	}
	for _, state := range []string{unitActive, Unreachable} {
		f.launcher.states[h.Unit] = state
		rows = audit()
		want := "parked"
		if state == Unreachable {
			want = Unknown
		}
		if rows[log].State != want {
			t.Fatalf("got %+v, want %s", rows[log], want)
		}
		if err := f.s.ReconcileResource(ctx, f.r.Ref, true); err == nil {
			t.Fatal("reconciled active/unknown resource")
		}
	}
	f.launcher.states[h.Unit] = unitInactive
	runtimeHome := f.s.agyMCPHomeDir(f.r.Ref)
	if err := os.MkdirAll(runtimeHome, 0700); err != nil {
		t.Fatal(err)
	}
	if err := f.s.ReconcileResource(ctx, f.r.Ref, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(runtimeHome); !os.IsNotExist(err) {
		t.Fatal("lifecycle runtime cleanup not reused", err)
	}
	rows = audit()
	if rows[log].Reason != "managed log retention" {
		t.Fatalf("not retained: %+v", rows[log])
	}
	if err := f.s.ReconcileResource(ctx, f.r.Ref, true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(log); err != nil {
		t.Fatal("released log erased before deadline")
	}
	now := f.s.now()
	f.s.Now = func() time.Time { return now.Add(LogRetention + time.Hour) }
	if err := f.s.ReconcileResource(ctx, f.r.Ref, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(log); err != nil {
		t.Fatal("evidence removed without release", err)
	}
	if !strings.Contains(audit()[log].Reason, "past retention") {
		t.Fatal("overdue log not reported")
	}
	for range 2 {
		if err := f.s.ReconcileResource(ctx, f.r.Ref, true); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Lstat(log); !os.IsNotExist(err) {
		t.Fatal("released expired log remains", err)
	}
	for _, p := range []string{h.Workspace, orphan, unrelated, link, filepath.Join(f.s.Root, id+".json")} {
		if _, err := os.Lstat(p); err != nil {
			t.Fatal("retained/unowned resource lost", p, err)
		}
	}
}

func TestRetentionRejectsSymlinkLog(t *testing.T) {
	f := testFixture(t)
	ctx := context.Background()
	h, err := f.s.Start(ctx, f.r)
	if err != nil {
		t.Fatal(err)
	}
	f.launcher.states[h.Unit] = unitInactive
	if err := f.s.ReconcileResource(ctx, f.r.Ref, false); err != nil {
		t.Fatal(err)
	}
	now := f.s.now()
	f.s.Now = func() time.Time { return now.Add(LogRetention) }
	target := filepath.Join(t.TempDir(), "evidence")
	if err := os.WriteFile(target, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(f.s.Root, string(f.r.Ref.Execution)+".log")); err != nil {
		t.Fatal(err)
	}
	if err := f.s.ReconcileResource(ctx, f.r.Ref, true); err == nil {
		t.Fatal("accepted symlink log")
	}
	if b, err := os.ReadFile(filepath.Clean(target)); err != nil || string(b) != "keep" {
		t.Fatal("symlink target changed")
	}
}

func TestResourceWireAndRevocationRetry(t *testing.T) {
	f := testFixture(t)
	ctx := context.Background()
	if _, err := f.s.Start(ctx, f.r); err != nil {
		t.Fatal(err)
	}
	// Change the journal consistently to model a credential-enabled execution.
	if err := f.s.update(ctx, f.r.Ref, func(j *Journal) error { j.Request.GitHub = true; j.Digest = j.Request.Digest(); return nil }); err != nil {
		t.Fatal(err)
	}
	f.launcher.states[UnitName(f.r.Ref)] = unitInactive
	f.s.Callback = func(context.Context, []string, []byte) ([]byte, error) { return nil, errors.New("lost coordinator") }
	transport := SSHTransport{Host: f.host, Run: func(ctx context.Context, _ []string, input []byte) ([]byte, error) {
		var out bytes.Buffer
		err := f.s.Serve(ctx, bytes.NewReader(input), &out)
		return out.Bytes(), err
	}}
	if err := transport.Call(ctx, "resource-reconcile", f.r.Ref, false, &struct{}{}); err == nil {
		t.Fatal("revocation failure not returned")
	}
	j, err := f.s.Read(f.r.Ref)
	if err != nil || !j.RevocationFailed || !j.RetainedAt.IsZero() {
		t.Fatalf("failure not retained: %+v %v", j.Handle, err)
	}
	var rows []ResourceFinding
	if err := transport.Call(ctx, "resource-audit", Ref{}, nil, &rows); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range rows {
		if r.Ref == f.r.Ref && r.Reason == "failed credential revocation" {
			found = true
		}
	}
	if !found {
		t.Fatal("failed revocation absent from audit")
	}
	// The same grant can be revoked on retry after connectivity returns.
	f.s.Callback = func(_ context.Context, _ []string, input []byte) ([]byte, error) {
		var req CallbackRequest
		if err := Decode(input, &req); err != nil {
			return nil, err
		}
		return mustJSON(WireResponse{Protocol: Protocol, RequestID: req.RequestID, Ref: req.Ref, Payload: mustJSON(struct{}{})}), nil
	}
	if err := transport.Call(ctx, "resource-reconcile", f.r.Ref, false, &struct{}{}); err != nil {
		t.Fatal(err)
	}
	j, err = f.s.Read(f.r.Ref)
	if err != nil || j.RevocationFailed || j.RetainedAt.IsZero() {
		t.Fatal("retry did not advance retention", err)
	}
}

func TestResourceAuditBoundAndMissingRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "missing")
	s := Server{Root: root}
	if _, err := s.AuditResources(context.Background()); err == nil {
		t.Fatal("missing root reported healthy")
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatal("audit created missing root")
	}
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4097; i++ {
		if err := os.WriteFile(filepath.Join(root, fmt.Sprint(i)), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.AuditResources(context.Background()); err == nil {
		t.Fatal("unbounded inventory accepted")
	}
}

// Model a concurrent start winning the execution lock after the first probe.
type resourceStartRace struct {
	*fakeLauncher
	probes int
}

func (l *resourceStartRace) State(context.Context, string) string {
	l.probes++
	if l.probes == 1 {
		return unitInactive
	}
	return unitActive
}
func TestRetentionRechecksUnderStartLock(t *testing.T) {
	f := testFixture(t)
	ctx := context.Background()
	if _, err := f.s.Start(ctx, f.r); err != nil {
		t.Fatal(err)
	}
	f.s.Launcher = &resourceStartRace{fakeLauncher: f.launcher}
	if err := f.s.ReconcileResource(ctx, f.r.Ref, true); err == nil {
		t.Fatal("stopped concurrently activated execution")
	}
	j, err := f.s.Read(f.r.Ref)
	if err != nil || j.StopRequest != nil || !j.RetainedAt.IsZero() {
		t.Fatal("concurrent start mutated", err)
	}
}

func TestResourceOrphanInventoryJoin(t *testing.T) {
	f := testFixture(t)
	rows := []ResourceFinding{
		{Ref: f.r.Ref, Coordinator: f.r.Coordinator, State: unitActive, Reason: "owned"},
		{Ref: f.r.Ref, Coordinator: "another-desk", State: "terminal"},
		{State: Unknown, Reason: "unclassified"},
	}
	ReportOrphans(rows, f.r.Coordinator, []Ref{f.r.Ref})
	if rows[0].Orphan {
		t.Fatal("registered execution called orphan")
	}
	ReportOrphans(rows, f.r.Coordinator, nil)
	if !rows[0].Orphan || rows[0].State != unitActive || rows[1].Orphan || rows[2].Orphan {
		t.Fatal("incorrect orphan classification", rows)
	}
}

func TestResourceAuditLeavesUnrelatedJSONAndJournalSymlinksUnknown(t *testing.T) {
	f := testFixture(t)
	ctx := context.Background()
	if _, err := f.s.Start(ctx, f.r); err != nil {
		t.Fatal(err)
	}
	// A valid-looking journal behind a symlink must never establish ownership.
	other := "11111111-1111-4111-8111-111111111111.json"
	link := filepath.Join(f.s.Root, other)
	if err := os.Symlink(filepath.Join(f.s.Root, string(f.r.Ref.Execution)+".json"), link); err != nil {
		t.Fatal(err)
	}
	unrelated := filepath.Join(f.s.Root, "settings.json")
	if err := os.WriteFile(unrelated, []byte(`{"unrelated":true}`), 0600); err != nil {
		t.Fatal(err)
	}
	rows, err := f.s.AuditResources(ctx)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, row := range rows {
		if row.Path == link {
			found = true
			if row.State != Unknown || row.Ref != (Ref{}) {
				t.Fatalf("symlink acquired ownership: %+v", row)
			}
		}
	}
	if !found {
		t.Fatal("symlink missing from inventory")
	}
	if data, err := os.ReadFile(filepath.Clean(unrelated)); err != nil || string(data) != `{"unrelated":true}` {
		t.Fatal("unrelated JSON changed")
	}
}

func TestResourceAuditDoesNotAdoptLookalikeNames(t *testing.T) {
	f := testFixture(t)
	ctx := context.Background()
	if _, err := f.s.Start(ctx, f.r); err != nil {
		t.Fatal(err)
	}
	id := string(f.r.Ref.Execution)
	paths := []string{
		filepath.Join(f.s.Root, id+".json.log"),
		filepath.Join(f.s.Root, id+"unexpected.accepted"),
		filepath.Join(f.s.Config.WorkspaceRoot, id+".log"),
		filepath.Join(f.s.Root, "sources", id+".json"),
	}
	for _, p := range paths {
		if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := f.s.AuditResources(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range paths {
		found := false
		for _, row := range rows {
			if row.Path == p {
				found = true
				if row.State != Unknown || row.Ref != (Ref{}) {
					t.Fatalf("unrelated entry adopted: %+v", row)
				}
			}
		}
		if !found {
			t.Fatalf("missing finding for %s", p)
		}
	}
}
