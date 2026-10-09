package alerts

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

// fakePagerDuty answers muster's x_pd_* tools as PagerDuty does: open is
// the list_incidents answer, installations the installation label of each
// incident's first alert; down fails every call, flaps the next flaps
// list_incidents calls with muster's auth_required.
type fakePagerDuty struct {
	open          []string
	installations map[string]string
	down          error
	flaps         int
	calls         []string
}

// errAuthRequired is muster's answer while it reconnects the PagerDuty server.
var errAuthRequired = errors.New("muster context central, x_pd_list_incidents: x_pd_list_incidents: auth_required: authentication required")

func (f *fakePagerDuty) call(_ context.Context, tool string, args map[string]any) (string, error) {
	f.calls = append(f.calls, tool)
	if f.down != nil {
		return "", f.down
	}
	if tool == listIncidentsTool && f.flaps > 0 {
		f.flaps--
		return "", errAuthRequired
	}
	switch tool {
	case listIncidentsTool:
		return `{"response": [` + strings.Join(f.open, ",") + `], "response_summary": "ListResponseModel<Incident>"}`, nil
	case "list_alerts_from_incident":
		id, _ := args["incident_id"].(string)
		return fmt.Sprintf(`{"response": [{"body": {"cef_details": {"details": {"installation": %q, "team": %q}}}}]}`, f.installations[id], ourTeam), nil
	}
	return "", fmt.Errorf("unknown tool %s", tool)
}

func incident(id string, number int, status, title, created string) string {
	return fmt.Sprintf(`{"id": %q, "incident_number": %d, "status": %q, "title": %q, "created_at": %q,
		"service": {"id": "PSVC", "summary": "bumblebee-alertmanager"}, "assignments": [], "type": "incident"}`, id, number, status, title, created)
}

var (
	kagentPage = incident("Q1", 9370, "triggered", "zeta - AgentPlatformContainerRestartingTooOften: Container controller in kagent on zeta has restarted 7 times.", "2026-09-24T18:49:20Z")
	fluxPage   = incident("Q2", 9369, "triggered", "eta-wc1 - FluxGiantswarmHelmReleaseFailed: HelmRelease agent-manager is stuck in Failed state. (2 firing)", "2026-09-24T17:10:00Z")
)

// instZ is the installation of the tests' incidents.
const instZ = "zeta"

// quickRetry shortens the pause before another attempt for the test.
func quickRetry(t *testing.T) {
	t.Helper()
	pause := retryPause
	retryPause = 100 * time.Millisecond
	t.Cleanup(func() { retryPause = pause })
}

func readPD(t *testing.T, f *fakePagerDuty, prev *PagerDuty, at time.Time) ([]string, *PagerDuty) {
	t.Helper()
	quickRetry(t)
	var known map[string]Incident
	if prev != nil {
		known = prev.Incidents
	}
	r := PagerDutyReader{Services: []string{"PSVC"}, Call: f.call}
	return rules.PagerDutyStep(prev, r.Read(context.Background(), known), at)
}

func TestPagerDutyNewAndResolved(t *testing.T) {
	f := &fakePagerDuty{open: []string{fluxPage}, installations: map[string]string{"Q1": instZ, "Q2": "eta"}}
	lines, base := readPD(t, f, nil, now)
	want := []string{
		"PAGERDUTY first look: 1 open",
		"PAGERDUTY OPEN eta BUMBLEBEE #9369 eta-wc1 - FluxGiantswarmHelmReleaseFailed: HelmRelease agent-manager is stuck in Failed state. (2 firing) since 17:10Z",
	}
	if !slices.Equal(lines, want) {
		t.Fatalf("first look:\n%s\nwant\n%s", strings.Join(lines, "\n"), strings.Join(want, "\n"))
	}

	f.open, f.calls = []string{fluxPage, kagentPage}, nil
	lines, base = readPD(t, f, base, now.Add(time.Minute))
	want = []string{"PAGERDUTY NEW zeta BUMBLEBEE #9370 zeta - AgentPlatformContainerRestartingTooOften: Container controller in kagent on zeta has restarted 7 times. since 18:49Z"}
	if !slices.Equal(lines, want) {
		t.Fatalf("new page:\n%s\nwant\n%s", strings.Join(lines, "\n"), strings.Join(want, "\n"))
	}
	if n := slices.Index(f.calls, "list_alerts_from_incident"); n < 0 || slices.Index(f.calls[n+1:], "list_alerts_from_incident") >= 0 {
		t.Errorf("calls %v: the installation is read once, for the new incident only", f.calls)
	}

	f.open = []string{strings.Replace(fluxPage, `"triggered"`, `"acknowledged"`, 1), kagentPage}
	lines, base = readPD(t, f, base, now.Add(2*time.Minute))
	if len(lines) != 1 || !strings.HasPrefix(lines[0], "PAGERDUTY ACKNOWLEDGED eta BUMBLEBEE #9369 ") {
		t.Errorf("acknowledged: %q", lines)
	}

	f.open = []string{kagentPage}
	lines, _ = readPD(t, f, base, now.Add(3*time.Minute))
	want = []string{"PAGERDUTY RESOLVED eta BUMBLEBEE #9369 eta-wc1 - FluxGiantswarmHelmReleaseFailed: HelmRelease agent-manager is stuck in Failed state. (2 firing)"}
	if !slices.Equal(lines, want) {
		t.Errorf("resolved:\n%s\nwant\n%s", strings.Join(lines, "\n"), strings.Join(want, "\n"))
	}
}

func TestPagerDutyEmpty(t *testing.T) {
	f := &fakePagerDuty{}
	lines, base := readPD(t, f, nil, now)
	if !slices.Equal(lines, []string{"PAGERDUTY first look: 0 open"}) {
		t.Errorf("first look: %q", lines)
	}
	if lines, _ := readPD(t, f, base, now.Add(time.Minute)); len(lines) != 0 {
		t.Errorf("nothing changed, yet %q", lines)
	}
	snap := rules.PagerDutySnapshot(PagerDutyReader{Services: []string{"PSVC"}, Call: f.call}.Read(context.Background(), nil), now)
	if !slices.Equal(snap, []string{"pagerduty at 18:50Z: 0 open"}) {
		t.Errorf("snapshot: %q", snap)
	}
}

func TestPagerDutyUnreachable(t *testing.T) {
	f := &fakePagerDuty{open: []string{kagentPage}, installations: map[string]string{"Q1": instZ}}
	_, base := readPD(t, f, nil, now)

	f.down = errors.New("muster context gazelle, x_pd_list_incidents: not signed in")
	lines, base := readPD(t, f, base, now.Add(5*time.Minute))
	want := []string{"PAGERDUTY unreachable, incidents unseen for 5m (since 18:50Z): muster context gazelle, x_pd_list_incidents: not signed in (2 attempts)"}
	if !slices.Equal(lines, want) {
		t.Fatalf("unreachable: %q", lines)
	}
	if _, kept := base.Incidents["Q1"]; !kept {
		t.Error("an unreachable PagerDuty dropped the open incidents: they would read as resolved")
	}
	if lines, _ := readPD(t, f, base, now.Add(10*time.Minute)); len(lines) != 0 {
		t.Errorf("said again within %s: %q", UnseenRepeat, lines)
	}
	lines, base = readPD(t, f, base, now.Add(5*time.Minute+UnseenRepeat))
	if len(lines) != 1 || !strings.HasPrefix(lines[0], "PAGERDUTY unreachable, incidents unseen for 20m") {
		t.Errorf("repeat: %q", lines)
	}

	f.down = nil
	lines, _ = readPD(t, f, base, now.Add(21*time.Minute))
	if !slices.Equal(lines, []string{"PAGERDUTY reachable again after 21m unseen"}) {
		t.Errorf("back: %q", lines)
	}

	f.down = errors.New("no muster")
	lines, _ = readPD(t, f, nil, now)
	if !slices.Equal(lines, []string{"PAGERDUTY unreachable, incidents never read: no muster (2 attempts)"}) {
		t.Errorf("never read: %q", lines)
	}
	snap := rules.PagerDutySnapshot(PagerDutyReader{Call: f.call}.Read(context.Background(), nil), now)
	if !slices.Equal(snap, []string{"pagerduty unreachable: no muster (2 attempts)"}) {
		t.Errorf("snapshot: %q", snap)
	}
}

// A single auth_required is read through by the retry: no line, the
// incidents as they are.
func TestPagerDutyAuthRequiredOnce(t *testing.T) {
	f := &fakePagerDuty{open: []string{kagentPage}, installations: map[string]string{"Q1": instZ}}
	_, base := readPD(t, f, nil, now)

	f.flaps, f.calls = 1, nil
	lines, base := readPD(t, f, base, now.Add(time.Minute))
	if len(lines) != 0 || !base.Reachable || !base.Seen.Equal(now.Add(time.Minute)) {
		t.Errorf("one auth_required: lines %q, baseline %+v", lines, base)
	}
	if !slices.Equal(f.calls, []string{listIncidentsTool, listIncidentsTool}) {
		t.Errorf("calls %v: the listing is tried once more", f.calls)
	}
}

// auth_required for a reading or two, then a good one, is never said
// unseen; one that persists past PagerDutyGrace is.
func TestPagerDutyAuthRequiredWithinGrace(t *testing.T) {
	f := &fakePagerDuty{open: []string{kagentPage}, installations: map[string]string{"Q1": instZ}}
	_, base := readPD(t, f, nil, now)

	for _, at := range []time.Duration{time.Minute, 2 * time.Minute} {
		f.flaps = 2
		var lines []string
		lines, base = readPD(t, f, base, now.Add(at))
		if len(lines) != 0 {
			t.Fatalf("auth_required %s after the last reading said: %q", at, lines)
		}
	}
	lines, base := readPD(t, f, base, now.Add(3*time.Minute-time.Second))
	if len(lines) != 0 || !base.Seen.Equal(now.Add(3*time.Minute-time.Second)) {
		t.Errorf("read again within the grace: lines %q, baseline %+v", lines, base)
	}

	f.flaps = 1 << 30
	for _, at := range []time.Duration{4 * time.Minute, 5 * time.Minute} {
		if lines, base = readPD(t, f, base, now.Add(at)); len(lines) != 0 {
			t.Fatalf("auth_required within %s of the last reading said: %q", PagerDutyGrace, lines)
		}
	}
	lines, _ = readPD(t, f, base, now.Add(6*time.Minute))
	want := []string{"PAGERDUTY unreachable, incidents unseen for 3m (since 18:52Z): " + errAuthRequired.Error() + " (2 attempts)"}
	if !slices.Equal(lines, want) {
		t.Errorf("persistent auth_required:\n%q\nwant\n%q", lines, want)
	}
}

func TestPagerDutyNoIncidentList(t *testing.T) {
	r := PagerDutyReader{Call: func(context.Context, string, map[string]any) (string, error) {
		return "The correct parameter is `status`, not `statuses`\nmore", nil
	}}
	ans := r.Read(context.Background(), nil)
	if ans.OK || ans.Why != "list_incidents: no incident list: The correct parameter is `status`, not `statuses`" {
		t.Errorf("answer %+v", ans)
	}
}

func TestPagerDutyInstallationUnread(t *testing.T) {
	f := &fakePagerDuty{open: []string{kagentPage}}
	_, base := readPD(t, f, nil, now)
	if got := base.Incidents["Q1"].Installation; got != "" {
		t.Fatalf("installation %q without an alert label", got)
	}
	f.installations = map[string]string{"Q1": instZ}
	_, base = readPD(t, f, base, now.Add(time.Minute))
	if got := base.Incidents["Q1"].Installation; got != instZ {
		t.Errorf("installation %q: an unread one is read again at the next reading", got)
	}
}
