package cmd

import (
	"cmp"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/gofrs/flock"
	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/diskprune"
	"github.com/giantswarm/beekeeper/internal/state"
)

// The state home (~/.local/state) collects holders no lifecycle owns: a
// task's GOCACHE or CARGO_TARGET_DIR, its scratch named after its issue,
// saved release-candidate binaries, worktrees moved aside. disk holders
// lists each entry of the root, and each entry of its aside directories, by
// size and age with its owner, and says which is eligible for removal. It
// removes nothing: a person or a worker reads the list and reclaims.
//
// The owner is read from the name: a task number in it (ao1011, ku807,
// go-build-153) names the session whose task or record mentions that
// number, else the holder is its program's (~/.local/state/<program>). A
// holder is eligible once nothing in it was written for disk.holders.
// staleAfter (an aside entry: disk.holders.asideRetention), no process uses
// it, its task is not open, it holds no checkout outside an aside directory,
// and it is neither beekeeper's own state nor named in disk.holders.keep.

const (
	stateHoldersFile = "state-holders.json"
	stateHoldersLock = "state-holders.lock"
	// stateHoldersShown is how many eligible holders a STATE HOLDERS line
	// names.
	stateHoldersShown = 5
)

// holder is one entry of the state home, or of one of its aside
// directories.
type holder struct {
	Name    string    `json:"name"`
	Path    string    `json:"path"`
	Bytes   int64     `json:"bytes"`
	Written time.Time `json:"written,omitzero"`
	Owner   string    `json:"owner"`
	// Task is the task or issue of the session its name names; TaskOpen
	// says that session still works it.
	Task     string `json:"task,omitempty"`
	TaskOpen bool   `json:"taskOpen,omitempty"`
	Aside    bool   `json:"aside,omitempty"`
	Eligible bool   `json:"eligible"`
	Why      string `json:"why"`
}

// holderMachine is what the listing reads besides the files: the paths
// running processes use and the desk's roster and session records.
type holderMachine struct {
	refs []string
	st   *state.State
}

func (a *app) holderMachine() (holderMachine, error) {
	refs, _, err := diskReferences()
	if err != nil {
		return holderMachine{}, fmt.Errorf("process references unavailable: %w", err)
	}
	st, err := a.store.Read()
	if err != nil {
		return holderMachine{}, err
	}
	return holderMachine{refs: refs, st: st}, nil
}

var liveHolders = (*app).holderMachine

// holdersRun is one listing of the state home.
type holdersRun struct {
	Root     string   `json:"root"`
	Holders  []holder `json:"holders"`
	Bytes    int64    `json:"bytes"`
	Eligible int64    `json:"eligibleBytes"`
	err      error
}

// eligible are the holders eligible for removal, largest first.
func (r holdersRun) eligible() []holder {
	return slices.DeleteFunc(slices.Clone(r.Holders), func(h holder) bool { return !h.Eligible })
}

// summary says the eligible holders in one line, the largest by name.
func (r holdersRun) summary() string {
	el := r.eligible()
	line := fmt.Sprintf("%d holders under %s eligible for removal, %s of %s (nothing removed)", len(el), r.Root, gibStr(r.Eligible), gibStr(r.Bytes))
	parts := make([]string, 0, stateHoldersShown)
	for _, h := range el[:min(stateHoldersShown, len(el))] {
		parts = append(parts, fmt.Sprintf("%s %s (%s, %s)", h.Name, sizeStr(h.Bytes), h.Owner, h.Why))
	}
	if len(parts) > 0 {
		line += ": " + strings.Join(parts, ", ")
	}
	return line + "; beekeeper disk holders lists them all"
}

// listHolders reads every holder of the state home, largest first.
func (a *app) listHolders(read func() (holderMachine, error)) holdersRun {
	cfg := a.cfg.Disk.Holders
	r := holdersRun{Root: cfg.Root}
	m, err := read()
	if err != nil {
		r.err = err
		return r
	}
	entries, err := os.ReadDir(cfg.Root)
	if err != nil {
		r.err = err
		return r
	}
	for _, e := range entries {
		p := filepath.Join(cfg.Root, e.Name())
		if !e.IsDir() || !slices.Contains(cfg.AsideDirs, e.Name()) {
			r.Holders = append(r.Holders, a.inspectHolder(m, e.Name(), p, false))
			continue
		}
		aside, err := os.ReadDir(p)
		if err != nil {
			r.err = errors.Join(r.err, err)
			continue
		}
		for _, c := range aside {
			r.Holders = append(r.Holders, a.inspectHolder(m, e.Name()+"/"+c.Name(), filepath.Join(p, c.Name()), true))
		}
	}
	slices.SortFunc(r.Holders, func(x, y holder) int { return cmp.Or(cmp.Compare(y.Bytes, x.Bytes), cmp.Compare(x.Name, y.Name)) })
	for _, h := range r.Holders {
		r.Bytes += h.Bytes
		if h.Eligible {
			r.Eligible += h.Bytes
		}
	}
	return r
}

// inspectHolder sizes one holder, names its owner and judges it.
func (a *app) inspectHolder(m holderMachine, name, path string, aside bool) holder {
	cfg := a.cfg.Disk.Holders
	u := diskprune.Inspect(path)
	h := holder{Name: name, Path: path, Bytes: u.Bytes, Written: u.Written, Aside: aside}
	h.Owner, h.Task, h.TaskOpen = holderOwner(filepath.Base(path), m.st)
	window := cfg.StaleAfter.Duration
	if aside {
		window = cfg.AsideRetention.Duration
	}
	switch {
	case filepath.Clean(path) == filepath.Clean(a.cfg.StateDir):
		h.Owner, h.Why = pluginName, "beekeeper's own state"
	case !aside && slices.Contains(cfg.Keep, name):
		h.Why = "kept by disk.holders.keep"
	case diskprune.InUse(path, m.refs):
		h.Why = "in use by a running process"
	case h.TaskOpen:
		h.Why = "its task is open"
	case u.Unreadable:
		h.Why = "partly unreadable"
	case u.Checkout && !aside:
		h.Why = "holds a checkout: the worktree prune decides"
	case a.now.Sub(u.Written) < window:
		h.Why = "written " + ago(a.now, u.Written) + " ago"
	default:
		h.Eligible = true
		h.Why = "unused for " + ago(a.now, u.Written)
		if u.Checkout {
			h.Why += ", holds a checkout: check its work first"
		}
	}
	return h
}

// taskNumber is a number in a name that can be a task's issue: three digits
// or more, so a version's 2 or 10 does not count.
var taskNumber = regexp.MustCompile(`\d{3,}`)

// holderOwner names the session whose task, last task or record mentions a
// task number of name, an open one first, and says whether it is open. A
// name without a task number is its program's; one whose number no session
// mentions is unowned.
func holderOwner(name string, st *state.State) (owner, task string, open bool) {
	nums := taskNumber.FindAllString(name, -1)
	if len(nums) == 0 {
		return "program " + name, "", false
	}
	mentions := func(s string) bool {
		return slices.ContainsFunc(taskNumber.FindAllString(s, -1), func(n string) bool { return slices.Contains(nums, n) })
	}
	if st != nil {
		for _, ag := range st.Agents {
			if ag.Task != "" && !ag.Done && mentions(ag.Task) {
				return ag.Name, ag.Task, true
			}
		}
		for _, r := range st.Records {
			if r.Ended.IsZero() && mentions(r.Issue) {
				return r.Session.Name, r.Issue, true
			}
		}
		for _, ag := range st.Agents {
			if mentions(ag.Task) || mentions(ag.LastTask) {
				return ag.Name, cmp.Or(ag.Task, ag.LastTask), false
			}
		}
		for _, r := range slices.Backward(st.Records) {
			if mentions(r.Issue) {
				return r.Session.Name, r.Issue, false
			}
		}
	}
	return "unowned (task " + strings.Join(nums, ", ") + ")", "", false
}

// stateHoldersMark is the watches' shared last listing: when it ran and
// what it said.
type stateHoldersMark struct {
	At   time.Time `json:"at"`
	On   bool      `json:"on"`
	Line string    `json:"line,omitempty"`
}

// stateHolders lists the state home every disk.holders.every, in the
// background, and says STATE HOLDERS while the eligible holders take
// disk.holders.warnGiB or more. A watch that finds a fresh listing another
// watch made says that listing's condition instead of walking again.
func (w *watcher) stateHolders(now time.Time) {
	cfg := w.cfg.Disk.Holders
	if !w.holdersOn || cfg.WarnGiB < 0 || !w.holdering.CompareAndSwap(false, true) {
		return
	}
	if now.Before(w.holdersNext) {
		w.holdering.Store(false)
		return
	}
	d := *w.app
	d.now = now
	w.bg.Add(1)
	go func() {
		defer w.bg.Done()
		defer w.holdering.Store(false)
		var m stateHoldersMark
		if _, _ = w.store.ReadFile(stateHoldersFile, &m); now.Sub(m.At) < cfg.Every.Duration {
			w.holdersNext = m.At.Add(cfg.Every.Duration)
			w.check("stateholders", m.On, "STATE HOLDERS: %s", m.Line)
			return
		}
		lock := flock.New(filepath.Join(w.store.Dir(), stateHoldersLock))
		if ok, err := lock.TryLock(); err != nil || !ok {
			w.holdersNext = now.Add(goCacheRetry)
			return
		}
		defer func() { _ = lock.Unlock() }()
		r := d.listHolders(func() (holderMachine, error) { return liveHolders(&d) })
		if r.err != nil && len(r.Holders) == 0 {
			// Unread, the holders are unknown: the next look tries again.
			w.holdersNext = now.Add(goCacheRetry)
			return
		}
		w.holdersNext = now.Add(cfg.Every.Duration)
		m = stateHoldersMark{At: now, On: r.Eligible >= int64(cfg.WarnGiB)<<30, Line: r.summary()}
		_ = w.store.WriteFile(stateHoldersFile, m)
		w.check("stateholders", m.On, "STATE HOLDERS: %s", m.Line)
	}()
}

func (a *app) diskHoldersCmd() *cobra.Command {
	var minMiB int
	c := &cobra.Command{
		Use:   "holders",
		Short: "List the state home's holders by size and age with their owners; remove nothing",
		Long: `List each entry of disk.holders.root (default ~/.local/state), and each
entry of its aside directories (disk.holders.asideDirs), largest first:
its size, when anything in it was last written, its owner and whether it is
eligible for removal.

The owner is read from the name: a task number in it (ao1011, go-build-153)
names the roster agent or session record that mentions it, open tasks
first; a name without one is its program's. A holder is eligible once
nothing in it was written for disk.holders.staleAfter (an aside entry:
disk.holders.asideRetention), no running process uses it, its task is not
open, it holds no git checkout outside an aside directory, and it is
neither beekeeper's own state nor named in disk.holders.keep.

Nothing is removed. The watch says STATE HOLDERS while the eligible
holders take disk.holders.warnGiB or more.`,
		Args: cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			a.now = time.Now()
			r := a.listHolders(func() (holderMachine, error) { return liveHolders(a) })
			if a.json {
				return errors.Join(a.printJSON(r), r.err)
			}
			if r.err != nil && len(r.Holders) == 0 {
				return r.err
			}
			_, _ = fmt.Fprintf(a.out, "%s: %s in %d holders; eligible for removal: %d, %s (nothing is removed)\n",
				r.Root, gibStr(r.Bytes), len(r.Holders), len(r.eligible()), gibStr(r.Eligible))
			w := a.table()
			_, _ = fmt.Fprintln(w, "  SIZE\tHOLDER\tOWNER\tWRITTEN\tELIGIBLE")
			var hidden int
			var hiddenBytes int64
			for _, h := range r.Holders {
				if h.Bytes < int64(minMiB)<<20 {
					hidden++
					hiddenBytes += h.Bytes
					continue
				}
				verdict := "no: " + h.Why
				if h.Eligible {
					verdict = "yes: " + h.Why
				}
				owner := h.Owner
				if h.Task != "" {
					owner += " (" + h.Task + ")"
				}
				_, _ = fmt.Fprintf(w, "  %s\t%s\t%s\t%s ago\t%s\n", sizeStr(h.Bytes), h.Name, truncate(owner, 60), ago(a.now, h.Written), verdict)
			}
			_ = w.Flush()
			if hidden > 0 {
				_, _ = fmt.Fprintf(a.out, "  and %d holders under %d MiB, %s (--min-mib 0 lists them)\n", hidden, minMiB, sizeStr(hiddenBytes))
			}
			return r.err
		},
	}
	c.Flags().IntVar(&minMiB, "min-mib", 100, "list only holders of at least this many MiB")
	return c
}
