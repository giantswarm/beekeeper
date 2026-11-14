package cmd

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/giantswarm/beekeeper/internal/platform"
	"github.com/giantswarm/beekeeper/internal/proc"
	"github.com/giantswarm/beekeeper/pkg/project"
)

// staleKey starts the condition key of a watch whose binary was replaced.
const staleKey = "stale-binary "

// staleWatches says each running beekeeper watch, this one included, whose
// binary self-update replaced: it still runs the old code, which only a
// re-arm (a restart) leaves. One WATCH STALE line per watch names the
// version it runs and the one installed, and one ENDED line follows once it
// exits. The watch never re-executes itself: its owner re-arms it.
func (w *watcher) staleWatches(ctx context.Context, t *proc.Table) {
	var owners map[int]string
	if o := w.owners.Load(); o != nil {
		owners = *o
	}
	found := map[string]bool{}
	for _, p := range t.ByPID {
		if !isWatch(p) {
			continue
		}
		path, ok := w.replacedBinary(p.PID)
		if !ok {
			continue
		}
		key := fmt.Sprintf("%s%d", staleKey, p.PID)
		found[key] = true
		if !w.isActive(key) {
			w.emit(key, "%s", w.staleLine(ctx, p, path, owner(t, p.PID, owners)))
			w.checkInstalledGate(ctx, path)
		}
	}
	w.clearMissing(staleKey, found)
}

// staleLine is a stale watch's line: which watch, the version it runs, the
// one installed at its path.
func (w *watcher) staleLine(ctx context.Context, p *proc.Process, path, session string) string {
	who, running := fmt.Sprintf("%s (pid %d", display(p.Args), p.PID), project.Version()
	if p.PID == os.Getpid() {
		who = "this watch (pid " + fmt.Sprint(p.PID)
	} else {
		running = w.binaryVersion(ctx, fmt.Sprintf("/proc/%d/exe", p.PID))
	}
	if session != "" {
		who += ", session " + session
	}
	return fmt.Sprintf("WATCH STALE %s): runs %s; self-update installed %s at %s, a re-arm picks it up: %s",
		who, running, w.binaryVersion(ctx, path), path, rearm(p.Args))
}

// replacedBinary is the path of process pid's executable and whether
// another file was renamed over it since the process started.
func (w *watcher) replacedBinary(pid int) (string, bool) {
	if w.replaced != nil {
		return w.replaced(pid)
	}
	b := platform.ProcessBinary(pid)
	if b == nil {
		return "", false
	}
	return b.Path, b.Replaced()
}

// binaryVersion is the version the beekeeper binary file reports, "an
// unknown version" when it does not run.
func (w *watcher) binaryVersion(ctx context.Context, file string) string {
	if w.versionOf != nil {
		return w.versionOf(ctx, file)
	}
	return binaryVersion(ctx, file)
}

// checkInstalledGate confirms the binary installed at file passes budget --gate
// with the live configuration, once per path, and reports at once when it fails.
func (w *watcher) checkInstalledGate(ctx context.Context, file string) {
	if file == "" {
		return
	}
	w.mu.Lock()
	if w.gateChecked == nil {
		w.gateChecked = map[string]bool{}
	}
	if w.gateChecked[file] {
		w.mu.Unlock()
		return
	}
	w.gateChecked[file] = true
	w.mu.Unlock()

	var err error
	if w.gateOf != nil {
		err = w.gateOf(ctx, file)
	} else if _, statErr := os.Stat(file); statErr == nil {
		gctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		args := budgetGateArgs
		if w.a.cfgPath != "" {
			args = append([]string{"--" + configFlag, w.a.cfgPath}, args...)
		}
		out, runErr := proc.Command(gctx, file, args...).CombinedOutput()
		if runErr != nil {
			detail := oneLine(string(out))
			if detail == "" {
				detail = oneLine(runErr.Error())
			}
			err = fmt.Errorf("%s", detail)
		}
	}
	if err != nil && ctx.Err() == nil {
		w.fail("installed-gate "+file, "WATCH GATE FAILED %s: budget --gate failed after install: %s", file, oneLine(err.Error()))
	}
}
