package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/guard"
)

// runTail is the ring that captures this run's output for the failure
// record, or nil when the configuration keeps nothing (run.tail off). The
// same ring serves the local scope and the remote builder's stream: a run
// that falls back from remote to local keeps what it streamed.
func (a *app) runTail() *guard.Tail {
	n, err := guard.ParseSize(a.cfg.Run.Tail) // KiB
	if a.cfg.Run.Tail == "off" || err != nil {
		return nil
	}
	if n >= int64(guard.MaxTailBytes)/1024 {
		return guard.NewTail(guard.MaxTailBytes)
	}
	return guard.NewTail(int(n) * 1024)
}

// runTailsDir is the directory of the kept tails: <stateDir>/run-tails.
func (a *app) runTailsDir() string {
	return filepath.Join(a.cfg.StateDir, guard.TailsDirName)
}

// redactOutput masks text the way the PostToolUse hook masks a tool result
// before the model sees it: every indexed value and every token pattern
// becomes [redacted: …]. A failing run's tail goes through it, so a
// credential a test printed reaches neither the kept tail nor the terminal
// line beekeeper reprints. An index that does not load still masks the
// patterns.
func (a *app) redactOutput(s string) string {
	ix, err := guard.LoadIndex(a.scanDir())
	if err != nil {
		ix = &guard.Index{} // the token patterns still run
	}
	out, _ := ix.Redact(s)
	return out
}

// tailRecord is one kept tail as `run tail` lists it.
type tailRecord struct {
	// Name is the tail's name: a local run's scope unit, a remote run's
	// id; what `run tail <name>` takes.
	Name string `json:"name"`
	// Run is the tail file's base name, kept for the reader.
	File  string `json:"file"`
	Exit  int    `json:"exit"`
	At    string `json:"at"`
	Bytes int64  `json:"bytes"`
	// Command is the command head the run recorded.
	Command string `json:"command"`
}

// runTailCmd lists the kept tails of failed runs, or prints one.
func (a *app) runTailCmd() *cobra.Command {
	var since string
	c := &cobra.Command{
		Use:   "tail [name]",
		Short: "The kept output tail of a failed run, or the list of them",
		Long: `A failed beekeeper run keeps the tail of its output — the "--- FAIL" lines
a truncated screen lost — masked like every output leaving the run, under
<stateDir>/run-tails/. Without a name this lists the kept tails, newest
first: the run's scope unit or remote id, its exit code, when it failed,
its size and its command. With a name it prints that tail, so a failing
test can be named without rerunning the suite. The name is the tail's,
whole or a part of it that no other kept tail matches; --since bounds the
listing (a duration like 24h, a time or RFC 3339).

run.tail sizes the kept tail (default 256K, capped at 1M, off keeps
nothing).`,
		Example: `  beekeeper run tail                 # what failed lately
  beekeeper run tail memcap-2516344  # print one run's tail`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var from time.Time
			if since != "" {
				t, err := reportTime(a.now, since, time.Local)
				if err != nil {
					return err
				}
				from = t
			}
			tails, err := listTails(a.runTailsDir())
			if err != nil {
				return err
			}
			if len(args) == 1 {
				want := args[0]
				var hits []tailRecord
				for _, t := range tails {
					if t.Name == want || strings.Contains(t.Name, want) {
						hits = append(hits, t)
					}
				}
				switch len(hits) {
				case 0:
					return usageErr("no kept tail matches %q: `beekeeper run tail` lists them", want)
				case 1:
					raw, err := os.ReadFile(filepath.Join(a.runTailsDir(), hits[0].File))
					if err != nil {
						return err
					}
					_, err = a.out.Write(raw)
					return err
				default:
					names := make([]string, len(hits))
					for i, t := range hits {
						names[i] = t.Name
					}
					return usageErr("%q matches %d kept tails, name one: %s", want, len(hits), strings.Join(names, ", "))
				}
			}
			var kept []tailRecord
			for _, t := range tails {
				at, err := time.Parse(time.RFC3339, t.At)
				if err == nil && at.Before(from) {
					continue
				}
				kept = append(kept, t)
			}
			if a.json {
				return a.printJSON(kept)
			}
			if len(kept) == 0 {
				a.say("no kept tail: a failed `beekeeper run` keeps one (run.tail)")
				return nil
			}
			w := a.table()
			for _, t := range kept {
				_, _ = fmt.Fprintf(w, "%s\texit %d\t%s\t%s\t%s\n", t.Name, t.Exit, clock(a.now, mustRFC3339(t.At)), tailSize(t.Bytes), t.Command)
			}
			return w.Flush()
		},
	}
	c.Flags().StringVar(&since, "since", "", "only tails from this on: a duration before now (24h), a time (15:04), a date (2006-01-02) or RFC 3339")
	return c
}

// listTails reads the kept tails of dir, newest first.
func listTails(dir string) ([]tailRecord, error) {
	es, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []tailRecord
	for _, e := range es {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".log") {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		r := tailRecord{Name: strings.TrimSuffix(e.Name(), ".log"), File: e.Name(), Bytes: fi.Size()}
		if raw, err := os.ReadFile(filepath.Join(dir, e.Name())); err == nil { //nolint:gosec // beekeeper's own tails directory
			r.Exit, r.At, r.Command, _ = parseTailHead(string(raw))
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].At > out[j].At })
	return out, nil
}

// parseTailHead reads a tail's first line: the exit code, the time and the
// command the run recorded. ok is false for a file without the header,
// which still lists by its name and size.
func parseTailHead(body string) (exit int, at, command string, ok bool) {
	line, _, _ := strings.Cut(body, "\n")
	rest, found := strings.CutPrefix(line, "# beekeeper run tail: exit ")
	if !found {
		return 0, "", "", false
	}
	code, rest, _ := strings.Cut(rest, " at ")
	at, command, _ = strings.Cut(rest, ": ")
	n, err := strconv.Atoi(code)
	if err != nil || at == "" {
		return 0, "", "", false
	}
	return n, at, command, true
}

// mustRFC3339 is the clock's reader for a timestamp the tail wrote; an
// unreadable one prints as the zero time.
func mustRFC3339(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

// tailSize says a kept tail's size: bytes below a KiB, KiB below a MiB,
// where a default tail sits, MiB above.
func tailSize(b int64) string {
	switch {
	case b < 1<<10:
		return fmt.Sprintf("%d B", b)
	case b < 1<<20:
		return fmt.Sprintf("%d KiB", b>>10)
	}
	return sizeStr(b)
}
