package cmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"time"

	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/execution"
	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/state"
)

const executionRerun = execution.CIRerun

type executionRerunRequest struct {
	PR int `json:"pr"`
}

type executionRerunResult struct {
	Output      string `json:"output"`
	ErrorOutput string `json:"errorOutput,omitempty"`
	Code        int    `json:"code"`
}

// The coordinator retains its CI credentials. Workers can request only a
// failed-workflow rerun for their own execution branch, never arbitrary argv.
func (a *app) executionCIRerun(ctx context.Context, host string, req execution.CallbackRequest) (any, error) {
	st, err := a.store.Read()
	if err != nil {
		return nil, err
	}
	r, i := executionRecord(st, req.Ref)
	if r == nil || req.Protocol != execution.Protocol || req.RequestID == "" || req.Ref.Validate() != nil || string(req.Ref.Host) != host || !execution.VerifyCallback(req, r.Request.ControlKey) {
		return nil, errors.New("unauthenticated execution callback")
	}
	repository := r.Request.Repository
	if err := a.workerGitHubLive(req.Ref, workerGitHubRequest{Repository: repository, Write: true}); err != nil {
		return nil, err
	}
	var request executionRerunRequest
	if execution.Decode(req.Payload, &request) != nil || request.PR <= 0 {
		return nil, errors.New("invalid CI rerun request")
	}
	on := fmt.Sprintf("%s#%d", repository, request.PR)
	if executionReviewHead(ctx, r, on, "") == "" {
		return nil, refused("CI rerun requires the execution's own pull request")
	}
	// Record the request before starting CI. Keep credentials, command output
	// and external errors out of the audit trail.
	actor := st.Starts[i].Party
	if err := a.store.Update(func(*state.State) ([]state.Event, error) {
		return []state.Event{event(actor, "execution.ci-rerun-requested", "%s", on)}, nil
	}); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	github.SetDevctlSkipVersionCheck(a.cfg != nil && !a.cfg.Hooks.GuardsEnabled())
	result, runErr := runExecutionCIRerun(ctx, a.cfg.Sandbox.Devctl, repository, request.PR)
	logErr := a.store.Update(func(*state.State) ([]state.Event, error) {
		return []state.Event{event(actor, "execution.ci-rerun-result", "%s: code=%d command-error=%t", on, result.Code, runErr != nil)}, nil
	})
	return result, errors.Join(runErr, logErr)
}

// devctlBinary is devctl's default command name.
const devctlBinary = "devctl"

var runExecutionCIRerun = func(ctx context.Context, binary, repository string, pr int) (executionRerunResult, error) {
	if binary == "" {
		binary = devctlBinary
	}
	child := exec.CommandContext(ctx, binary, "pr", "rerun", repository, strconv.Itoa(pr)) //nolint:gosec // configured coordinator devctl, fixed verb and validated repository/PR
	child.Env = devctlCommandEnv("")
	var out, errOut bytes.Buffer
	child.Stdout, child.Stderr = &out, &errOut
	err := child.Run()
	result := executionRerunResult{Output: out.String(), ErrorOutput: errOut.String()}
	if err != nil {
		var status *exec.ExitError
		if !errors.As(err, &status) {
			return result, fmt.Errorf("CI rerun failed: %w", err)
		}
		result.Code = status.ExitCode()
		if result.Code < 1 {
			result.Code = ExitError
		}
	}
	return result, nil
}

func (a *app) executionRerunCmd() *cobra.Command {
	return &cobra.Command{Use: "rerun <pull-request-number>", Short: "Ask the coordinator to rerun failed CI for this execution's pull request", Long: "Rerun failed CircleCI workflows from failed without pushing an empty commit. Runs devctl pr rerun on the coordinator with its CI login; no CI credential is sent to the execution host. Starts the rerun without waiting for CI.", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		pr, err := strconv.Atoi(args[0])
		if err != nil || pr <= 0 {
			return refused("expected a positive pull request number")
		}
		s, j, err := a.executionCaller()
		if err != nil {
			return err
		}
		if !j.Request.GitHub {
			return refused("worker GitHub access is not enabled")
		}
		co := s.Config.Coordinators[j.Request.Coordinator]
		ctx, cancel := context.WithTimeout(cmd.Context(), 3*time.Minute)
		defer cancel()
		var result executionRerunResult
		if err := execution.CoordinatorCall(ctx, co.Callback, string(j.Handle.Ref.Host), executionRerun, j.Handle.Ref, executionRerunRequest{PR: pr}, j.Request.ControlKey, &result, s.Callback); err != nil {
			return err
		}
		if _, err := fmt.Fprint(cmd.OutOrStdout(), result.Output); err != nil {
			return err
		}
		if _, err := fmt.Fprint(cmd.ErrOrStderr(), result.ErrorOutput); err != nil {
			return err
		}
		if result.Code != 0 {
			return &exitError{code: result.Code, msg: "coordinator CI rerun failed"}
		}
		return nil
	}}
}
