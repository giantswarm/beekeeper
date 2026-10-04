package upgrade

import "time"

// ReadingsFile is the state store's side file of the last readings, shared
// by every watch, snapshot and ui of the machine: one reads an installation,
// the others use its reading while it is fresh.
const ReadingsFile = "upgrades.json"

// Reading is an installation's last status and when its reading began.
type Reading struct {
	At     time.Time `json:"at"`
	Status Status    `json:"status"`
}

// Readings are the last readings by installation.
type Readings map[string]Reading

// Merge keeps the newer reading of each installation.
func (rs Readings) Merge(o Readings) {
	for name, r := range o {
		if r.At.After(rs[name].At) {
			rs[name] = r
		}
	}
}

// Fresh says whether the installation's last reading began less than within
// before now.
func (rs Readings) Fresh(installation string, now time.Time, within time.Duration) bool {
	r, ok := rs[installation]
	return ok && now.Sub(r.At) < within
}

// Running says whether the installation's last reading found an upgrade.
func (rs Readings) Running(installation string) bool {
	return len(rs[installation].Status.Upgrades) > 0
}

// Keep records the statuses read at at.
func (rs Readings) Keep(statuses []Status, at time.Time) {
	for _, s := range statuses {
		rs[s.Installation] = Reading{At: at, Status: s}
	}
}
