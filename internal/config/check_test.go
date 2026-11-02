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
		{name: "duplicates", raw: "resources: [a]\nwatch: {loadMax: 1, loadMax: 2}\nlabs: {a: a, a: b}\n", want: []string{"watch.loadMax: duplicate key", "labs.a: duplicate key"}},
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
			for _, want := range tc.want {
				found := false
				for _, finding := range findings {
					found = found || strings.HasPrefix(finding, want)
				}
				if !found {
					t.Errorf("findings %v; want prefix %q", findings, want)
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

func TestCheckReviewRegressions(t *testing.T) {
	for _, tc := range []struct {
		name  string
		raw   string
		want  []string
		loads bool
	}{
		{name: "nested provider usage", raw: "capacity: {providers: {claude: {usage: {command: [echo]}}}}", loads: true},
		{name: "nested usage typo", raw: "capacity: {providers: {claude: {usage: {commnad: [echo]}}}}", want: []string{"capacity.providers.claude.usage.commnad:"}},
		{name: "explicit override", raw: "supervisor: {<<: {workerBrief: /missing-review-2062}, workerBrief: ''}", loads: true},
		{name: "explicit type override", raw: "watch: {<<: {loadMax: bad}, loadMax: 2}", loads: true},
		{name: "sequence first wins", raw: "supervisor: {<<: [{workerBrief: ''}, {workerBrief: /missing-review-2062}]}", loads: true},
		{name: "sequence first type wins", raw: "watch: {<<: [{loadMax: 2}, {loadMax: bad}]}", loads: true},
		{name: "sequence first missing file", raw: "supervisor: {<<: [{workerBrief: /missing-review-2062}, {workerBrief: ''}]}", want: []string{"supervisor.workerBrief:"}},
		{name: "nested aliases and merges", raw: "supervisor: {<<: [&base {<<: {workerBrief: /missing-review-2062}, workerBrief: ''}, *base]}", loads: true},
		{name: "map merge overrides", raw: "metrics: {models: {<<: {model: {input: bad}}, model: {input: 1}}}", loads: true},
		{name: "independent rules", raw: "run: {tail: wrong}\ndisk: {prune: wrong}", want: []string{"run.tail:", "disk.prune:"}},
		{name: "mixed findings", raw: "run: {tail: wrong}\ndisk: {prune: wrong}\nwatch: {loadMax: bad}\nreporter: {brief: /missing-review-2062}\nsecret: {ageIdentities: [{recipient: age1x, ref: store://keys/age}]}", want: []string{"run.tail:", "disk.prune:", "watch.loadMax:", "reporter.brief:", "secret.ageIdentities[0]:"}},
		{name: "nested usage wrong type", raw: "capacity: {providers: {claude: {usage: {command: [echo], highPercent: bad}}}}", want: []string{"capacity.providers.claude.usage.highPercent:"}},
		{name: "multiple usage rules", raw: "capacity: {usage: {command: [echo], highPercent: -1, fiveHourMax: -1, sevenDayMax: -1}}", want: []string{"capacity.usage.highPercent:", "capacity.usage.fiveHourMax:", "capacity.usage.sevenDayMax:"}},
		{name: "independent rules in section", raw: "capacity: {ceiling: -1, reviewHeadroom: -1}", want: []string{"capacity.ceiling:", "capacity.reviewHeadroom:"}},
		{name: "bad field isolated within section", raw: "capacity: {ceiling: bad, reviewHeadroom: -1}", want: []string{"capacity.ceiling:", "capacity.reviewHeadroom:"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			file := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(file, []byte(tc.raw), 0o600); err != nil {
				t.Fatal(err)
			}
			if tc.loads {
				if _, err := Load(file); err != nil {
					t.Fatalf("fixture must load: %v", err)
				}
			}
			findings, err := Check(file)
			if err != nil {
				t.Fatal(err)
			}
			if len(findings) != len(tc.want) {
				t.Fatalf("findings = %v; want paths %v", findings, tc.want)
			}
			for _, path := range tc.want {
				found := false
				for _, finding := range findings {
					found = found || strings.HasPrefix(finding, path)
				}
				if !found {
					t.Errorf("missing %s in %v", path, findings)
				}
			}
		})
	}
}
