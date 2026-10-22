package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/lease"
)

// releaseVerb is the lease subcommand the flag cases run.
const releaseVerb = "release"

// labYAML is a lab's agentlab.yaml as agentlab writes it, without overlay.
const labYAML = `# Written by agentlab v0.1.0.
clusterName: agentlab
platform:
    enabled: true
    chartVersion: 4.164.3
`

// labConfigs writes an agentlab.yaml for each lab cluster and returns the
// directories as agentlab list maps them.
func labConfigs(t *testing.T, clusters ...string) map[string]string {
	t.Helper()
	dirs := map[string]string{}
	for _, cl := range clusters {
		dir := filepath.Join(t.TempDir(), cl)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, labConfigFileName), []byte(labYAML), 0o600); err != nil {
			t.Fatal(err)
		}
		dirs[cl] = dir
	}
	return dirs
}

// labConfigPath is the agentlab.yaml of the lab the test app's lease res
// stands for.
func labConfigPath(t *testing.T, a *app, res string) string {
	t.Helper()
	dirs, err := a.labDirs()
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(dirs[a.cfg.LabCluster(res)], labConfigFileName)
}

// overlay adds a proof's overlay to the lab's agentlab.yaml, as a holder's
// agentlab configure does, its version comment rewritten.
func overlay(t *testing.T, path string) {
	t.Helper()
	raw := strings.Replace(labYAML, "v0.1.0", "v0.2.0", 1) + "    valuesFiles:\n        - overlay-857-freshdb.yaml\n"
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
}

// A lab's claim records its agentlab.yaml; a release with it unchanged (a
// rewritten comment is no change) frees the lab and says so.
func TestLabReleaseUnchanged(t *testing.T) {
	a := claimApp(t, claimGrant(labOne, claimMe, time.Minute))
	if out, err := runLease(a, "claim", labOne, "-p", "my proof"); err != nil {
		t.Fatalf("claim: %q, %v", out, err)
	}
	h, _ := lease.Dir(a.cfg.LeaseDir).Get(labOne)
	if h == nil || h.LabConfig == nil || h.LabConfig.Path != labConfigPath(t, a, labOne) || len(h.LabConfig.SHA256) != 64 || len(h.LabConfig.ValuesFiles) != 0 {
		t.Fatalf("the claim recorded no configuration: %+v", h)
	}
	path := labConfigPath(t, a, labOne)
	if err := os.WriteFile(path, []byte(strings.Replace(labYAML, "v0.1.0", "v0.2.0", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := runLease(a, "status", labOne)
	if Code(err) != ExitRefused || !strings.Contains(err.Error(), "\nagentlab.yaml is at its claim-time state") {
		t.Errorf("status of the held lab: %q, %v", out, err)
	}
	out, err = runLease(a, "release", labOne)
	if err != nil || out != "released agentlab-1; agentlab.yaml is at its claim-time state\n" {
		t.Errorf("release: %q, %v", out, err)
	}
}

// A release of a lab whose agentlab.yaml carries an overlay the claim did
// not find is refused naming the key and keeps the lease; --restore puts
// the claim-time file back, the released one beside it.
func TestLabReleaseChangedIsRefusedThenRestored(t *testing.T) {
	a := claimApp(t, claimGrant(labOne, claimMe, time.Minute))
	if out, err := runLease(a, "claim", labOne, "-p", "my proof"); err != nil {
		t.Fatalf("claim: %q, %v", out, err)
	}
	path := labConfigPath(t, a, labOne)
	overlay(t, path)
	a.json = true
	out, err := runLease(a, "status", labOne)
	a.json = false
	if Code(err) != ExitRefused || !strings.Contains(err.Error(), "\nagentlab.yaml changed since the claim in platform.valuesFiles") {
		t.Errorf("status of the changed lab: %v", err)
	}
	var v leaseView
	if jerr := json.Unmarshal([]byte(out), &v); jerr != nil || v.LabConfigState == nil || v.LabConfigState.State != labConfigChanged {
		t.Errorf("status --json: %q, %v", out, jerr)
	}
	out, err = runLease(a, "release", labOne)
	if Code(err) != ExitRefused || !strings.Contains(err.Error(), "agentlab-1 not released: its agentlab.yaml ("+path+") differs from its claim-time state in platform.valuesFiles; put it back with `beekeeper lease release agentlab-1 --restore`") {
		t.Errorf("release of the changed lab: %q, %v", out, err)
	}
	if h, _ := lease.Dir(a.cfg.LeaseDir).Get(labOne); h == nil {
		t.Fatal("the refused release freed the lab")
	}
	out, err = runLease(a, "release", labOne, "--restore")
	if err != nil || !strings.HasPrefix(out, "released agentlab-1; restored agentlab.yaml to its claim-time state (platform.valuesFiles; the released one is "+path+".released)") {
		t.Errorf("release --restore: %q, %v", out, err)
	}
	if raw, _ := os.ReadFile(path); string(raw) != labYAML { //nolint:gosec // the test's lab
		t.Errorf("restored agentlab.yaml: %q", raw)
	}
	if raw, _ := os.ReadFile(path + ".released"); !strings.Contains(string(raw), "overlay-857-freshdb.yaml") { //nolint:gosec // the test's lab
		t.Errorf("the released agentlab.yaml was not kept: %q", raw)
	}
	if out, err := runLease(a, "status", labOne); err != nil || out != "free\n" {
		t.Errorf("status after the restore: %q, %v", out, err)
	}
}

// A declared change releases the lab and is shown to its next claim and,
// until then, by status; the next holder's release compares with the
// changed file it claimed.
func TestLabKeepConfigIsShownToTheNextClaim(t *testing.T) {
	a := claimApp(t, claimGrant(labOne, claimMe, 2*time.Minute), claimGrant(labOne, claimMe, time.Minute))
	if out, err := runLease(a, "claim", labOne, "-p", "my proof"); err != nil {
		t.Fatalf("claim: %q, %v", out, err)
	}
	overlay(t, labConfigPath(t, a, labOne))
	out, err := runLease(a, "release", labOne, "--keep-config", "the fresh database stays for the follow-up proof")
	if err != nil || out != "released agentlab-1; agentlab.yaml left changed in platform.valuesFiles, declared: the next claim is told\n" {
		t.Errorf("release --keep-config: %q, %v", out, err)
	}
	want := `agentlab.yaml was left changed by "Agent me" at ` + relayNow.UTC().Format(time.RFC3339) + " in platform.valuesFiles: the fresh database stays for the follow-up proof"
	if out, err := runLease(a, "status", labOne); err != nil || out != "free; "+want+"\n" {
		t.Errorf("status of the free lab: %q, %v", out, err)
	}
	out, err = runLease(a, "claim", labOne, "-p", "the follow-up proof")
	if err != nil || !strings.Contains(out, "(kind cluster agentlab, running)\n"+want+"\nexport KUBECONFIG=") {
		t.Errorf("the next claim: %q, %v", out, err)
	}
	h, _ := lease.Dir(a.cfg.LeaseDir).Get(labOne)
	if h == nil || h.LabChange == nil || h.LabChange.Why == "" || h.LabConfig == nil || len(h.LabConfig.ValuesFiles) != 1 {
		t.Errorf("the next holder's record: %+v", h)
	}
	if c, _ := lease.Dir(a.cfg.LeaseDir).LabChangeOf(labOne); c != nil {
		t.Errorf("the change still waits after its claim: %+v", c)
	}
	if out, err := runLease(a, "release", labOne); err != nil || out != "released agentlab-1; agentlab.yaml is at its claim-time state\n" {
		t.Errorf("the next holder's release: %q, %v", out, err)
	}
}

// A --force release of a gone holder's changed lab frees it and leaves the
// change, not declared, for the next claim.
func TestLabForceReleaseLeavesTheChange(t *testing.T) {
	a := claimApp(t)
	dir := lease.Dir(a.cfg.LeaseDir)
	path := labConfigPath(t, a, labOne)
	c, raw, err := lease.ReadLabConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dir.Claim(labOne, lease.Holder{Env: labOne, Session: claimOther.Session, Name: claimOther.Name, Purpose: "its proof", LabConfig: &c}); err != nil {
		t.Fatal(err)
	}
	if err := dir.KeepLabConfig(labOne, raw); err != nil {
		t.Fatal(err)
	}
	overlay(t, path)
	out, err := runLease(a, "release", labOne, "--force")
	if err != nil || out != `released agentlab-1 (held by "Agent other": its proof); agentlab.yaml left changed in platform.valuesFiles, not declared: the next claim is told`+"\n" {
		t.Errorf("release --force: %q, %v", out, err)
	}
	if ch, _ := dir.LabChangeOf(labOne); ch == nil || ch.Why != "" || ch.By != claimMe.Name {
		t.Errorf("the left change: %+v", ch)
	}
}

// --restore and --keep-config are for labs, one at a time, and a
// declaration says why.
func TestLabReleaseFlags(t *testing.T) {
	a := claimApp(t)
	for _, args := range [][]string{
		{graveler, "--restore"},
		{labOne, "--restore", "--keep-config", "why"},
		{labOne, "--keep-config", " "},
	} {
		if _, err := runLease(a, append([]string{releaseVerb}, args...)...); Code(err) != ExitUsage {
			t.Errorf("%v: %v, want a usage error", args, err)
		}
	}
}

// A lab agentlab does not list is claimed without a record, and its
// release compares nothing.
func TestLabClaimWithoutConfig(t *testing.T) {
	a := claimApp(t, claimGrant(labOne, claimMe, time.Minute))
	a.labDirs = func() (map[string]string, error) { return map[string]string{}, nil }
	out, err := runLease(a, "claim", labOne, "-p", "my proof")
	if err != nil || !strings.Contains(out, "\nagentlab.yaml not recorded (agentlab lists no lab agentlab): the release compares nothing\nexport KUBECONFIG=") {
		t.Errorf("claim: %q, %v", out, err)
	}
	if out, err := runLease(a, "release", labOne); err != nil || out != "released agentlab-1\n" {
		t.Errorf("release: %q, %v", out, err)
	}
}
