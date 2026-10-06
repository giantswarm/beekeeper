package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/internal/sandbox"
)

// agentBK1 is the tests' agent, labX their lab's kind cluster, briefMD a
// start's brief.
const (
	agentBK1 = "BK 1"
	labX     = "lab-x"
	briefMD  = "brief.md"
)

func TestBrokeredHostArgv(t *testing.T) {
	for _, tc := range []struct {
		argv func(sandbox.Request, bool) ([]string, error)
		req  sandbox.Request
		want string
	}{
		{brokeredAgentsArgv, sandbox.Request{Args: []string{agentStartName, "--dir=/w", "--", agentBK1, briefMD}}, "agents start --dir=/w -- BK 1 " + briefMD},
		{brokeredAgentsArgv, sandbox.Request{Args: []string{agentWakeName, "--", agentBK1, "--as x"}}, "agents wake -- BK 1 --as x"},
		{brokeredAgentsArgv, sandbox.Request{Args: []string{agentResumeName, "--", agentBK1}}, "agents resume -- BK 1"},
		{brokeredWatchArgv, sandbox.Request{Args: []string{"watch", "--notify=true", "--"}}, "watch --notify=true --"},
		{brokeredLabArgv, sandbox.Request{Resource: labOne, Args: []string{labUp}}, "lease up agentlab-1"},
		{brokeredLabArgv, sandbox.Request{Resource: "agentlab-2", Args: []string{labDown}}, "lease down agentlab-2"},
	} {
		got, err := tc.argv(tc.req, true)
		if err != nil || strings.Join(got, " ") != tc.want {
			t.Errorf("%q: %q, %v; want %q", tc.req.Args, got, err, tc.want)
		}
	}
	for _, tc := range []struct {
		argv func(sandbox.Request, bool) ([]string, error)
		req  sandbox.Request
	}{
		{brokeredAgentsArgv, sandbox.Request{Args: []string{"remove", "--", agentBK1}}},
		{brokeredAgentsArgv, sandbox.Request{Args: []string{"handover", "--", agentBK1}}},
		{brokeredAgentsArgv, sandbox.Request{Args: []string{agentWakeName, "--as=supervisor", "--", agentBK1, "hi"}}},
		{brokeredAgentsArgv, sandbox.Request{Args: []string{agentStartName, "--config", "/tmp/c.yaml", "--", agentBK1, "b"}}},
		{brokeredAgentsArgv, sandbox.Request{}},
		{brokeredWatchArgv, sandbox.Request{Args: []string{ghSecret, compareOp}}},
		{brokeredWatchArgv, sandbox.Request{Args: []string{"watch", "--config=/tmp/c.yaml"}}},
		{brokeredLabArgv, sandbox.Request{Resource: labOne, Args: []string{"claim"}}},
		{brokeredLabArgv, sandbox.Request{Resource: "../x", Args: []string{labUp}}},
		{brokeredLabArgv, sandbox.Request{Resource: labOne, Args: []string{labUp, "--as=x"}}},
	} {
		if got, err := tc.argv(tc.req, true); err == nil {
			t.Errorf("%+v: %q, want a refusal", tc.req, got)
		}
	}
}

func TestLabArgv(t *testing.T) {
	none, other, same, plain := t.TempDir(), t.TempDir(), t.TempDir(), t.TempDir()
	for dir, body := range map[string]string{other: "clusterName: agentlab-2\n", same: "clusterName: \"lab-x\"\n", plain: "components: {}\n"} {
		if err := os.WriteFile(filepath.Join(dir, "agentlab.yaml"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct{ op, cluster, dir, want string }{
		{labUp, labX, none, "agentlab --lab lab-x up --open=false --trust=false"},
		{labDown, labX, none, "agentlab --lab lab-x down"},
		{labUp, labX, same, "agentlab up --open=false --trust=false"},
		{labDown, "agentlab", plain, "agentlab down"},
	} {
		got, err := labArgv(tc.op, tc.cluster, tc.dir)
		if err != nil || strings.Join(got, " ") != tc.want {
			t.Errorf("%s %s in %s: %q, %v; want %q", tc.op, tc.cluster, tc.dir, got, err, tc.want)
		}
	}
	if got, err := labArgv(labUp, labX, other); Code(err) != ExitRefused {
		t.Errorf("another lab's directory: %q, exit %d, want refused", got, Code(err))
	}
}

func TestScratchConfig(t *testing.T) {
	args, scratch, err := scratchConfig([]string{agentStartName, "--task=t", "--config=/s/bk.yaml", "--", agentBK1, "--config=/x"})
	if err != nil || scratch != "/s/bk.yaml" || strings.Join(args, " ") != "start --task=t -- BK 1 --config=/x" {
		t.Errorf("got %q, %q, %v", args, scratch, err)
	}
	if args, scratch, err := scratchConfig([]string{agentWakeName, "--", agentBK1}); err != nil || scratch != "" || len(args) != 3 {
		t.Errorf("no scratch: %q, %q, %v", args, scratch, err)
	}
	if _, _, err := scratchConfig([]string{agentStartName, "--config=bk.yaml", "--", agentBK1, "b"}); err == nil {
		t.Error("a relative scratch configuration: want a refusal")
	}
}
