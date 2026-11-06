package cmd

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/internal/guard"
	"github.com/giantswarm/beekeeper/internal/state"
)

func TestAppConsentManifestDrift(t *testing.T) {
	a, _ := stubApp(t)
	current := map[string]string{"apps/" + recordedApp + "/manifest.json": recordedManifest}
	declareApps(t, a, current)
	original := appsGH
	pinned := "example/github@aaaaaaaaaaaa:apps/" + recordedApp + "/manifest.json"
	old := strings.ReplaceAll(recordedManifest, recordedCallback, "old.example.org")
	appsGH = func(ctx context.Context, args ...string) ([]byte, error) {
		if strings.Contains(strings.Join(args, " "), "?ref=aaaaaaaaaaaa") {
			return []byte(old), nil
		}
		return original(ctx, args...)
	}
	records := []state.App{{Name: recordedApp, ClientID: recordedClientID, Declared: pinned, Callbacks: []string{"old.example.org"}}}
	drift := a.appDrifts(context.Background(), records)
	line := drift[pinned]
	for _, want := range []string{recordedApp, pinned, recordedDeclared, "drift"} {
		if !strings.Contains(line, want) {
			t.Errorf("drift %q missing %q", line, want)
		}
	}

	if lines := driftLines(drift); len(lines) != 1 || lines[0] != line {
		t.Fatalf("doctor lines: %v", lines)
	}
	var out bytes.Buffer
	w := a.newWatcher(false, false)
	w.out = &out
	w.sayAppDrifts(drift)
	if !strings.Contains(out.String(), line) {
		t.Fatalf("watch output: %s", out.String())
	}
	out.Reset()
	w.sayAppDrifts(nil)
	if !strings.Contains(out.String(), "ENDED") {
		t.Fatalf("watch did not clear drift: %s", out.String())
	}
	apps := a.consentApps(context.Background(), &state.State{Apps: records}, listenerUp)
	d := guard.Click(apps, guard.Page{URL: recordedConsent, Title: recordedTitle}, "Click Authorize")
	if d.Decision != guard.Deny || !strings.Contains(d.Reason, line) {
		t.Fatalf("decision = %+v", d)
	}
	// Same callback URLs at a different commit do not indicate drift.
	old = recordedManifest
	if got := a.appDrifts(context.Background(), records); len(got) != 0 {
		t.Fatalf("matching record: %v", got)
	}
	// Callback path changes matter even when the host stays the same.
	old = strings.ReplaceAll(recordedManifest, "/callback", "/previous")
	if got := a.appDrifts(context.Background(), records); len(got) != 1 {
		t.Fatalf("path drift: %v", got)
	}
	records[0].Google = true
	if got := a.appDrifts(context.Background(), records); len(got) != 0 {
		t.Fatalf("external consent: %v", got)
	}
}
