package watch

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

// StaleWatches says each running beekeeper watch, this one included, whose
// binary self-update replaced: it still runs the old code, which only a
// re-arm (a restart) leaves. One WATCH STALE line per watch names the
// version it runs and the one installed, and one ENDED line follows once it
// exits. The watch never re-executes itself: its owner re-arms it.
func (w *Watcher) StaleWatches(ctx context.Context, t *proc.Table) {
	var owners map[int]string
	if o := w.Owners.Load(); o != nil {
		owners = *o
	}
	found := map[string]bool{}
	for _, p := range t.ByPID {
		if !Cmd.IsWatch(p) {
			continue
		}
		path, ok := w.replacedBinary(p.PID)
		if !ok {
			continue
		}
		key := fmt.Sprintf("%s%d", staleKey, p.PID)
		found[key] = true
		if !w.IsActive(key) {
			w.Emit(key, "%s", w.staleLine(ctx, p, path, Cmd.Owner(t, p.PID, owners)))
			w.checkInstalledGate(ctx, path)
		}
	}
	w.ClearMissing(staleKey, found)
}

// staleLine is a stale watch's line: which watch, the version it runs, the
// one installed at its path.
func (w *Watcher) staleLine(ctx context.Context, p *proc.Process, path, session string) string {
	who, running := fmt.Sprintf("%s (pid %d", Cmd.Display(p.Args), p.PID), project.Version()
	if p.PID == os.Getpid() {
		who = "this watch (pid " + fmt.Sprint(p.PID)
	} else {
		running = w.binaryVersion(ctx, fmt.Sprintf("/proc/%d/exe", p.PID))
	}
	if session != "" {
		who += ", session " + session
	}
	return fmt.Sprintf("WATCH STALE %s): runs %s; self-update installed %s at %s, a re-arm picks it up: %s",
		who, running, w.binaryVersion(ctx, path), path, Cmd.Rearm(p.Args))
}

// replacedBinary is the path of process pid's executable and whether
// another file was renamed over it since the process started.
func (w *Watcher) replacedBinary(pid int) (string, bool) {
	if w.Replaced != nil {
		return w.Replaced(pid)
	}
	b := platform.ProcessBinary(pid)
	if b == nil {
		return "", false
	}
	return b.Path, b.Replaced()
}

// binaryVersion is the version the beekeeper binary file reports, "an
// unknown version" when it does not run.
func (w *Watcher) binaryVersion(ctx context.Context, file string) string {
	if w.VersionOf != nil {
		return w.VersionOf(ctx, file)
	}
	return Cmd.BinaryVersion(ctx, file)
}

// checkInstalledGate confirms the binary installed at file passes budget --gate
// with the live configuration, once per path, and reports at once when it fails.
func (w *Watcher) checkInstalledGate(ctx context.Context, file string) {
	if file == "" {
		return
	}
	w.Mu.Lock()
	if w.gateChecked == nil {
		w.gateChecked = map[string]bool{}
	}
	if w.gateChecked[file] {
		w.Mu.Unlock()
		return
	}
	w.gateChecked[file] = true
	w.Mu.Unlock()

	var err error
	if w.GateOf != nil {
		err = w.GateOf(ctx, file)
	} else if _, statErr := os.Stat(file); statErr == nil {
		gctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		args := Cmd.BudgetGateArgs()
		if w.Host.CfgPath() != "" {
			args = append([]string{"--" + configFlag, w.Host.CfgPath()}, args...)
		}
		out, runErr := proc.Command(gctx, file, args...).CombinedOutput()
		if runErr != nil {
			detail := Cmd.OneLine(string(out))
			if detail == "" {
				detail = Cmd.OneLine(runErr.Error())
			}
			err = fmt.Errorf("%s", detail)
		}
	}
	if err != nil && ctx.Err() == nil {
		w.Fail("installed-gate "+file, "WATCH GATE FAILED %s: budget --gate failed after install: %s", file, Cmd.OneLine(err.Error()))
	}
}
