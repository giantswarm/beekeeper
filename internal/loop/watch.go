package loop

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/giantswarm/beekeeper/internal/capacity"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/superloop"
)

const LoopDiskKind = "disk"

const WatchDiskStateVerb = "watch.disk-state"

const loopDiskPruneFailed = "prune failed: "

// Retain only events this projection consumes. Broad watch.* and file-event
// prefixes otherwise copy large, unrelated report bodies into the checkpoint.
func watchConditionEvent(e state.Event) bool {
	switch e.Verb {
	case LoopClaimVerb:
		_, target, ok := Keyed(e.Detail)
		return ok && target == "disk prune"
	case LoopFileVerb:
		key, _, ok := Keyed(e.Detail)
		return ok && strings.HasPrefix(key, LoopDiskKind+" ")
	case "supervisor.disk", "disk.prune", WatchDiskStateVerb, "watch.budget-state", "watch.machine-state", "watch.clear",
		"watch.disk", "watch.diskfull", "watch.load", "watch.cpupsi", "watch.avail", "watch.psi", "watch.swap", "watch.oomd":
		return true
	}
	return false
}

// Watch conditions are lasting levels, reconstructed after a restart.
// Budget readings expire rather than treating a dead watch as permission
// to start workers. Thresholds and all source reads stay in watch.
func (l *Loop) WatchConditions(ctx context.Context) error {
	if line, err := l.Host.CheckDeliveries(l.Host.TimeNow()); err != nil {
		return err
	} else if line != "" {
		_, _ = fmt.Fprintln(l.Host.Out(), line)
	}
	if line, err := l.Host.CheckThroughput(l.Host.TimeNow(), false, func() { l.LifecyclePhase("read throughput history") }); err != nil {
		return err
	} else if line != "" {
		_, _ = fmt.Fprintln(l.Host.Out(), line)
	}
	if l.IndependentFill {
		return nil // the refill stream owns admission while observations may block
	}
	_, err := l.refreshStartConditions(ctx)
	return err
}

// refreshStartConditions belongs to the refill stream in continuous mode and
// the main pass in once mode. It must not refresh the observation heartbeat.
// Its reason lets every held refill explain why it cannot start a worker,
// even when the watch's transition-only diagnostic has already been printed.
func (l *Loop) refreshStartConditions(ctx context.Context) (string, error) {
	evs, err := state.FollowSnapshot(l.Host.Store(), "supervisor-watch-conditions", "v2", watchConditionEvent)
	if err != nil {
		return "", err
	}
	active := map[string]string{}
	var budgetAt, machineAt time.Time
	budgetHeld := true
	var disk capacity.WatchDiskReading
	var diskKey string
	var diskAt time.Time
	pruned := map[string]time.Time{}
	pruneFailures := map[string]string{}
	filed := map[string]bool{}
	claims := map[string]time.Time{}
	var lastPrune time.Time
	for _, e := range evs {
		switch e.Verb {
		case LoopClaimVerb:
			if key, target, ok := Keyed(e.Detail); ok && target == "disk prune" {
				claims[key] = e.At
			}
		case "disk.prune":
			lastPrune = e.At
		case WatchDiskStateVerb:
			if err := json.Unmarshal([]byte(e.Detail), &disk); err != nil {
				return "", err
			}
			diskAt = e.At
			if !disk.Low {
				diskKey = ""
			} else if diskKey == "" {
				diskKey = "disk " + e.At.UTC().Format(time.RFC3339Nano)
			}
		case "supervisor.disk":
			key, what, ok := Keyed(e.Detail)
			if ok && (what == "pruned" || strings.HasPrefix(what, loopDiskPruneFailed)) {
				pruned[key] = e.At
				if reason, failed := strings.CutPrefix(what, loopDiskPruneFailed); failed {
					pruneFailures[key] = reason
				}
			}
			// "noted" is the verb of the person notes an older loop filed
			// for an episode; it counts as filed, so an upgrade does not
			// file the same episode's finding again.
			if ok && what == "noted" {
				filed[key] = true
			}
		case LoopFileVerb:
			// The finding this loop files for an episode whose prune freed
			// nothing arrives here (KindFile); its key names the episode.
			if key, _, ok := Keyed(e.Detail); ok && strings.HasPrefix(key, LoopDiskKind+" ") {
				filed[key] = true
			}
		case "watch.budget-state":
			budgetAt, budgetHeld = e.At, e.Detail != "false"
		case "watch.machine-state":
			machineAt = e.At
			active = map[string]string{}
			if err := json.Unmarshal([]byte(e.Detail), &active); err != nil {
				return "", err
			}
		case "watch.clear":
			machineAt = e.At
			delete(active, e.Detail)
		// watch.diskfill is absent: a fill forecast never holds starts,
		// not even one an older watch said (see publishMachine).
		case "watch.disk", "watch.diskfull", "watch.load", "watch.cpupsi", "watch.avail", "watch.psi", "watch.swap", "watch.oomd":
			machineAt = e.At
			active[e.Verb[6:]] = e.Detail
		}
	}
	// Prefer fresh levels without replaying identical samples into events.jsonl.
	var freshBudget bool
	if at, err := l.Host.ReadWatchLevel("budget-state", &freshBudget); err != nil {
		return "", err
	} else if at.After(budgetAt) {
		budgetAt, budgetHeld = at, freshBudget
	}
	var freshMachine map[string]string
	if at, err := l.Host.ReadWatchLevel("machine-state", &freshMachine); err != nil {
		return "", err
	} else if at.After(machineAt) {
		active = freshMachine
	}
	var freshDisk capacity.WatchDiskReading
	if at, err := l.Host.ReadWatchLevel("disk-state", &freshDisk); err != nil {
		return "", err
	} else if at.After(diskAt) {
		diskAt, disk = at, freshDisk
	}
	// Maintenance must never take the loop (and its watch) down. Establish
	// the hold before attempting any disk action, including failed writes.
	if disk.Low {
		l.HoldStarts(true)
		active[LoopDiskKind] = disk.Writers
		act := superloop.Action{Kind: LoopDiskKind, Key: diskKey}
		if at, ok := pruned[diskKey]; !ok {
			outcome := "pruned"
			claim, claimed := claims[diskKey]
			if !claimed || lastPrune.Before(claim) {
				if err := l.claim(act, "disk prune"); err != nil {
					return "", err
				}
				if _, err := l.Run(ctx, LoopDiskKind, "prune", "--apply"); err != nil {
					l.SayOnce(diskKey+" prune", "SUPERVISOR DISK prune failed; starts remain held: %v", err)
					outcome = loopDiskPruneFailed + Cmd.OneLine(err.Error())
				}
			}
			if err := l.Record(act, outcome); err != nil {
				return "", err
			}
		} else if diskAt.After(at) && !filed[diskKey] {
			// The prune freed nothing the desk can reclaim: what remains
			// is a finding, never a decision for the person — disk is not
			// theirs to decide. The finding goes through the loop's file
			// channel to the guide, which files it in its owning
			// repository. The hold stays while free space stays under the
			// floor; it lifts on its own once the watch reports recovery.
			text := fmt.Sprintf("P1: Disk remains low after pruning; every worker start is held: free disk / %d MiB (floor %d MiB). Writers: %s", disk.FreeMiB, disk.MinMiB, disk.Writers)
			if reason := pruneFailures[diskKey]; reason != "" {
				text += ". Cleanup failed: " + reason
			}
			var holding DiskHolding
			if found, _ := l.Host.Store().ReadFile(diskHoldingFile, &holding); found && holding.Text != "" && l.Host.TimeNow().Sub(holding.At) < time.Hour {
				text += ". Holding the space: " + holding.Text
			}
			text += ". `beekeeper disk usage` lists what holds the space by owner; the writers above are process counters, not what stayed on disk. Own the reclaim gap to the beekeeper disk step, the writer's free-space need to its repository."
			fileAct := superloop.Action{Kind: superloop.KindFile, Key: diskKey}
			line := fmt.Sprintf("%s reported at %s%s%s", LoopParty.Name, l.Host.TimeNow().UTC().Format(time.RFC3339), FileReportSeparator, text)
			if err := l.Record(fileAct, line); err != nil {
				l.SayOnce(diskKey+" file", "SUPERVISOR DISK finding failed; starts remain held: %v", err)
			}
		}
	}
	// A live watch's tick can wait minutes on the state lock: only a reading
	// older than that says the watch is gone.
	stale := budgetAt.IsZero() || l.Host.TimeNow().Sub(budgetAt) > max(3*l.Host.Cfg().Watch.Interval.Duration, loopBudgetStale)
	l.HoldStarts(budgetHeld || stale || len(active) > 0)
	if l.HoldingStarts() {
		reason := fmt.Sprintf("watch budget held=%t stale=%t; machine conditions %v", budgetHeld, stale, active)
		l.SayOnce("watch-hold", "SUPERVISOR STARTS HELD: %s", reason)
		return reason, nil
	}
	l.ClearSaid("watch-hold")
	return "", nil
}

// loopBudgetStale is the least age at which the watch's budget reading
// holds starts as stale: the watch's tick waits up to 2m on the state lock
// under load, and a 1m window held every start of a live watch for an hour
// (https://github.com/giantswarm/beekeeper/issues/3388).
const loopBudgetStale = 5 * time.Minute
