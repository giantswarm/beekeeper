// Package execution manages owned harness processes on SSH execution hosts.
// Transport reachability never establishes whether a task or process ended.
package execution

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
)

const (
	HarnessClaude  = "claude"
	Preparing      = "preparing"
	Activating     = "activating"
	Applied        = "applied"
	ControlNote    = "note"
	ControlPark    = "park"
	ControlReport  = "report"
	unitActive     = "active"
	unitInactive   = "inactive"
	sshCommand     = "ssh"
	authCommand    = "auth"
	Protocol       = 1
	EnvID          = "BEEKEEPER_EXECUTION_ID"
	EnvCoordinator = "BEEKEEPER_EXECUTION_COORDINATOR"
	MaxWire        = 1 << 20
	MaxLog         = 256 << 10
	Prepared       = "prepared"
	Running        = "running"
	Exited         = "exited"
	Failed         = "failed"
	Unknown        = "unknown"
	Reachable      = "reachable"
	Unreachable    = "unreachable"
)

var ErrUnsupported = errors.New("remote wake acceptance is not implemented; see https://github.com/giantswarm/beekeeper/issues/1714")

type HostID string
type ExecutionID string

type Ref struct {
	Host       HostID      `json:"host"`
	Execution  ExecutionID `json:"execution"`
	Generation uint64      `json:"generation"`
}

var identityPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}$`)
var repoPattern = regexp.MustCompile(`^[a-zA-Z0-9_.-]+/[a-zA-Z0-9_.-]+$`)
var refPattern = regexp.MustCompile(`^[a-zA-Z0-9_][a-zA-Z0-9_./-]*$`)

func ValidIdentity(s string) bool { return identityPattern.MatchString(s) && s != "." && s != ".." }

func (r Ref) Validate() error {
	if !ValidIdentity(string(r.Host)) || r.Generation == 0 {
		return errors.New("invalid execution host or generation")
	}
	if id, err := uuid.Parse(string(r.Execution)); err != nil || id.String() != string(r.Execution) {
		return errors.New("invalid execution identity")
	}
	return nil
}

func (r Ref) Key() string { return fmt.Sprintf("%s/%s/%d", r.Host, r.Execution, r.Generation) }

type StartRequest struct {
	Builds         bool   `json:"builds"`
	Ref            Ref    `json:"ref"`
	RequestID      string `json:"requestID"`
	Coordinator    string `json:"coordinator"`
	Name           string `json:"name"`
	Task           string `json:"task"`
	Repository     string `json:"repository"`
	GitRef         string `json:"gitRef"`
	Harness        string `json:"harness"`
	Model          string `json:"model"`
	Prompt         string `json:"prompt"`
	PolicyDigest   string `json:"policyDigest"`
	ConversationID string `json:"conversationID"`
	// ControlKey authenticates component callbacks, never provider requests.
	// It travels only in the protected SSH request and owned journals.
	ControlKey          string `json:"controlKey"`
	RegistrationReceipt string `json:"registrationReceipt,omitempty"`
}

func (r StartRequest) Validate() error {
	if err := r.Ref.Validate(); err != nil {
		return err
	}
	if _, err := uuid.Parse(r.RequestID); err != nil {
		return errors.New("invalid start request identity")
	}
	if _, err := uuid.Parse(r.ConversationID); err != nil {
		return errors.New("invalid conversation identity")
	}
	if !validControlKey(r.ControlKey) {
		return errors.New("invalid execution control credential")
	}
	if !ValidIdentity(r.Coordinator) || !repoPattern.MatchString(r.Repository) || !refPattern.MatchString(r.GitRef) || strings.Contains(r.GitRef, "..") || strings.HasSuffix(r.GitRef, ".lock") {
		return errors.New("invalid coordinator, repository or Git ref")
	}
	if r.Harness != HarnessClaude || !refPattern.MatchString(r.Model) || len(r.Model) > 256 || strings.TrimSpace(r.Name) == "" || len(r.Name) > 256 || strings.ContainsAny(r.Name, "\x00\r\n") {
		return errors.New("remote starts require harness claude, an explicit model and a name")
	}
	if strings.TrimSpace(r.Prompt) == "" || len(r.Prompt) > 256<<10 || len(r.Task) > 1024 || !ValidDigest(r.PolicyDigest) {
		return errors.New("invalid prompt, task or policy digest")
	}
	return nil
}

func (r StartRequest) Digest() string {
	r.RegistrationReceipt = ""
	b, _ := json.Marshal(r)
	return Digest(b)
}

func Digest(b []byte) string { h := sha256.Sum256(b); return "sha256:" + hex.EncodeToString(h[:]) }
func ValidDigest(s string) bool {
	if !strings.HasPrefix(s, "sha256:") || len(s) != 71 {
		return false
	}
	_, err := hex.DecodeString(s[7:])
	return err == nil
}

type Handle struct {
	Ref            Ref    `json:"ref"`
	Repository     string `json:"repository"`
	Commit         string `json:"commit"`
	Workspace      string `json:"workspace"`
	Unit           string `json:"unit"`
	ConversationID string `json:"conversationID"`
}

type Observation struct {
	Handle           Handle    `json:"handle"`
	ProcessState     string    `json:"processState"`
	Reachability     string    `json:"reachability"`
	ObservedAt       time.Time `json:"observedAt"`
	ReachabilityAt   time.Time `json:"reachabilityAt"`
	Ready            bool      `json:"ready"`
	StopRequested    bool      `json:"stopRequested"`
	StopAcknowledged bool      `json:"stopAcknowledged"`
	LastError        string    `json:"lastError,omitempty"`
}

// FailedContact preserves the last process evidence. A transport failure
// never establishes completion, releases capacity or changes a worker's task.
func (o Observation) FailedContact(err error, now time.Time) Observation {
	o.Reachability = Reachable
	var down *UnreachableError
	if errors.As(err, &down) {
		o.Reachability = Unreachable
	}
	o.ReachabilityAt = now.UTC()
	if err != nil {
		o.LastError = err.Error()
	}
	return o
}

type Envelope struct {
	ID       string `json:"id"`
	Sequence uint64 `json:"sequence"`
	From     Ref    `json:"from"`
	To       Ref    `json:"to"`
	Kind     string `json:"kind"`
	Text     string `json:"text"`
}

type Ack struct {
	ID       string `json:"id"`
	Sequence uint64 `json:"sequence"`
	Stage    string `json:"stage"`
}
type StopRequest struct {
	ID     string `json:"id"`
	Reason string `json:"reason"`
}
type LogRequest struct {
	After string `json:"after"`
	Limit int    `json:"limit"`
}
type LogPage struct {
	Data      []byte `json:"data"`
	Next      string `json:"next"`
	Truncated bool   `json:"truncated"`
}

type Starter interface {
	Start(context.Context, StartRequest) (Handle, error)
}
type Inspector interface {
	Inspect(context.Context, Ref) (Observation, error)
}
type Messenger interface {
	Message(context.Context, Ref, Envelope) (Ack, error)
}
type Stopper interface {
	Stop(context.Context, Ref, StopRequest) (Ack, error)
}
type Logger interface {
	Logs(context.Context, Ref, LogRequest) (LogPage, error)
}
type Transport interface {
	Starter
	Inspector
	Messenger
	Stopper
	Logger
}

// ControlSyncer is the optional host outbox reconciliation operation. It
// does not implement worker wake acceptance, which remains unsupported.
type ControlSyncer interface {
	SyncControl(context.Context, Ref) (ControlResult, error)
}

type Resources struct {
	Blocks        []string  `json:"blocks,omitempty"`
	BuildBlocks   []string  `json:"buildBlocks,omitempty"`
	AvailableMiB  int       `json:"availableMiB"`
	Load1         float64   `json:"load1"`
	DiskFreeGiB   float64   `json:"diskFreeGiB"`
	SwapGrowthMiB int       `json:"swapGrowthMiB"`
	BuildBusy     bool      `json:"buildBusy"`
	ObservedAt    time.Time `json:"observedAt"`
}

func (r Resources) Validate(now time.Time) error {
	if r.AvailableMiB < 0 || r.Load1 < 0 || math.IsNaN(r.Load1) || math.IsInf(r.Load1, 0) || r.DiskFreeGiB < 0 || math.IsNaN(r.DiskFreeGiB) || math.IsInf(r.DiskFreeGiB, 0) {
		return errors.New("invalid execution host resource observation")
	}
	if r.ObservedAt.IsZero() || now.Sub(r.ObservedAt) > time.Minute || r.ObservedAt.After(now.Add(5*time.Second)) {
		return errors.New("execution host resource observation is stale")
	}
	return nil
}

type Capabilities struct {
	Protocol     int               `json:"protocol"`
	Identity     HostID            `json:"identity"`
	Version      string            `json:"version"`
	PolicyDigest string            `json:"policyDigest"`
	Harnesses    map[string]string `json:"harnesses"`
	Resources    Resources         `json:"resources"`
}

// ControlRequest is a host-to-coordinator mutation. Its sequence and receipt
// survive both processes; it cannot grant authority or select a caller.
type ControlRequest struct {
	Ref       Ref             `json:"ref"`
	ID        string          `json:"id"`
	Sequence  uint64          `json:"sequence"`
	Operation string          `json:"operation"`
	Data      json.RawMessage `json:"data"`
}

func (r ControlRequest) ContentDigest() string { r.ID = ""; r.Sequence = 0; return Digest(mustJSON(r)) }

type ControlResult struct {
	ID       string `json:"id"`
	Sequence uint64 `json:"sequence"`
	Digest   string `json:"digest"`
	Output   string `json:"output"`
	Error    string `json:"error,omitempty"`
}

// ActivationDecision is a fresh coordinator answer, never a cached grant.
type ActivationDecision struct {
	Allowed bool `json:"allowed"`
}

type UnreachableError struct {
	Operation string
	Cause     error
}

func (e *UnreachableError) Error() string {
	return fmt.Sprintf("execution host unreachable during %s: %v", e.Operation, e.Cause)
}
func (e *UnreachableError) Unwrap() error { return e.Cause }
