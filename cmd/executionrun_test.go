package cmd

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/giantswarm/beekeeper/internal/execution"
	"github.com/giantswarm/beekeeper/internal/guard"
)

func TestExecutionCappedRunBindsOnlyVerifiedOwnerWithLiveAuthority(t *testing.T) {
	a, r := executionTestApp(t)
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	key, err := execution.SigningKey(filepath.Join(root, "key"))
	if err != nil {
		t.Fatal(err)
	}
	endpoint := execution.Endpoint{SSH: "coordinator.example", Executable: sshTestExecutable, Config: sshTestConfig}
	s := &execution.Server{Root: filepath.Join(root, "execution"), Rules: []byte("fixture rules"), Version: "fixture", Executable: sshTestExecutable, ConfigPath: "/etc/beekeeper/host.yaml", Config: execution.HostConfig{Identity: r.Ref.Host, RulesFile: filepath.Join(root, "host-rules.md"), WorkspaceRoot: filepath.Join(root, "workspaces"), Repositories: map[string]string{r.Repository: filepath.Join(root, "source")}, Harnesses: map[string]execution.HarnessConfig{claudeComm: {Executable: "/usr/local/bin/claude", Version: "2.1.290"}}, Coordinators: map[string]execution.Coordinator{r.Coordinator: {PublicKey: execution.PublicKey(key), Callback: endpoint}}, MaxBusy: 1}}
	digest, err := s.Prepare(s.Policy())
	if err != nil {
		t.Fatal(err)
	}
	r.PolicyDigest = digest
	j := execution.Journal{Request: r, Digest: r.Digest(), Handle: execution.Handle{Ref: r.Ref, Unit: execution.UnitName(r.Ref), ConversationID: r.ConversationID}}
	b, err := json.Marshal(j)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.Root, string(r.Ref.Execution)+".json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	s.OwnUnit = func() string { return j.Handle.Unit }
	stopped := false
	calls := 0
	s.Callback = func(_ context.Context, _ []string, raw []byte) ([]byte, error) {
		calls++
		var request execution.CallbackRequest
		if err := execution.Decode(raw, &request); err != nil {
			t.Fatal(err)
		}
		if !execution.VerifyCallback(request, r.ControlKey) || request.Operation != "guard" {
			t.Fatal("unverified run authority request")
		}
		return json.Marshal(execution.WireResponse{Protocol: execution.Protocol, Ref: r.Ref, RequestID: request.RequestID, Payload: mustExecutionJSON(t, executionAuthority{Stopped: stopped})})
	}
	a.executionRuntime = func() (*execution.Server, error) { return s, nil }
	t.Setenv(execution.EnvID, string(r.Ref.Execution))
	var options guard.Options
	if err := a.executionRunOptions(context.Background(), &options); err != nil {
		t.Fatal(err)
	}
	if options.StopWith != j.Handle.Unit || calls != 1 {
		t.Fatal("run did not bind to its verified owned service")
	}
	stopped = true
	options = guard.Options{}
	if err := a.executionRunOptions(context.Background(), &options); err == nil || options.StopWith != "" {
		t.Fatal("revoked run authority admitted a child scope")
	}
	t.Setenv(execution.EnvID, "")
	options = guard.Options{}
	if err := a.executionRunOptions(context.Background(), &options); err != nil || options.StopWith != "" {
		t.Fatal("local run gained remote lifecycle dependencies")
	}
}
