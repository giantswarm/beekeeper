//go:build perf && linux

package perfsuite

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/state"
)

// The desk of record: the coordinator's size when the suite was written
// (about 100 sessions, a 6 MB state.json, a 12 MB event log, 150 open pull
// requests and three execution hosts).
const (
	deskSessions   = 100
	deskStateBytes = 6 << 20
	deskEventBytes = 12 << 20
	deskPulls      = 150
	deskHosts      = 3
	fixturePerson  = "fixture-person"
)

var (
	binaryOnce    sync.Once
	binaryPath    string
	binaryDir     string
	binaryErr     error
	binaryVersion = "not built"
)

// beekeeperBinary is BEEKEEPER_PERF_BINARY, or this tree built once.
func beekeeperBinary(t *testing.T) string {
	t.Helper()
	binaryOnce.Do(func() {
		if p := os.Getenv("BEEKEEPER_PERF_BINARY"); p != "" {
			binaryPath, binaryErr = filepath.Abs(p)
		} else {
			binaryDir, binaryErr = os.MkdirTemp("", "beekeeper-perf-bin-")
			if binaryErr != nil {
				return
			}
			binaryPath = filepath.Join(binaryDir, "beekeeper")
			root, err := filepath.Abs("../..")
			if err != nil {
				binaryErr = err
				return
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
			defer cancel()
			c := exec.CommandContext(ctx, "go", "build", "-o", binaryPath, ".")
			c.Dir = root
			if out, err := c.CombinedOutput(); err != nil {
				binaryErr = fmt.Errorf("build beekeeper: %w\n%s", err, out)
				return
			}
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		out, err := exec.CommandContext(ctx, binaryPath, "--version").CombinedOutput() //nolint:gosec // binary under test
		if err == nil {
			binaryVersion = strings.TrimSpace(string(out))
		}
	})
	if binaryErr != nil {
		t.Fatal(binaryErr)
	}
	return binaryPath
}

func cleanupBinary() {
	if binaryDir != "" {
		_ = os.RemoveAll(binaryDir)
	}
}

// desk is a generated desk: its state, event log, configuration and PATH of
// fakes, and a fake GitHub API. Nothing in it reaches the network or the
// machine's own desk.
type desk struct {
	t        *testing.T
	dir      string
	bin      string
	stateDir string
	env      []string
	store    *state.FileStore
	httpMiss atomic.Int64
	missMu   sync.Mutex
	misses   map[string]bool
	agents   []state.Agent
	checkout string
}

type deskOptions struct {
	loopInterval time.Duration
	sessions     int
	stateBytes   int
	eventBytes   int
	systemd      bool
}

func defaultDesk() deskOptions {
	return deskOptions{loopInterval: 5 * time.Second, sessions: deskSessions, stateBytes: deskStateBytes, eventBytes: deskEventBytes}
}

func newDesk(t *testing.T, o deskOptions) *desk {
	t.Helper()
	d := &desk{t: t, dir: t.TempDir(), bin: beekeeperBinary(t), misses: map[string]bool{}}
	d.stateDir = filepath.Join(d.dir, "state")
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	binDir := filepath.Join(d.dir, "bin")
	must(os.MkdirAll(binDir, 0o700))
	self, err := os.Executable()
	must(err)
	for _, name := range fakes {
		must(os.Symlink(self, filepath.Join(binDir, name)))
	}
	must(os.Symlink(d.bin, filepath.Join(binDir, "beekeeper")))
	realGit, _ := exec.LookPath("git")

	now := time.Now().UTC()
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Limit", "5000")
		w.Header().Set("X-RateLimit-Remaining", "4900")
		w.Header().Set("X-RateLimit-Reset", fmt.Sprint(now.Add(time.Hour).Unix()))
		switch r.URL.Path {
		case "/graphql":
			_, _ = fmt.Fprintf(w, `{"data":{"viewer":{"login":"fixture-bot"},"rateLimit":{"limit":5000,"remaining":4900,"used":100,"resetAt":%q}}}`, now.Add(time.Hour).Format(time.RFC3339))
		case "/rate_limit":
			_, _ = fmt.Fprint(w, `{"resources":{"core":{"limit":5000,"remaining":4900},"graphql":{"limit":5000,"remaining":4900},"search":{"limit":30,"remaining":30},"code_search":{"limit":10,"remaining":10}}}`)
		default:
			if parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/"); len(parts) == 3 && parts[0] == "repos" {
				_, _ = fmt.Fprintf(w, `{"full_name":%q,"default_branch":"main","private":false}`, parts[1]+"/"+parts[2])
				return
			}
			d.httpMiss.Add(1)
			d.missMu.Lock()
			if len(d.misses) < 20 {
				d.misses[r.Method+" "+r.URL.Path] = true
			}
			d.missMu.Unlock()
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
		}
	}))
	t.Cleanup(api.Close)

	home := filepath.Join(d.dir, "home")
	must(os.MkdirAll(home, 0o700))
	d.env = []string{
		"HOME=" + home, "XDG_CONFIG_HOME=" + home, "XDG_STATE_HOME=" + home, "XDG_CACHE_HOME=" + filepath.Join(home, "cache"),
		"PATH=" + binDir, "LANG=C.UTF-8", "TZ=UTC", "BEEKEEPER_CONFIG=" + filepath.Join(d.dir, "config.yaml"),
		"BEEKEEPER_GITHUB_API=" + api.URL, "MEMCAP_STATE=" + filepath.Join(d.dir, "memcap"),
		fakeDirEnv + "=" + d.dir, "PERF_REAL_GIT=" + realGit, fmt.Sprintf("PERF_PULLS=%d", deskPulls),
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "GIT_ALLOW_PROTOCOL=file", "GIT_TERMINAL_PROMPT=0",
		"HTTP_PROXY=http://127.0.0.1:1", "HTTPS_PROXY=http://127.0.0.1:1", "NO_PROXY=127.0.0.1",
	}
	if o.systemd {
		for _, name := range []string{"XDG_RUNTIME_DIR", "DBUS_SESSION_BUS_ADDRESS"} {
			if v := os.Getenv(name); v != "" {
				d.env = append(d.env, name+"="+v)
			}
		}
	}
	// One checkout of the fixture repository, as agents start --dir needs.
	d.checkout = filepath.Join(d.dir, "checkout")
	if realGit != "" {
		for _, args := range [][]string{{"init", "-q", "-b", "main", d.checkout}, {"-C", d.checkout, "remote", "add", "origin", "https://github.com/" + fixtureRepo + ".git"}} {
			c := exec.CommandContext(t.Context(), realGit, args...) //nolint:gosec // fixture git
			c.Env = d.env
			if out, err := c.CombinedOutput(); err != nil {
				t.Fatalf("fixture checkout: %v: %s", err, out)
			}
		}
	}
	must(os.WriteFile(filepath.Join(d.dir, "config.yaml"), []byte(deskConfig(d.stateDir, d.checkout, o.loopInterval)), 0o600))
	must(os.MkdirAll(filepath.Join(d.dir, "work"), 0o700))

	d.store, err = state.Open(d.stateDir)
	must(err)
	d.agents = fixtureAgents(now, o.sessions)
	must(d.store.Update(func(st *state.State) ([]state.Event, error) {
		fillState(st, now, d.agents, o.stateBytes)
		return nil, nil
	}))
	if fi, err := os.Stat(filepath.Join(d.stateDir, "state.json")); err == nil {
		rep.note("%s: state.json %d bytes, %d sessions", t.Name(), fi.Size(), o.sessions)
	}
	n, err := writeEvents(filepath.Join(d.stateDir, "events.jsonl"), now.Add(-30*24*time.Hour), o.eventBytes)
	must(err)
	rep.note("%s: events.jsonl %d bytes", t.Name(), n)
	t.Cleanup(d.keepArtifacts)
	return d
}

func deskConfig(stateDir, checkout string, loopInterval time.Duration) string {
	var hosts strings.Builder
	for i := range deskHosts {
		name := fmt.Sprintf("host-%c", 'a'+i)
		fmt.Fprintf(&hosts, "    %s:\n      identity: %s\n      ssh: %s.example\n      executable: /usr/local/bin/beekeeper\n      config: /etc/beekeeper/host.yaml\n      workspaceRoot: /srv/workspaces\n      repositories: {%s: /srv/source}\n      harnesses: [codex]\n      policyDigest: sha256:%s\n      maxBusy: 2\n", name, name, name, fixtureRepo, strings.Repeat("a", 64))
	}
	return fmt.Sprintf(`board:
  owner: fixture
  project: 1
  tidyEvery: -1s
  order:
  - name: ready
    status: [Ready]
capacity:
  ceiling: 0
identity:
  host: fixture
stateDir: %s
disk:
  checkouts: [%q]
github:
  floor: -1
  allowedOrgs: [fixture]
supervisor:
  loopInterval: %s
  ownership:
    authors: [fixture-bot]
agents:
  hosts:
%s`, stateDir, checkout, loopInterval, hosts.String())
}

// fixtureAgents is the roster: a third each working, idle and parked on a
// pull request's desk review.
func fixtureAgents(now time.Time, n int) []state.Agent {
	out := make([]state.Agent, 0, n)
	for i := range n {
		p := state.Party{Session: fmt.Sprintf("0000%04x-0000-4000-8000-%012x", i, i), Name: fmt.Sprintf("perf worker %d", i), Person: fixturePerson, Host: "fixture"}
		ag := state.Agent{Party: p, Registered: now.Add(-time.Duration(n-i) * time.Hour), ActivityAt: now.Add(-time.Duration(i) * time.Minute)}
		task := fmt.Sprintf("Work https://github.com/%s/issues/%d to the finish", fixtureRepo, i+1)
		switch i % 3 {
		case 0:
			ag.Task, ag.AssignedAt = task, now.Add(-time.Hour)
		case 1:
			ag.IdleSince, ag.LastTask = now.Add(-2*time.Hour), task
		case 2:
			ag.Task = task
			ag.Park = &state.Park{By: p, At: now.Add(-30 * time.Minute), On: fmt.Sprintf("%s#%d", fixtureRepo, 1000+i), Waits: "desk review"}
		}
		out = append(out, ag)
	}
	return out
}

// fillState fills st like the coordinator's: the roster and what it leaves
// behind (starts, serve records, review parks, open notes, worker reports),
// worker reports padding it to size bytes of JSON.
func fillState(st *state.State, now time.Time, agents []state.Agent, size int) {
	st.AutoInstallPaused = true
	st.Budget = &state.Budget{Remaining: 4900, Limit: 5000, Reset: now.Add(time.Hour), At: now, GraphQL: &state.GraphQL{Remaining: 4900, Limit: 5000, Reset: now.Add(time.Hour), At: now}}
	st.Agents = agents
	guide := state.Party{Name: "perf guide", Person: fixturePerson}
	for i, ag := range agents {
		st.Starts = append(st.Starts, state.Start{Party: ag.Party, Mode: state.ModeBypass, Dir: "/srv/source", By: guide, At: ag.Registered, Harness: "claude", Model: "fixture-model", Task: ag.Task})
		st.Records = append(st.Records, state.Record{Session: ag.Party, Issue: fmt.Sprintf("%s#%d", fixtureRepo, i+1), By: guide, At: ag.Registered})
		if ag.Park != nil {
			st.ReviewParks = append(st.ReviewParks, state.ReviewPark{On: ag.Park.On, At: ag.Park.At, ReviewHead: fmt.Sprintf("%040x", 0xbee0000+i), Agent: ag.Name, Party: ag.Party})
		}
	}
	for i := range 300 {
		st.Notes = append(st.Notes, state.Note{ID: i + 1, For: fixturePerson, Text: fmt.Sprintf("Fixture decision %d? %s", i+1, strings.Repeat("context ", 70)), By: guide, At: now.Add(-time.Duration(i) * time.Minute)})
	}
	st.NextNote = 301
	workerReport := func(i int) state.WorkerReport {
		ag := agents[i%len(agents)]
		return state.WorkerReport{Finished: true, By: ag.Party, At: now.Add(-time.Duration(i) * time.Minute), Task: ag.Task, Text: fmt.Sprintf("https://github.com/%s/pull/%d merged; %s", fixtureRepo, 1000+i, strings.Repeat("proof ", 140))}
	}
	raw, _ := json.Marshal(st)
	one, _ := json.Marshal(workerReport(0))
	for i := 0; len(raw)+i*len(one) < size; i++ {
		st.WorkerReports = append(st.WorkerReports, workerReport(i))
	}
}

// writeEvents appends a realistic mix of events from since until the log
// holds size bytes, and returns its size.
func writeEvents(path string, since time.Time, size int) (int64, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600) //nolint:gosec // fixture event log
	if err != nil {
		return 0, err
	}
	w := bufio.NewWriterSize(f, 1<<20)
	verbs := []string{"supervisor.stage", "agents.park", "agents.wake", "watch.budget-state", "note.add", "supervisor.review", "agents.idle", "board.move"}
	fi, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return 0, err
	}
	written := fi.Size()
	for i := 0; written < int64(size); i++ {
		ev := state.Event{
			At:      since.Add(time.Duration(i) * time.Second),
			By:      state.Party{Name: fmt.Sprintf("perf worker %d", i%deskSessions), Person: fixturePerson},
			Verb:    verbs[i%len(verbs)],
			Subject: fmt.Sprintf("%s#%d", fixtureRepo, 1000+i%deskPulls),
			Detail:  fmt.Sprintf("[fixture %d] %s", i, strings.Repeat("detail ", 40)),
		}
		raw, err := json.Marshal(ev)
		if err != nil {
			_ = f.Close()
			return 0, err
		}
		n, err := w.Write(append(raw, '\n'))
		if err != nil {
			_ = f.Close()
			return 0, err
		}
		written += int64(n)
	}
	if err := w.Flush(); err != nil {
		_ = f.Close()
		return 0, err
	}
	return written, f.Close()
}

// traceCount is the number of fake processes started so far.
func (d *desk) traceCount() int {
	raw, err := os.ReadFile(filepath.Join(d.dir, "trace.jsonl"))
	if err != nil {
		return 0
	}
	return strings.Count(string(raw), "\n")
}

// keepArtifacts keeps what the fakes saw: the calls by kind and the calls no
// fake answers, with the fixture's path replaced.
func (d *desk) keepArtifacts() {
	dest := os.Getenv("BEEKEEPER_PERF_REPORT")
	if dest == "" {
		return
	}
	dest = filepath.Join(dest, strings.ReplaceAll(d.t.Name(), "/", "-"))
	if err := os.MkdirAll(dest, 0o700); err != nil {
		return
	}
	for _, name := range []string{"trace.jsonl", "unknown.jsonl", "loop.log"} {
		raw, err := os.ReadFile(filepath.Join(d.dir, name)) //nolint:gosec // fixture file
		if err != nil {
			continue
		}
		_ = os.WriteFile(filepath.Join(dest, name), []byte(strings.ReplaceAll(string(raw), d.dir, "<fixture>")), 0o600)
	}
	if n := d.httpMiss.Load(); n > 0 {
		d.missMu.Lock()
		paths := make([]string, 0, len(d.misses))
		for p := range d.misses {
			paths = append(paths, p)
		}
		d.missMu.Unlock()
		slices.Sort(paths)
		rep.note("%s: %d GitHub API requests no fake answers: %s", d.t.Name(), n, strings.Join(paths, ", "))
	}
}
