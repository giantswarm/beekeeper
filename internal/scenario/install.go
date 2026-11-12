//go:build scenario

package scenario

import (
	"fmt"
	"slices"

	"github.com/Masterminds/semver/v3"
)

// installTarget is the installed executable of given.install inside the
// sandbox; its recovery directory is beside it (installTarget + ".update").
const installTarget = "/work/install/beekeeper"

// Install is an installation in probation, as an update leaves it: the
// candidate build at installTarget, the release it displaced and the
// candidate retained in the recovery directory under their SHA-256, and a
// journal of a watched probation that is pending. A recovery controller of
// the previous release then judges it (self-update --recovery-controller
// --recovery-target /work/install/beekeeper).
type Install struct {
	// Candidate is the build in probation.
	Candidate *InstallBuild `yaml:"candidate"`
	// Previous is the release the candidate displaced, the last known good.
	Previous string `yaml:"previous"`
	// RolledBack, when set, rolls the installation back before the
	// scenario starts, for that reason, as a controller would have: the
	// previous release installed again, the candidate quarantined.
	RolledBack string `yaml:"rolled_back"`
}

// InstallBuild is a build of the candidate: a version stamp and, for a
// broken one, a mutation of Mutations.
type InstallBuild struct {
	Version  string `yaml:"version"`
	Mutation string `yaml:"mutation"`
}

// build is b as a release build.
func (b InstallBuild) build() ReleaseBuild { return ReleaseBuild(b) }

// builds are the builds the installation needs.
func (in Install) builds() []ReleaseBuild {
	if in.Candidate == nil {
		return nil
	}
	return []ReleaseBuild{in.Candidate.build(), {Version: in.Previous}}
}

// validate is what the loader refuses of given.install.
func (in Install) validate() []string {
	if in.Candidate == nil {
		if in.Previous != "" || in.RolledBack != "" {
			return []string{"previous and rolled_back without a candidate"}
		}
		return nil
	}
	var out []string
	for _, v := range []string{in.Candidate.Version, in.Previous} {
		if _, err := semver.StrictNewVersion(v[min(1, len(v)):]); err != nil || v == "" || v[0] != 'v' {
			out = append(out, fmt.Sprintf("%q is no release version (v<major>.<minor>.<patch>[-<pre-release>])", v))
		}
	}
	if m := in.Candidate.Mutation; m != "" && !slices.Contains(MutationNames(), m) {
		out = append(out, fmt.Sprintf("candidate: mutation %q is none of %v", m, MutationNames()))
	}
	if in.Candidate.build() == (ReleaseBuild{Version: in.Previous}) {
		out = append(out, "the candidate is the previous release itself")
	}
	return out
}

// InstallExpect checks the installation of given.install: Installed is
// the build (ReleaseBuild.Name) whose bytes the target holds; Probation
// the journal's probation (pending, passed, rolled-back); Quarantined a
// build the journal quarantines; ProbeFailures how many failed probes the
// controller counted; Notified whether the person's line about a rollback
// is out; JudgeRecovered whether an outdated judge's rollback was
// recovered (--recover-outdated-judge).
type InstallExpect struct {
	Installed      string `yaml:"installed"`
	Probation      string `yaml:"probation"`
	Quarantined    string `yaml:"quarantined"`
	ProbeFailures  *Range `yaml:"probe_failures"`
	Notified       *bool  `yaml:"notified"`
	JudgeRecovered *bool  `yaml:"judge_recovered"`
}

// Range is a count, exactly (Equals) or between Min and Max.
type Range struct {
	Min *int `yaml:"min"`
	Max *int `yaml:"max"`
}

// holds says whether n is in the range.
func (r Range) holds(n int) bool {
	return (r.Min == nil || n >= *r.Min) && (r.Max == nil || n <= *r.Max)
}

// String is the range as an expectation names it.
func (r *Range) String() string {
	switch {
	case r.Min != nil && r.Max != nil:
		return fmt.Sprintf("%d to %d", *r.Min, *r.Max)
	case r.Min != nil:
		return fmt.Sprintf("at least %d", *r.Min)
	case r.Max != nil:
		return fmt.Sprintf("at most %d", *r.Max)
	}
	return "any"
}

// validate is what the loader refuses of an install expectation.
func (e InstallExpect) validate(s *Scenario) []string {
	var out []string
	if s.Given.Install.Candidate == nil {
		out = append(out, "install: the scenario has no given.install")
	}
	if e == (InstallExpect{}) {
		out = append(out, "install: no check")
	}
	for _, name := range []string{e.Installed, e.Quarantined} {
		if name != "" && !s.Given.knownBuild(name) {
			out = append(out, fmt.Sprintf("install: %q is none of the scenario's builds", name))
		}
	}
	if p := e.Probation; p != "" && !slices.Contains([]string{"prepared", "pending", "passed", "rolling-back", "rolled-back"}, p) {
		out = append(out, fmt.Sprintf("install: probation %q is none of prepared, pending, passed, rolling-back, rolled-back", p))
	}
	if r := e.ProbeFailures; r != nil && r.Min == nil && r.Max == nil {
		out = append(out, "install: probe_failures without min or max")
	}
	return out
}
