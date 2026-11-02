package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCheck(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
		want []string
	}{
		{name: "empty"},
		{name: "supported values", raw: "grantTTL: 2m\ncapacity: {ceiling: 2, providers: {codex: {ceiling: 1}}}\nalerts: {installations: [test, {name: other, floor: info}]}\n"},
		{name: "all findings", raw: "typo: true\nwatch: {intervalTypo: 1m, loadMax: broken}\ncapacity: {providers: {codex: {ceilng: 3}}}\nreporter: {brief: /missing-config-check-brief}\n", want: []string{"typo: unknown key", "watch.intervalTypo: unknown key", "watch.loadMax: invalid value", "capacity.providers.codex.ceilng: unknown key", "reporter.brief: referenced file does not exist"}},
		{name: "custom scalars", raw: "grantTTL: soon\nguide: {relayAt: lots}\nalerts: {installations: [{name: x, flooor: info}]}\n", want: []string{"grantTTL: invalid value", "guide.relayAt: invalid value", "alerts.installations[0].flooor: unknown key"}},
		{name: "duration mapping", raw: "grantTTL: {duration: 2}\n", want: []string{"grantTTL: invalid value"}},
		{name: "wrong containers", raw: "watch: [bad]\nresources: {bad: value}\n", want: []string{"watch: invalid value", "resources: invalid value"}},
		{name: "duplicates", raw: "watch: {loadMax: 1, loadMax: 2}\nlabs: {a: a, a: b}\n", want: []string{"watch.loadMax: duplicate key", "labs.a: duplicate key"}},
		{name: "aliases", raw: "watch: &watch {loadMax: bad}\nwatch: *watch\n", want: []string{"watch.loadMax: invalid value", "watch: duplicate key"}},
		{name: "merge", raw: "watch: {<<: &base {badKey: 1}, loadMax: 2}\n", want: []string{"watch.badKey: unknown key"}},
		{name: "semantic", raw: "run: {tail: wrong}\n", want: []string{"run.tail:"}},
		{name: "incomplete", raw: "secret: {ageIdentities: [{recipient: age1x, ref: store://keys/age}]}\n", want: []string{"secret.ageIdentities[0]:"}},
		{name: "output need not exist", raw: "stateDir: /missing-config-check-output\napps: {secrets: {test: {file: /missing-output.sops.yaml}}}\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			file := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(file, []byte(tc.raw), 0o600); err != nil {
				t.Fatal(err)
			}
			findings, err := Check(file)
			if err != nil {
				t.Fatal(err)
			}
			if len(findings) != len(tc.want) {
				t.Fatalf("findings = %v; want %v", findings, tc.want)
			}
			for i, want := range tc.want {
				if !strings.HasPrefix(findings[i], want) {
					t.Errorf("finding %q; want prefix %q", findings[i], want)
				}
			}
			got, err := os.ReadFile(filepath.Clean(file))
			if err != nil || string(got) != tc.raw {
				t.Fatal("check changed configuration", err)
			}
		})
	}
}

func TestCheckInputFiles(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	if err := os.WriteFile(filepath.Join(home, "brief"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(home, "config.yaml")
	raw := "supervisor: {reviewBrief: ~/brief, workerBrief: ~/missing}\nalerts: {kubeconfig: [~/brief, ~/missing]}\nguide: {instructions: ~/brief}\n"
	if err := os.WriteFile(file, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	findings, err := Check(file)
	if err != nil || len(findings) != 2 || !strings.HasPrefix(findings[0], "supervisor.workerBrief:") || !strings.HasPrefix(findings[1], "alerts.kubeconfig[1]:") {
		t.Fatalf("Check = %v, %v", findings, err)
	}
	if _, err := Check(filepath.Join(home, "absent")); !os.IsNotExist(err) {
		t.Fatalf("missing file: %v", err)
	}
}
