//go:build unix

package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/internal/state"
)

func TestExecutionCIRerunScopeAndRevocation(t *testing.T) {
	a, r := workerGitHubTestApp(t)
	oldGH, oldRun := executionReviewGH, runExecutionCIRerun
	t.Cleanup(func() { executionReviewGH, runExecutionCIRerun = oldGH, oldRun })
	own := true
	executionReviewGH = func(_ context.Context, args ...string) ([]byte, error) {
		if args[len(args)-1] != "repos/"+r.Repository+"/pulls/2224" {
			t.Fatalf("unexpected lookup: %v", args)
		}
		// Use the registered handle, including its generation.
		st, err := a.store.Read()
		if err != nil {
			t.Fatal(err)
		}
		branch := executionOwnedBranch(st.Starts[0].Execution)
		if !own {
			branch = "someone-else"
		}
		return []byte(fmt.Sprintf(`{"head":{"ref":%q,"sha":"0123456789012345678901234567890123456789","repo":{"full_name":%q}}}`, branch, r.Repository)), nil
	}
	calls := 0
	code := 8
	var commandErr error
	runExecutionCIRerun = func(_ context.Context, _, repo string, pr int) (executionRerunResult, error) {
		if repo != r.Repository || pr != 2224 {
			t.Fatal("scope changed")
		}
		calls++
		requested, err := a.store.Events(0, func(e state.Event) bool { return e.Verb == "execution.ci-rerun-requested" })
		if err != nil || len(requested) != calls {
			t.Fatal("CI started without a durable request audit", err)
		}
		return executionRerunResult{Output: `{"verdict":"authentication_required"}`, ErrorOutput: "private diagnostic", Code: code}, commandErr
	}
	var result executionRerunResult
	if err := coordinatorTestCall(t, a, r, executionRerun, executionRerunRequest{PR: 2224}, &result, false); err != nil {
		t.Fatal(err)
	}
	if calls != 1 || result.Code != 8 || result.Output == "" {
		t.Fatalf("lost coordinator outcome: %+v", result)
	}
	commandErr = errors.New("private command error")
	if err := coordinatorTestCall(t, a, r, executionRerun, executionRerunRequest{PR: 2224}, &result, false); err == nil {
		t.Fatal("command error lost")
	}
	commandErr, code = nil, 0
	if err := coordinatorTestCall(t, a, r, executionRerun, executionRerunRequest{PR: 2224}, &result, false); err != nil || result.Code != 0 {
		t.Fatalf("successful rerun failed: %+v, %v", result, err)
	}
	own = false
	if err := coordinatorTestCall(t, a, r, executionRerun, executionRerunRequest{PR: 2224}, &result, false); err == nil {
		t.Fatal("foreign PR accepted")
	}
	own = true
	bad := r
	bad.ControlKey = "invalid"
	if err := coordinatorTestCall(t, a, bad, executionRerun, executionRerunRequest{PR: 2224}, &result, false); err == nil {
		t.Fatal("unauthenticated rerun accepted")
	}
	if err := coordinatorTestCall(t, a, r, executionRerun, executionRerunRequest{PR: 0}, &result, false); err == nil {
		t.Fatal("invalid PR accepted")
	}
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) { st.Starts[0].Execution.Completed = true; return nil, nil }); err != nil {
		t.Fatal(err)
	}
	if err := coordinatorTestCall(t, a, r, executionRerun, executionRerunRequest{PR: 2224}, &result, false); err == nil {
		t.Fatal("completed worker accepted")
	}
	if calls != 3 {
		t.Fatalf("denied requests ran devctl: %d", calls)
	}
	audits, err := a.store.Events(0, func(e state.Event) bool { return strings.HasPrefix(e.Verb, "execution.ci-rerun-") })
	if err != nil || len(audits) != 6 {
		t.Fatalf("want request and result for each authorized call, got %v: %v", audits, err)
	}
	for i, audit := range audits {
		encoded, err := json.Marshal(audit)
		if err != nil || strings.Contains(string(encoded), "private") || strings.Contains(string(encoded), "authentication_required") {
			t.Fatalf("audit contains command diagnostics: %s (%v)", encoded, err)
		}
		wantVerb := "execution.ci-rerun-requested"
		if i%2 == 1 {
			wantVerb = "execution.ci-rerun-result"
		}
		if audit.Verb != wantVerb {
			t.Fatalf("audit order: got %s, want %s", audit.Verb, wantVerb)
		}
		wantDetail := r.Repository + "#2224"
		if i%2 == 1 {
			wantDetail += []string{": code=8 command-error=false", ": code=8 command-error=true", ": code=0 command-error=false"}[i/2]
		}
		if audit.Detail != wantDetail || audit.By.Name != r.Name {
			t.Fatalf("incorrect audit attribution or result: %+v", audit)
		}
	}
}

func TestRunExecutionCIRerunPreservesCLIResult(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "devctl")
	script := "#!/bin/sh\n[ \"$*\" = 'pr rerun giantswarm/beekeeper 2224' ] || exit 99\nprintf '{\"verdict\":\"refused\"}'\nprintf 'CI login required' >&2\nexit 8\n"
	if err := os.WriteFile(binary, []byte(script), 0o700); err != nil { //nolint:gosec // G306: executable probe in a private test directory
		t.Fatal(err)
	}
	result, err := runExecutionCIRerun(context.Background(), binary, sshTestRepository, 2224)
	if err != nil || result.Code != 8 || !json.Valid([]byte(result.Output)) || result.ErrorOutput != "CI login required" {
		t.Fatalf("result: %+v, %v", result, err)
	}
}
