package cmd

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/alerts"
	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/state"
)

const (
	otherTeam = "other-team"
	ownedPage = "alpha/PodRestarting"
)

// pageApp is stubApp with an alert baseline of one page and one notify
// alert on the installation alpha, both firing since start.
func pageApp(t *testing.T, start time.Time) (*app, *bytes.Buffer) {
	t.Helper()
	a, out := stubApp(t)
	since := start.UTC().Format(time.RFC3339Nano)
	base := &alerts.State{Installations: map[string]*alerts.Installation{"alpha": {Reachable: true, Alerts: alerts.Set{
		"fp1": {Severity: alerts.Page, Team: "t", Alertname: "PodRestarting", Where: "ns/app", Since: since},
		"fp2": {Severity: "notify", Team: "t", Alertname: "DiskFilling", Where: "ns/vol", Since: since},
		"fp3": {Severity: alerts.Page, Team: otherTeam, Alertname: "TheirPage", Where: "ns/x", Since: since},
	}}}}
	a.cfg.Alerts.Team = "t"
	if err := alerts.NewStore(a.cfg.StateDir).Save(base); err != nil {
		t.Fatal(err)
	}
	return a, out
}

// pollPages runs the watch's page check at start plus each minute offset
// and returns the PAGE UNOWNED lines.
func pollPages(w *watcher, out *bytes.Buffer, start time.Time, sessions []*claude.Session, minutes ...int) []string {
	out.Reset()
	for _, m := range minutes {
		w.now = start.Add(time.Duration(m) * time.Minute)
		w.unownedPages(context.Background(), sessions)
	}
	var lines []string
	for l := range strings.Lines(out.String()) {
		if strings.Contains(l, "PAGE UNOWNED") {
			lines = append(lines, strings.TrimSpace(l))
		}
	}
	return lines
}

// A page firing for 15 minutes without an owner is one line, then one per
// further 15 minutes; the notify alert and another team's page never are.
func TestWatchSaysUnownedPageOncePerGrace(t *testing.T) {
	start := time.Now().Add(-3 * time.Hour)
	a, out := pageApp(t, start)
	w := a.newWatcher(false, true)
	if got := pollPages(w, out, start, nil, 0, 5, 14); len(got) != 0 {
		t.Fatalf("before the grace: %q", got)
	}
	got := pollPages(w, out, start, nil, 15, 16, 20, 29)
	if all := out.String(); strings.Contains(all, "DiskFilling") || strings.Contains(all, "TheirPage") {
		t.Fatalf("a non-paging alert or another team's page:\n%s", all)
	}
	if len(got) != 1 || !strings.HasSuffix(got[0], "PAGE UNOWNED alpha PodRestarting ns/app for 15m: beekeeper alerts own alpha/PodRestarting") {
		t.Fatalf("from 15m to 29m: %q", got)
	}
	if got := pollPages(w, out, start, nil, 30, 31, 44, 45); len(got) != 2 || !strings.Contains(got[0], " for 30m: ") || !strings.Contains(got[1], " for 45m: ") {
		t.Fatalf("from 30m to 45m: %q", got)
	}
	if got := pollPages(w, out, start, nil, 46); len(got) != 0 {
		t.Fatalf("a non-paging alert or a repeat within the period:\n%s", out.String())
	}
}

// alerts own stops the lines; the owner's session ending restarts the count.
func TestAlertsOwnStopsTheLinesUntilTheOwnerEnds(t *testing.T) {
	start := time.Now().Add(-2 * time.Hour)
	a, out := pageApp(t, start)
	owner := &claude.Session{ID: "s2", Name: ownerName}
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.AlertOwners = []state.AlertOwner{{Alert: "fp1@" + start.UTC().Format(time.RFC3339Nano), Name: ownedPage, By: owner.Party(), At: start}}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	w := a.newWatcher(false, true)
	if got := pollPages(w, out, start, []*claude.Session{owner}, 15, 30, 60); len(got) != 0 {
		t.Fatalf("an owned page: %q", got)
	}
	// The owner's session is gone at 60m: the count starts there.
	if got := pollPages(w, out, start, nil, 61, 74); len(got) != 0 {
		t.Fatalf("within the grace after the owner ended: %q", got)
	}
	st, _ := a.store.Read()
	if len(st.AlertOwners) != 1 || st.AlertOwners[0].Ended.IsZero() {
		t.Fatalf("the owner's end is not recorded: %+v", st.AlertOwners)
	}
	if got := pollPages(w, out, start, nil, 76); len(got) != 1 || !strings.Contains(got[0], " for 15m: ") {
		t.Fatalf("15m after the owner ended: %q", got)
	}
}

// watch --once writes nothing: an owner gone with no end recorded counts
// from now, so it says no line.
func TestWatchOnceRecordsNoOwnerEnd(t *testing.T) {
	start := time.Now().Add(-2 * time.Hour)
	a, out := pageApp(t, start)
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.AlertOwners = []state.AlertOwner{{Alert: "fp1@" + start.UTC().Format(time.RFC3339Nano), By: state.Party{Session: holderGone, Name: "w"}, At: start}}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	once := a.newWatcher(false, false)
	if got := pollPages(once, out, start, nil, 90); len(got) != 0 {
		t.Fatalf("--once: %q", got)
	}
	if st, _ := a.store.Read(); !st.AlertOwners[0].Ended.IsZero() {
		t.Fatal("--once recorded the owner's end")
	}
}

// alerts own records the caller as the owner of the matching alerts and
// logs alert.owned; an argument naming none lists the firing pages.
func TestAlertsOwnCommand(t *testing.T) {
	start := time.Now().Add(-20 * time.Minute)
	a, out := pageApp(t, start)
	c := a.alertsOwnCmd()
	c.SetArgs([]string{"alpha/Nope"})
	c.SetOut(out)
	c.SetErr(out)
	if err := c.Execute(); err == nil || !strings.Contains(err.Error(), "firing pages: "+ownedPage) {
		t.Fatalf("own of no alert: %v", err)
	}
	c.SetArgs([]string{ownedPage})
	if err := c.Execute(); err != nil {
		t.Fatal(err)
	}
	st, _ := a.store.Read()
	if len(st.AlertOwners) != 1 || st.AlertOwners[0].By.Name != agentOne || st.AlertOwners[0].Name != ownedPage+"#fp1" {
		t.Fatalf("owners: %+v", st.AlertOwners)
	}
	if got := lastEventOf(t, a, verbAlertOwned); !strings.Contains(got, "alpha/PodRestarting#fp1 ns/app") {
		t.Errorf("alert.owned event: %q", got)
	}
	w := a.newWatcher(false, true)
	if got := pollPages(w, out, start, nil, 15, 60, 180); len(got) != 0 {
		t.Errorf("a person's page is said unowned: %q", got)
	}
}

// twoInstances adds a second instance of the page alpha/PodRestarting to
// pageApp's baseline, firing since start.
func twoInstances(t *testing.T, a *app, start time.Time) {
	t.Helper()
	st := alerts.NewStore(a.cfg.StateDir)
	base, err := st.Load()
	if err != nil {
		t.Fatal(err)
	}
	base.Installations["alpha"].Alerts["fp4"] = alerts.Alert{Severity: alerts.Page, Team: "t", Alertname: "PodRestarting", Where: "ns/other", Since: start.UTC().Format(time.RFC3339Nano)}
	if err := st.Save(base); err != nil {
		t.Fatal(err)
	}
}

// runAlerts runs an alerts subcommand with args and returns its output.
func runAlerts(t *testing.T, c *cobra.Command, out *bytes.Buffer, args ...string) (string, error) {
	t.Helper()
	out.Reset()
	c.SetArgs(args)
	c.SetOut(out)
	c.SetErr(out)
	err := c.Execute()
	return out.String(), err
}

// Owning one instance by its fingerprint leaves the alertname's other
// instances unowned; the bare form takes every instance and names each.
func TestAlertsOwnOneInstance(t *testing.T) {
	start := time.Now().Add(-20 * time.Minute)
	a, out := pageApp(t, start)
	twoInstances(t, a, start)
	got, err := runAlerts(t, a.alertsOwnCmd(), out, ownedPage+"#fp1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, ownedPage+"#fp1") || strings.Contains(got, "fp4") {
		t.Fatalf("own of one instance said:\n%s", got)
	}
	st, _ := a.store.Read()
	if len(st.AlertOwners) != 1 || !strings.HasPrefix(st.AlertOwners[0].Alert, "fp1@") {
		t.Fatalf("owners: %+v", st.AlertOwners)
	}
	w := a.newWatcher(false, true)
	if lines := pollPages(w, out, start, nil, 15); len(lines) != 1 || !strings.Contains(lines[0], " ns/other for ") || strings.Contains(lines[0], "(+") {
		t.Fatalf("the other instance is not the one unowned page: %q", lines)
	}
	got, err = runAlerts(t, a.alertsOwnCmd(), out, ownedPage)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, ownedPage+"#fp1") || !strings.Contains(got, ownedPage+"#fp4") {
		t.Fatalf("the bare form does not name the instances it took:\n%s", got)
	}
	if st, _ := a.store.Read(); len(st.AlertOwners) != 2 {
		t.Fatalf("the bare form took %d instances, want 2: %+v", len(st.AlertOwners), st.AlertOwners)
	}
}

// alerts disown releases an owned instance, logs alert.disowned and leaves
// the alert unowned from then: the watch says it again after the grace.
// Naming an alert no one owns is refused.
func TestAlertsDisown(t *testing.T) {
	start := time.Now().Add(-20 * time.Minute)
	a, out := pageApp(t, start)
	a.now = start.Add(20 * time.Minute)
	twoInstances(t, a, start)
	if _, err := runAlerts(t, a.alertsOwnCmd(), out, ownedPage); err != nil {
		t.Fatal(err)
	}
	got, err := runAlerts(t, a.alertsDisownCmd(), out, "fp4")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "no longer owns "+ownedPage+"#fp4") {
		t.Fatalf("disown said:\n%s", got)
	}
	if got := lastEventOf(t, a, verbAlertDisowned); !strings.Contains(got, ownedPage+"#fp4") {
		t.Errorf("alert.disowned event: %q", got)
	}
	w := a.newWatcher(false, true)
	if lines := pollPages(w, out, start, nil, 21, 34); len(lines) != 0 {
		t.Fatalf("within the grace after the disown: %q", lines)
	}
	if lines := pollPages(w, out, start, nil, 35); len(lines) != 1 || !strings.Contains(lines[0], " ns/other for 15m: ") {
		t.Fatalf("15m after the disown: %q", lines)
	}
	if _, err := runAlerts(t, a.alertsDisownCmd(), out, ownedPage+"#fp4"); err == nil || !strings.Contains(err.Error(), "no owner") {
		t.Fatalf("disown of an unowned alert: %v", err)
	}
	if _, err := runAlerts(t, a.alertsDisownCmd(), out, "alpha/Nope"); err == nil || !strings.Contains(err.Error(), "firing pages: ") {
		t.Fatalf("disown of no alert: %v", err)
	}
}
