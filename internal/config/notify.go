package config

import (
	"fmt"
	"slices"
	"strings"

	"github.com/giantswarm/beekeeper/internal/notify"
)

// Notify configures what `watch --notify` sends to the desktop.
type Notify struct {
	// Kinds are the kinds that notify (notify.Kinds, the default: due,
	// budget, stale-lease, no-supervisor). The machine's lines never
	// notify: the supervisor's watch says them.
	Kinds []string `yaml:"kinds"`
	// QuietHours ("22:00-07:00", local time) hold every notification but a
	// critical one until they end.
	QuietHours string `yaml:"quietHours"`
	// Urgency is a kind's urgency (low, normal, critical); no-supervisor
	// is critical, the others normal.
	Urgency map[string]string `yaml:"urgency"`
	// Repeat is how often a lasting condition (budget, no-supervisor)
	// notifies again while it lasts.
	Repeat Duration `yaml:"repeat"`
}

// Policy is the notify package's view of the section.
func (n Notify) Policy() notify.Policy {
	q, _ := notify.ParseQuietHours(n.QuietHours) // validated on load
	return notify.Policy{Kinds: n.Kinds, Quiet: q, Urgency: n.Urgency, Repeat: n.Repeat.Duration}
}

// machineKinds are the kinds that notified before the machine's lines went
// to the supervisor only: a config naming one is told so.
var machineKinds = []string{"oom-line", "oom-kill"}

func notifyKind(field, k string) error {
	if slices.Contains(machineKinds, k) {
		return fmt.Errorf("%s: %q is a machine line, said to the supervisor's watch and never notified: remove it", field, k)
	}
	if !slices.Contains(notify.Kinds, k) {
		return fmt.Errorf("%s: %q is none of %s", field, k, strings.Join(notify.Kinds, ", "))
	}
	return nil
}

func (n Notify) validateEach(report func(error)) {
	for _, k := range n.Kinds {
		if err := notifyKind("notify.kinds", k); err != nil {
			report(err)
		}
	}
	for k, u := range n.Urgency {
		if err := notifyKind("notify.urgency", k); err != nil {
			report(err)
		}
		if !slices.Contains(notify.Urgencies, u) {
			report(fmt.Errorf("notify.urgency.%s: %q is none of %s", k, u, strings.Join(notify.Urgencies, ", ")))
		}
	}
	if _, err := notify.ParseQuietHours(n.QuietHours); err != nil {
		report(fmt.Errorf("notify.quietHours: %w", err))
	}
}
