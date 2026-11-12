//go:build scenario && linux

package scenario

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/update"
)

// install writes given.install's installation: the candidate at the
// target, both builds in the recovery directory under their SHA-256 and
// the journal of a watched, pending probation. Every file is a copy: the
// recovery controller replaces the target's bytes, never a release build's.
func (d *driver) install(ctx context.Context) error {
	in := d.s.Given.Install
	if in.Candidate == nil {
		return nil
	}
	recovery := installTarget + ".update"
	if err := os.MkdirAll(recovery, 0o700); err != nil {
		return err
	}
	binaries := map[string]update.Binary{}
	for _, b := range in.builds() {
		raw, err := os.ReadFile(releaseBinary(b.Name()))
		if err != nil {
			return fmt.Errorf("given.install: the build %s: %w", b.Name(), err)
		}
		digest, err := update.Digest(releaseBinary(b.Name()))
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(recovery, digest), raw, 0o700); err != nil { //nolint:gosec // an executable of the installation
			return err
		}
		binaries[b.Name()] = update.Binary{Version: b.Version, Digest: digest}
	}
	candidate := in.Candidate.build()
	raw, err := os.ReadFile(releaseBinary(candidate.Name()))
	if err != nil {
		return err
	}
	if err := os.WriteFile(installTarget, raw, 0o700); err != nil { //nolint:gosec // the installed executable
		return err
	}
	previous := binaries[in.Previous]
	j := update.Journal{
		Schema: 1, Displaced: previous, Previous: &previous, Candidate: binaries[candidate.Name()],
		InstalledAt: d.now().UTC(), Probation: "pending", Watched: true,
	}
	journal, err := json.Marshal(j)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(recovery, "journal.json"), append(journal, '\n'), 0o600); err != nil {
		return err
	}
	if in.RolledBack == "" {
		return nil
	}
	store, err := state.Open(stateDir)
	if err != nil {
		return err
	}
	if err := update.Rollback(ctx, installTarget, in.RolledBack, store.RecoverVersion); err != nil {
		return fmt.Errorf("given.install: rolling back: %w", err)
	}
	return nil
}

// now is the world's time, the system's without a clock.
func (d *driver) now() time.Time {
	if d.clock != nil {
		return d.clock.Now()
	}
	return time.Now()
}

// checkInstall checks an install expectation against the installation.
func (d *driver) checkInstall(e *InstallExpect) error {
	j, err := update.Inspect(installTarget)
	if err != nil {
		return fmt.Errorf("install: reading the journal: %w", err)
	}
	if j == nil {
		return errors.New("install: no journal")
	}
	digests := map[string]string{}
	for _, b := range d.s.Given.builds() {
		digest, err := update.Digest(releaseBinary(b.Name()))
		if err != nil {
			return err
		}
		digests[b.Name()] = digest
	}
	var errs []error
	if e.Installed != "" {
		got, err := update.Digest(installTarget)
		if err != nil {
			return err
		}
		if got != digests[e.Installed] {
			name := "none of the scenario's builds"
			for n, digest := range digests {
				if digest == got {
					name = n
				}
			}
			errs = append(errs, fmt.Errorf("install: the target holds %s, want %s", name, e.Installed))
		}
	}
	if e.Probation != "" && j.Probation != e.Probation {
		errs = append(errs, fmt.Errorf("install: probation %s, want %s", j.Probation, e.Probation))
	}
	if e.Quarantined != "" && j.Quarantine[digests[e.Quarantined]] == "" {
		errs = append(errs, fmt.Errorf("install: %s is not quarantined (quarantine %v)", e.Quarantined, j.Quarantine))
	}
	observed := j.Observed
	if observed == nil {
		observed = &update.Observed{}
	}
	if r := e.ProbeFailures; r != nil && !r.holds(observed.ProbeFailures) {
		errs = append(errs, fmt.Errorf("install: %d failed probe(s), want %s", observed.ProbeFailures, r))
	}
	if e.Notified != nil && observed.Notified != *e.Notified {
		errs = append(errs, fmt.Errorf("install: notified %t, want %t", observed.Notified, *e.Notified))
	}
	if e.JudgeRecovered != nil && (j.JudgeRecovery != "") != *e.JudgeRecovered {
		errs = append(errs, fmt.Errorf("install: judge recovery %q, want recovered %t", j.JudgeRecovery, *e.JudgeRecovered))
	}
	return errors.Join(errs...)
}
