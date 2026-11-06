package cmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"

	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/execution"
)

const (
	logName          = "log"
	executionLogRead = "log-read"
	// executionLogLimit keeps a reply within execution.MaxWire, whose JSON
	// escapes can double what it carries.
	executionLogLimit = 256 << 10
)

type executionLogRequest struct {
	Lines int `json:"lines"`
	// DefaultLines leaves -n out, as the worker did: --since then means the
	// whole window.
	DefaultLines bool   `json:"defaultLines,omitempty"`
	Verb         string `json:"verb,omitempty"`
	Since        string `json:"since,omitempty"`
	Until        string `json:"until,omitempty"`
	Short        bool   `json:"short,omitempty"`
	JSON         bool   `json:"json,omitempty"`
}

type executionLogResult struct {
	Output string `json:"output"`
	Code   int    `json:"code"`
	Error  string `json:"error,omitempty"`
}

// executionLog serves a worker on a host a read of the coordinator's event log,
// its proof that something ran there after a merge. Only the log's own flags
// cross, and the coordinator's command reads and bounds it.
func (a *app) executionLog(ctx context.Context, host string, req execution.CallbackRequest) (any, error) {
	st, err := a.store.Read()
	if err != nil {
		return nil, err
	}
	r, _ := executionRecord(st, req.Ref)
	if r == nil || req.Protocol != execution.Protocol || req.RequestID == "" || req.Ref.Validate() != nil || string(req.Ref.Host) != host || !execution.VerifyCallback(req, r.Request.ControlKey) {
		return nil, errors.New("unauthenticated execution callback")
	}
	if why := a.executionEnded(st, r); why != "" {
		return nil, errors.New("coordinator log unavailable: " + why)
	}
	var request executionLogRequest
	if execution.Decode(req.Payload, &request) != nil {
		return nil, errors.New("invalid log request")
	}
	copyApp := *a
	var output bytes.Buffer
	copyApp.out, copyApp.json = &output, request.JSON
	command := copyApp.logCmd()
	args := []string{}
	if !request.DefaultLines {
		args = append(args, "--lines", strconv.Itoa(request.Lines))
	}
	for flag, value := range map[string]string{"--verb": request.Verb, "--since": request.Since, "--until": request.Until} {
		if value != "" {
			args = append(args, flag, value)
		}
	}
	if request.Short {
		args = append(args, "--short")
	}
	command.SetArgs(args)
	command.SilenceUsage, command.SilenceErrors = true, true
	command.SetOut(&output)
	command.SetErr(&output)
	result := executionLogResult{}
	if err := command.ExecuteContext(ctx); err != nil {
		result.Code, result.Error = Code(err), err.Error()
	}
	result.Output = output.String()
	if len(result.Output) > executionLogLimit {
		result.Output = "(older events cut: narrow the read with --since, --until, --verb or -n)\n" + result.Output[len(result.Output)-executionLogLimit:]
	}
	return result, nil
}

// remoteExecutionLog is beekeeper log for a worker on a host: the coordinator
// holds the log, so its flags go there and its answer comes back.
func (a *app) remoteExecutionLog(cmd *cobra.Command, _ []string) error {
	s, j, err := a.executionCaller()
	if err != nil {
		return err
	}
	flags := cmd.Flags()
	var request executionLogRequest
	request.JSON = a.json
	if request.Lines, err = flags.GetInt("lines"); err != nil {
		return err
	}
	if request.Short, err = flags.GetBool("short"); err != nil {
		return err
	}
	for name, value := range map[string]*string{"verb": &request.Verb, "since": &request.Since, "until": &request.Until} {
		if *value, err = flags.GetString(name); err != nil {
			return err
		}
	}
	request.DefaultLines = !flags.Changed("lines")
	co := s.Config.Coordinators[j.Request.Coordinator]
	var result executionLogResult
	if err := execution.CoordinatorCall(cmd.Context(), co.Callback, string(j.Handle.Ref.Host), executionLogRead, j.Handle.Ref, request, j.Request.ControlKey, &result, s.Callback); err != nil {
		return err
	}
	if _, err := io.WriteString(a.out, result.Output); err != nil {
		return err
	}
	if result.Code != 0 {
		return &exitError{code: result.Code, msg: fmt.Sprintf("coordinator log: %s", result.Error)}
	}
	return nil
}
