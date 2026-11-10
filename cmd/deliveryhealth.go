package cmd

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/giantswarm/beekeeper/internal/state"
)

// Evaluation and filing share the state transaction. Neither a restart nor
// simultaneous standby and supervisor watches can file an episode twice.
func (a *app) checkDeliveries(now time.Time) (string, error) {
	if a.store == nil {
		return "", fmt.Errorf("durable delivery health unavailable")
	}
	st, err := a.store.Read()
	if err != nil {
		return "", err
	}
	var lines []string
	step := func(st *state.State) ([]state.Event, bool) {
		lines = nil
		after := a.cfg.Delivery.StallAfter.Duration
		if after <= 0 {
			after = 24 * time.Hour
		}
		old := st.DeliveryHealth
		// Watches capture their tick before taking the lock. A slower older tick
		// must not recover an episode a newer tick has just raised.
		if now.Before(old.At) {
			return nil, false
		}
		next := state.DeliveryHealth{At: now, Episodes: map[string]string{}}
		var events []state.Event
		var stalls []string
		file := func(key, detail string) {
			lines = append(lines, detail)
			events = append(events, event(loopParty, loopFileVerb, "[delivery %s] %s reported at %s%s%s", key, loopParty.Name, now.UTC().Format(time.RFC3339), fileReportSeparator, detail))
		}
		var active []state.Delivery
		var signature []string
		latest := time.Time{}
		for _, d := range st.Deliveries {
			signature = append(signature, fmt.Sprintf("%s %s %s %s", d.Ref, d.Step, d.Since.UTC().Format(time.RFC3339Nano), d.Started.UTC().Format(time.RFC3339Nano)))
			if d.Since.After(latest) {
				latest = d.Since
			}
			if !d.Active() || d.Origin == state.DeliveryOriginObserved && !deliveryOwned(st, d, now, a.deliveryPolicy()) {
				continue
			}
			flow := classifyDelivery(st, d, now, a.deliveryPolicy())
			if deliveryOwned(st, d, now, a.deliveryPolicy()) && flow.Class != deliveryFlowing {
				if flow.MissingNote {
					key := "missing-note " + d.Ref
					detail := "DELIVERY WAITING WITHOUT A NOTE " + deliveryURL(d.Ref, false) + ": file a decision with --for and --ref so the guide can serve it"
					next.Episodes[key] = detail
					if _, ok := old.Episodes[key]; !ok {
						file(key, detail)
					}
				}
				continue
			}
			if deliveryInFlight(st, d, now, a.deliveryPolicy()) {
				active = append(active, d)
			}
			failed := d.Step == state.DeliveryReleasing && (d.Verdict == state.DeliveryReleaseFailed || d.Verdict == verdictUnconfirmed)
			// Transitional steps also need time to finish; only explicit failures
			// are actionable immediately. Never diagnose a seconds-old step.
			stepAfter := max(after, 5*time.Minute)
			if d.Step == state.DeliveryReleasing {
				stepAfter = max(after, 30*time.Minute)
			}
			if !failed && (d.Since.IsZero() || now.Sub(d.Since) < stepAfter) {
				continue
			}
			key := fmt.Sprintf("%s %s %s %s", d.Ref, d.Step, d.Since.UTC().Format(time.RFC3339Nano), d.Started.UTC().Format(time.RFC3339Nano))
			wait := strings.TrimSpace(d.Waits + " " + d.Verdict + " " + d.Release)
			if wait == "" {
				wait = "no worker or recorded dependency; supervisor reconciliation needed"
				if deliveryOwned(st, d, now, a.deliveryPolicy()) {
					wait = "worker progress; no dependency recorded"
				}
			}
			detail := fmt.Sprintf("DELIVERY STALLED %s: %s for %s; waits: %s", deliveryURL(d.Ref, slices.Contains(d.Pulls, d.Ref)), d.Step, max(0, now.Sub(d.Since)).Round(time.Second), wait)
			if prev, ok := old.Episodes[key]; ok && !strings.HasSuffix(strings.TrimSpace(prev), "waits:") {
				next.Episodes[key] = prev
			} else {
				next.Episodes[key] = detail
				if !ok {
					stalls = append(stalls, detail)
				}
			}
		}
		if len(stalls) == 1 {
			file("stalls", stalls[0])
		} else if len(stalls) > 1 {
			file("stalls", fmt.Sprintf("DELIVERY STALLED: %d deliveries; waits: inspect delivery records for worker progress or missing dependencies; examples: %s", len(stalls), strings.Join(stalls[:min(3, len(stalls))], "; ")))
		}
		var recovered []string
		for key, detail := range old.Episodes {
			if _, ok := next.Episodes[key]; !ok {
				recovered = append(recovered, detail)
			}
		}
		if len(recovered) == 1 {
			file("recovered", "RECOVERED: "+recovered[0])
		} else if len(recovered) > 1 {
			file("recovered", fmt.Sprintf("RECOVERED: %d delivery episodes ended; inspect delivery records for current status", len(recovered)))
		}
		limit := a.cfg.Delivery.WIPLimit
		if limit > 0 && len(active) >= limit {
			slices.Sort(signature)
			next.FullSignature = fmt.Sprintf("%d: %s", limit, strings.Join(signature, ";"))
			next.FullSince = latest
			if next.FullSince.IsZero() || next.FullSince.After(now) {
				next.FullSince = now
			}
			if next.FullSignature == old.FullSignature {
				next.FullSince, next.FullRaised = old.FullSince, old.FullRaised
			} else if old.FullSignature != "" {
				next.FullSince = now
			}
			if now.Sub(next.FullSince) >= after && !next.FullRaised {
				slices.SortFunc(active, func(a, b state.Delivery) int { return a.Since.Compare(b.Since) })
				var refs []string
				for _, d := range active[:min(5, len(active))] {
					refs = append(refs, fmt.Sprintf("%s (%s, %s; %s %s)", deliveryURL(d.Ref, slices.Contains(d.Pulls, d.Ref)), d.Step, now.Sub(d.Since).Round(time.Second), d.Waits, d.Verdict))
				}
				file("full "+next.FullSignature, fmt.Sprintf("WIP FULL: %d of %d in delivery with no step change for %s; oldest: %s", len(active), limit, now.Sub(next.FullSince).Round(time.Second), strings.Join(refs, ", ")))
				next.FullRaised = true
			}
		}
		if old.FullRaised && (next.FullSignature != old.FullSignature) {
			file("full recovered "+old.FullSignature, "RECOVERED: WIP FULL episode ended: delivery step or limit changed")
		}
		changed := len(events) > 0 || !maps.Equal(old.Episodes, next.Episodes) ||
			old.FullSignature != next.FullSignature || !old.FullSince.Equal(next.FullSince) || old.FullRaised != next.FullRaised
		st.DeliveryHealth = next
		return events, changed
	}
	if _, changed := step(st); !changed {
		return "", nil
	}
	err = a.store.Update(func(st *state.State) ([]state.Event, error) {
		events, _ := step(st)
		return events, nil
	})
	return strings.Join(lines, "\n"), err
}
