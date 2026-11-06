package guard

import (
	"fmt"
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/internal/lease"
)

const testProduction = "fixtureprod"

func TestHookTextPayloads(t *testing.T) {
	h := kubeHook(machineKC)
	h.Production = testProduction
	for _, command := range []string{
		`beekeeper agents idle --done --report "Blocked; muster auth login is required"`,
		"beekeeper agents idle --done --report 'Report:\nmuster auth login is required'",
		`beekeeper agents idle --done --report="Blocked; muster auth login is required"`,
		`beekeeper timer add 1m "Probe mentions kubectl --context fixtureprod apply"`,
		`beekeeper timer add 1m 'Report; muster auth login is required' --probe 'echo "--context fixtureprod"'`,
		`timeout 5 beekeeper agents idle --report "Report; muster auth login is required"`,
		`env MODE=test beekeeper agents idle --report "Report; muster auth login is required"`,
		`beekeeper agents idle --report $'Report; muster auth login is required'`,
		`beekeeper note add 'Report; muster auth login is required'`,
		`gh issue create --body 'Report; muster auth login is required'`,
		`gh pr create --body="Report; muster auth login is required"`,
		`beekeeper timer add 1m '--context fixtureprod' ; kubectl --context kind-fixture get pods`,
	} {
		t.Run(command, func(t *testing.T) { wantPassed(t, h, command) })
	}
	for _, command := range []string{
		`beekeeper agents idle --report 'Done'; muster auth login`,
		`beekeeper agents idle --report "$(muster auth login)"`,
		`beekeeper timer add 1m "$(kubectl --context fixtureprod apply -f manifest.yaml)"`,
		`kubectl --context fixtureprod apply -f manifest.yaml`,
	} {
		t.Run(command, func(t *testing.T) {
			d := decide(t, h, "/", command, nil)
			if d == nil || d.PermissionDecision != decisionDeny {
				t.Fatalf("expected refusal, got %+v", d)
			}
			token := "muster"
			if strings.Contains(command, kubectlCmd) {
				token = kubectlCmd
			}
			want := fmt.Sprintf("Matched token %q at byte %d", token, strings.Index(command, token))
			if !strings.Contains(d.Reason, want) {
				t.Errorf("reason lacks %q: %s", want, d.Reason)
			}
		})
	}
}

// Only text is exempt: what a timer or a login note runs later stays checked.
func TestHookExecutedArgumentsStayChecked(t *testing.T) {
	h := kubeHook(machineKC)
	h.Production = testProduction
	h.Leases = func() []lease.Holder { return nil }
	for _, payload := range []string{"kubectl --context fixtureprod apply -f manifest.yaml", "muster auth login", "xdg-open https://example.com"} {
		for _, command := range []string{
			`beekeeper timer add 1m "check" --probe '` + payload + `'`,
			`beekeeper timer add 1m "check" --run '` + payload + `'`,
			`beekeeper timer add 1m "check" --probe='` + payload + `'`,
			`beekeeper timer add 1m "check" --run="` + payload + `"`,
			`beekeeper timer add --run '` + payload + `' 1m "check"`,
			`beekeeper timer add 1m '--run=` + payload + `' "check"`,
			`beekeeper timer add 1m "check" --unknown 'x' --run '` + payload + `'`,
			`F=--run; beekeeper timer add $F '` + payload + `' 1m "check"`,
			`beekeeper timer add 1m -- '` + payload + `'`,
			`beekeeper note add "sign in" --kind login --until '` + payload + `'`,
			`beekeeper note add "sign in" --default '` + payload + `'`,
		} {
			t.Run(command, func(t *testing.T) {
				if d := decide(t, h, "/", command, nil); d == nil || d.PermissionDecision != decisionDeny {
					t.Fatalf("expected refusal, got %+v", d)
				}
			})
		}
	}
}

func TestHookLaterCommandsMayRead(t *testing.T) {
	h := kubeHook(machineKC)
	h.Production = testProduction
	for _, command := range []string{
		`beekeeper timer add 1m "check" --probe 'kubectl --context fixtureprod get pods'`,
		`beekeeper timer add 1m "check" --run 'beekeeper agents wake x done' --when 'pr-merged o/r#1'`,
		`beekeeper note add "sign in" --kind login --until 'muster auth login --no-browser'`,
	} {
		t.Run(command, func(t *testing.T) { wantPassed(t, h, command) })
	}
}

func TestAddTextArguments(t *testing.T) {
	for _, tc := range []struct{ command, kept string }{
		{`beekeeper timer add 1m 'muster auth login' --wake x`, "1m"},
		{`beekeeper timer add --wake x 1m 'muster auth login'`, "--wake x 1m"},
		{`beekeeper note add 'muster auth login' --why 'muster auth login' --for x`, "--for x"},
		{`beekeeper note add 'muster auth login' --status-quo='muster auth login'`, ""},
		{`beekeeper note add q --option 'muster auth login'`, "--option"},
	} {
		got := commandText(tc.command)
		if strings.Contains(got, "muster") || !strings.Contains(got, tc.kept) {
			t.Errorf("%s: masked to %s", tc.command, got)
		}
	}
}

func TestCommandTextUnparseable(t *testing.T) {
	command := `beekeeper agents idle --report 'unterminated; muster auth login`
	if got := commandText(command); got != command {
		t.Fatalf("unparseable command changed: %q", got)
	}
	if reason := musterLoginRefusal(command); !strings.Contains(reason, fmt.Sprintf("Matched token %q at byte %d", "muster", strings.Index(command, "muster"))) {
		t.Fatalf("fallback lacks matching token position: %s", reason)
	}
}
