package guard

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/machine"
	"github.com/giantswarm/beekeeper/internal/platform"
)

func TestTailRingKeepsTheLastBytes(t *testing.T) {
	for _, c := range []struct {
		name string
		max  int
		in   []string
		want string
		cut  bool
	}{
		{"under the ring", 64, []string{"aa\nbb\n"}, "aa\nbb\n", false},
		{"wrap mid-line", 8, []string{"1234", "56789"}, "23456789", true},
		{"one write past the ring", 4, []string{"abcdefghij"}, "ghij", true},
		{"the ring's own size", 4, []string{"abcd"}, "abcd", true},
		{"the last write is the tail", 4, []string{"0123456789", "wxyz"}, "wxyz", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			tr := NewTail(c.max)
			for _, s := range c.in {
				tr.write([]byte(s))
			}
			got, cut := tr.Bytes()
			if string(got) != c.want || cut != c.cut {
				t.Errorf("Bytes() = %q, cut %v: want %q, cut %v", got, cut, c.want, c.cut)
			}
		})
	}
	if NewTail(0) != nil {
		t.Error("NewTail(0) keeps something")
	}
	if tr := NewTail(MaxTailBytes + 1<<20); tr.max != MaxTailBytes {
		t.Errorf("ring of %d bytes past the cap", tr.max)
	}
}

// noKills is the machine with no cap kill: a failed run's end reads the
// journal once and finds nothing of its scope.
type noKills struct{ platform.Machine }

func (noKills) OOMKills(context.Context, time.Time) ([]machine.OOMKill, error) { return nil, nil }

func TestKeepTailMasksAndCutsTheFirstLine(t *testing.T) {
	ix := plantedIndex(t)
	redact := func(s string) string { out, _ := ix.Redact(s); return out }
	dir := t.TempDir()
	tr := NewTail(64)
	tr.write([]byte(strings.Repeat("0", 70) + "\n")) // 71 bytes: the ring keeps the last 63 zeros and the newline
	tr.write([]byte("--- FAIL: TestFlaky (0.04s)\ntoken=" + planted + "\n"))
	at, masked, err := KeepTail(dir, "memcap-1-2", "go test ./...", 1, tr, redact)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(at, filepath.Join("memcap-1-2.log")) {
		t.Errorf("path %q", at)
	}
	raw, err := os.ReadFile(at) //nolint:gosec // the test's own file
	if err != nil {
		t.Fatal(err)
	}
	body := string(raw)
	lines := strings.Split(body, "\n")
	if !strings.HasPrefix(lines[0], "# beekeeper run tail: exit 1 at ") || !strings.Contains(lines[0], ": go test ./...") {
		t.Errorf("the header: %q", lines[0])
	}
	if lines[1] != "--- FAIL: TestFlaky (0.04s)" {
		t.Errorf("the cut first line survived, or the failure block moved: %q", lines[1])
	}
	if lines[2] != "token=[redacted: "+plantedRef+"]" {
		t.Errorf("the kept tail is not masked: %q", lines[2])
	}
	if strings.Contains(body, planted) || strings.Contains(body, strings.Repeat("0", 10)) {
		t.Errorf("the kept tail carries the credential or the dropped line:\n%s", body)
	}
	if !strings.Contains(masked, "[redacted: ") || strings.Contains(masked, planted) {
		t.Errorf("masked answer = %q", masked)
	}
}

func TestKeepTailReplacesAndTrimsTheDirectory(t *testing.T) {
	dir := t.TempDir()
	one := func(name string, at time.Time) string {
		tr := NewTail(16)
		tr.write([]byte("x"))
		path, _, err := KeepTail(dir, name, "go test", 1, tr, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, at, at); err != nil {
			t.Fatal(err)
		}
		return path
	}
	oldest := one("oldest", time.Now().Add(-time.Duration(tailsKeep+5)*time.Hour))
	for i := range tailsKeep - 1 {
		one(fmt.Sprintf("run%d", i), time.Now().Add(-time.Duration(tailsKeep-i)*time.Minute))
	}
	newest := one("newest", time.Now().Add(-time.Second))
	es, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(es) != tailsKeep {
		t.Fatalf("%d tails kept, want the bound of %d", len(es), tailsKeep)
	}
	if _, err := os.Stat(oldest); !os.IsNotExist(err) {
		t.Errorf("the oldest tail survived the trim")
	}
	if _, err := os.Stat(newest); err != nil {
		t.Errorf("the newest tail is gone: %v", err)
	}
}

// runTailOptions is the options of a run that keeps a tail: a ring of
// 1 KiB under dir, the planted index masking it, no cap kill to read.
func runTailOptions(t *testing.T, dir string) Options {
	t.Helper()
	ix := plantedIndex(t)
	return Options{Max: "64M", Swap: "0", SlotDir: t.TempDir(), Slots: 1, Wait: time.Second,
		Tail: NewTail(1024), TailDir: dir, Redact: func(s string) string { out, _ := ix.Redact(s); return out }}
}

// runCappedOn is Run with the fake capper and no OOM journal to read.
func runCappedOn(t *testing.T, o Options, argv []string) int {
	t.Helper()
	f := &fakeCapper{}
	savedC, savedM := plat.Capper, plat.Machine
	plat.Capper, plat.Machine = f, noKills{Machine: savedM}
	t.Cleanup(func() { plat.Capper, plat.Machine = savedC, savedM })
	return Run(o, argv)
}

// A failed run keeps its masked output tail, names it in the end event and
// prints it once at the end, so the failing test's name needs no rerun; a
// run that passes keeps nothing.
func TestRunKeepsTheFailedTail(t *testing.T) {
	failCmd := []string{"sh", "-c", `echo 'ok  pkg/a'; echo '--- FAIL: TestFlaky (0.11s)'; echo 'token='` + planted + `; echo FAIL; exit 1`}
	for _, c := range []struct {
		name string
		cmd  []string
		rc   int
	}{
		{"failed", failCmd, 1},
		{"passed", []string{"sh", "-c", "echo ok; exit 0"}, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			var stderr strings.Builder
			var ends []string
			o := runTailOptions(t, dir)
			o.Stderr = &stderr
			o.Record = func(verb, detail string) {
				if verb == VerbEnd {
					ends = append(ends, detail)
				}
			}
			rc := runCappedOn(t, o, c.cmd)
			if rc != c.rc {
				t.Fatalf("exit %d, want %d:\n%s", rc, c.rc, stderr.String())
			}
			es, err := os.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			if c.rc == 0 {
				if len(es) != 0 || strings.Contains(stderr.String(), "beekeeper run tail") {
					t.Fatalf("a passed run kept a tail: %d files, stderr:\n%s", len(es), stderr.String())
				}
				return
			}
			if len(es) != 1 {
				t.Fatalf("%d tails kept, want one", len(es))
			}
			raw, err := os.ReadFile(filepath.Join(dir, es[0].Name())) //nolint:gosec // the test's own file
			if err != nil {
				t.Fatal(err)
			}
			body := string(raw)
			if !strings.Contains(body, "--- FAIL: TestFlaky") || strings.Contains(body, planted) || !strings.Contains(body, "[redacted: "+plantedRef+"]") {
				t.Errorf("the tail:\n%s", body)
			}
			if want := strings.TrimSuffix(es[0].Name(), ".log"); !strings.HasPrefix(want, ScopePrefix) {
				t.Errorf("tail %q is not named by the run's scope", es[0].Name())
			}
			out := stderr.String()
			if !strings.Contains(out, "beekeeper run: the failed command's tail (exit 1)") || !strings.Contains(out, "--- FAIL: TestFlaky") {
				t.Errorf("the failure did not print the tail:\n%s", out)
			}
			if strings.Contains(out, planted) {
				t.Errorf("the printed tail carries the credential:\n%s", out)
			}
			if len(ends) != 1 || !strings.Contains(ends[0], "exit 1 after") || !strings.Contains(ends[0], "tail "+filepath.Join(dir, es[0].Name())) {
				t.Errorf("end event %q", ends)
			}
		})
	}
}

// Output past the ring keeps its end — the failure block — and loses its
// start: the ring never grows past run.tail.
func TestRunTailOversizedOutputKeepsTheEnd(t *testing.T) {
	dir := t.TempDir()
	o := runTailOptions(t, dir)
	o.Stderr = &strings.Builder{}
	rc := runCappedOn(t, o, []string{"sh", "-c", `echo HEAD; head -c 20000 /dev/zero | tr '\0' x; echo; echo '--- FAIL: TestBig (1s)'; exit 1`})
	if rc != 1 {
		t.Fatalf("exit %d", rc)
	}
	es, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(es) != 1 {
		t.Fatalf("%d tails, want one", len(es))
	}
	raw, err := os.ReadFile(filepath.Join(dir, es[0].Name())) //nolint:gosec // the test's own file
	if err != nil {
		t.Fatal(err)
	}
	body := string(raw)
	header := strings.IndexByte(body, '\n')
	if header < 0 || !strings.HasPrefix(body, "# beekeeper run tail: exit 1 at ") {
		t.Fatalf("header: %q", body)
	}
	if tail := body[header+1:]; !strings.Contains(tail, "--- FAIL: TestBig") || strings.Contains(tail, "HEAD") || len(tail) > 1025 {
		t.Errorf("the oversized tail lost its end, kept its start or outgrew the ring of 1024:\n%s", tail)
	}
}

// With no index the kept tail still masks the token patterns: a caller
// that forgot the index stores no token.
func TestKeepTailWithoutAnIndexMasksTokens(t *testing.T) {
	dir := t.TempDir()
	tr := NewTail(64)
	tr.write([]byte("ghp_" + strings.Repeat("A1b2", 9)))
	_, masked, err := KeepTail(dir, "run", "go test", 1, tr, nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(masked, "A1b2A1b2") || !strings.Contains(masked, "[redacted: github-pat]") {
		t.Errorf("masked tail kept the token: %q", masked)
	}
}
