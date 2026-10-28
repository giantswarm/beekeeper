package execution

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path"
	"strconv"
	"strings"
	"time"
)

// Endpoint names an operator-provisioned SSH route, never shell source.
type Endpoint struct {
	SSH        string `yaml:"ssh" json:"ssh"`
	Executable string `yaml:"executable" json:"executable"`
	Config     string `yaml:"config" json:"config"`
}

func HostPath(s string) bool {
	return path.IsAbs(s) && path.Clean(s) == s && !strings.ContainsAny(s, "\x00\r\n\\")
}

func (e Endpoint) Validate() error {
	if e.SSH == "" || strings.HasPrefix(e.SSH, "-") || strings.ContainsAny(e.SSH, " \t\r\n\x00\\'\"`$;&|<>()") || !HostPath(e.Executable) || !HostPath(e.Config) {
		return errors.New("SSH endpoint requires one destination and absolute clean executable/config paths")
	}
	return nil
}

// Host is the coordinator's agents.hosts entry. Timeouts are seconds so the
// execution contract has no dependency on the configuration package.
type Host struct {
	Endpoint         `yaml:",inline"`
	Identity         HostID            `yaml:"identity" json:"identity"`
	WorkspaceRoot    string            `yaml:"workspaceRoot" json:"workspaceRoot"`
	Repositories     map[string]string `yaml:"repositories" json:"repositories"`
	Harnesses        []string          `yaml:"harnesses" json:"harnesses"`
	PolicyDigest     string            `yaml:"policyDigest" json:"policyDigest"`
	MaxBusy          int               `yaml:"maxBusy" json:"maxBusy"`
	ConnectTimeout   string            `yaml:"connectTimeout,omitempty" json:"connectTimeout,omitempty"`
	OperationTimeout string            `yaml:"operationTimeout,omitempty" json:"operationTimeout,omitempty"`
}

func (h Host) Validate() error {
	if err := h.Endpoint.Validate(); err != nil {
		return err
	}
	if !ValidIdentity(string(h.Identity)) || !HostPath(h.WorkspaceRoot) || h.WorkspaceRoot == "/" || !ValidDigest(h.PolicyDigest) || h.MaxBusy < 1 || len(h.Repositories) == 0 || len(h.Harnesses) != 1 || h.Harnesses[0] != HarnessClaude {
		return errors.New("host requires identity, policy digest, workspace root, repositories, maxBusy and harnesses [claude]")
	}
	for repo, p := range h.Repositories {
		if !repoPattern.MatchString(repo) || !HostPath(p) {
			return errors.New("invalid host repository mapping")
		}
	}
	for _, raw := range []string{h.ConnectTimeout, h.OperationTimeout} {
		if raw != "" {
			d, err := time.ParseDuration(raw)
			if err != nil || d < time.Second || d > 5*time.Minute {
				return errors.New("execution timeouts must be between 1s and 5m")
			}
		}
	}
	return nil
}

type WireRequest struct {
	Protocol  int             `json:"protocol"`
	RequestID string          `json:"requestID"`
	Operation string          `json:"operation"`
	Ref       Ref             `json:"ref"`
	Payload   json.RawMessage `json:"payload,omitempty"`
}
type WireResponse struct {
	Protocol  int             `json:"protocol"`
	RequestID string          `json:"requestID"`
	Ref       Ref             `json:"ref"`
	Payload   json.RawMessage `json:"payload,omitempty"`
	Error     string          `json:"error,omitempty"`
	Code      string          `json:"code,omitempty"`
}

// CommandRunner is replaced by tests; production uses SSHCommand.
type CommandRunner func(context.Context, []string, []byte) ([]byte, error)

type SSHTransport struct {
	Host Host
	Run  CommandRunner
}

func quote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }

func SSHArgv(e Endpoint, connect time.Duration, command string) []string {
	if connect <= 0 {
		connect = 10 * time.Second
	}
	return []string{sshCommand, "-T", "-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=yes", "-o", "IdentitiesOnly=yes", "-o", "ForwardAgent=no", "-o", "ForwardX11=no", "-o", "ClearAllForwardings=yes", "-o", "PermitLocalCommand=no", "-o", "SendEnv=-*", "-o", "ConnectTimeout=" + strconv.Itoa(max(1, int(connect.Seconds()))), "--", e.SSH, command}
}

func endpointCommand(e Endpoint, role string, args ...string) string {
	s := quote(e.Executable) + " --config " + quote(e.Config) + " agents " + role + " --protocol 1"
	for _, a := range args {
		s += " " + quote(a)
	}
	return s
}

type boundedBuffer struct {
	bytes.Buffer
	limit int
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	if len(p) > b.limit-b.Len() {
		return 0, errors.New("SSH response exceeds limit")
	}
	return b.Buffer.Write(p)
}

func SSHCommand(ctx context.Context, argv []string, stdin []byte) ([]byte, error) {
	if len(argv) < 2 {
		return nil, errors.New("missing SSH command")
	}
	c := exec.CommandContext(ctx, argv[0], argv[1:]...) //nolint:gosec // fixed SSH executable and validated argument vector
	c.Stdin = bytes.NewReader(stdin)
	out := &boundedBuffer{limit: MaxWire}
	// Stderr may carry remote diagnostics or credentials. Never reflect it
	// into a protocol response or transcript.
	c.Stdout, c.Stderr = out, io.Discard
	if err := c.Run(); err != nil {
		return nil, fmt.Errorf("SSH command failed: %w", err)
	}
	return out.Bytes(), nil
}

func Decode(raw []byte, v any) error {
	if len(raw) > MaxWire {
		return errors.New("execution message exceeds limit")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return err
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return errors.New("execution message must contain exactly one JSON value")
	}
	return nil
}

func (t SSHTransport) Call(ctx context.Context, op string, ref Ref, payload, result any) error {
	if err := t.Host.Validate(); err != nil {
		return err
	}
	if op != "check" {
		if err := ref.Validate(); err != nil {
			return err
		}
		if ref.Host != t.Host.Identity {
			return errors.New("execution host mismatch")
		}
	}
	p, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req := WireRequest{Protocol: Protocol, RequestID: newID(), Operation: op, Ref: ref, Payload: p}
	b, err := json.Marshal(req)
	if err != nil || len(b) > MaxWire {
		return errors.New("invalid execution request size")
	}
	d, _ := time.ParseDuration(t.Host.OperationTimeout)
	if d <= 0 {
		d = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, d)
	defer cancel()
	run := t.Run
	if run == nil {
		run = SSHCommand
	}
	connect, _ := time.ParseDuration(t.Host.ConnectTimeout)
	raw, err := run(ctx, SSHArgv(t.Host.Endpoint, connect, endpointCommand(t.Host.Endpoint, "host-rpc")), b)
	if err != nil {
		return &UnreachableError{Operation: op, Cause: err}
	}
	var reply WireResponse
	if err := Decode(raw, &reply); err != nil {
		return &UnreachableError{Operation: op, Cause: errors.New("invalid SSH response")}
	}
	if reply.Protocol != Protocol || reply.RequestID != req.RequestID || reply.Ref != req.Ref {
		return &UnreachableError{Operation: op, Cause: errors.New("SSH response identity mismatch")}
	}
	if reply.Error != "" {
		if reply.Code == "unsupported" {
			return ErrUnsupported
		}
		return errors.New(reply.Error)
	}
	return Decode(reply.Payload, result)
}

func (t SSHTransport) Check(ctx context.Context) (Capabilities, error) {
	var x Capabilities
	err := t.Call(ctx, "check", Ref{}, nil, &x)
	if err == nil && (x.Identity != t.Host.Identity || x.Protocol != Protocol || x.PolicyDigest != t.Host.PolicyDigest) {
		err = errors.New("execution host identity, protocol or policy mismatch")
	}
	return x, err
}
func (t SSHTransport) Start(ctx context.Context, r StartRequest) (Handle, error) {
	var x Handle
	if err := r.Validate(); err != nil {
		return x, err
	}
	err := t.Call(ctx, "start", r.Ref, r, &x)
	if err == nil {
		err = ValidateHandle(t.Host, r, x)
	}
	return x, err
}

func ValidateHandle(h Host, r StartRequest, x Handle) error {
	if x.Ref != r.Ref || x.ConversationID != r.ConversationID || x.Repository != r.Repository || x.Unit != UnitName(r.Ref) || x.Workspace != path.Join(h.WorkspaceRoot, string(r.Ref.Execution), "workspace") {
		return errors.New("start handle identity or workspace mismatch")
	}
	if len(x.Commit) != 40 && len(x.Commit) != 64 {
		return errors.New("invalid pinned commit")
	}
	for _, c := range x.Commit {
		if !strings.ContainsRune("0123456789abcdef", c) {
			return errors.New("invalid pinned commit")
		}
	}
	return nil
}
func (t SSHTransport) Inspect(ctx context.Context, r Ref) (Observation, error) {
	var x Observation
	err := t.Call(ctx, "inspect", r, nil, &x)
	if err == nil && x.Handle.Ref != r {
		err = errors.New("observation identity mismatch")
	}
	return x, err
}
func (t SSHTransport) Message(ctx context.Context, r Ref, m Envelope) (Ack, error) {
	var x Ack
	err := t.Call(ctx, "message", r, m, &x)
	return x, err
}
func (t SSHTransport) Stop(ctx context.Context, r Ref, q StopRequest) (Ack, error) {
	var x Ack
	err := t.Call(ctx, "stop", r, q, &x)
	if err == nil && (x.ID != q.ID || x.Stage != Applied) {
		err = errors.New("remote termination is not acknowledged")
	}
	return x, err
}
func (t SSHTransport) Logs(ctx context.Context, r Ref, q LogRequest) (LogPage, error) {
	var x LogPage
	err := t.Call(ctx, "logs", r, q, &x)
	return x, err
}
