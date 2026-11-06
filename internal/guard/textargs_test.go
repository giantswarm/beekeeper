package guard

import (
	"fmt"
	"strings"
	"testing"
)

func TestHookTextPayloads(t *testing.T) {
	h := kubeHook(machineKC)
	h.Production = "fixtureprod"
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

func TestCommandTextUnparseable(t *testing.T) {
	command := `beekeeper agents idle --report 'unterminated; muster auth login`
	if got := commandText(command); got != command {
		t.Fatalf("unparseable command changed: %q", got)
	}
	if reason := musterLoginRefusal(command); !strings.Contains(reason, fmt.Sprintf("Matched token %q at byte %d", "muster", strings.Index(command, "muster"))) {
		t.Fatalf("fallback lacks matching token position: %s", reason)
	}
}
