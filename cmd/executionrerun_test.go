//go:build unix

package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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
	runExecutionCIRerun = func(_ context.Context, _, repo string, pr int) (executionRerunResult, error) {
		if repo != r.Repository || pr != 2224 {
			t.Fatal("scope changed")
		}
		calls++
		return executionRerunResult{Output: `{"verdict":"authentication_required"}`, Code: 8}, nil
	}
	var result executionRerunResult
	if err := coordinatorTestCall(t, a, r, executionRerun, executionRerunRequest{PR: 2224}, &result, false); err != nil {
		t.Fatal(err)
	}
	if calls != 1 || result.Code != 8 || result.Output == "" {
		t.Fatalf("lost coordinator outcome: %+v", result)
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
	if calls != 1 {
		t.Fatalf("denied requests ran devctl: %d", calls)
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
