package execution

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/gofrs/flock"
	"github.com/google/uuid"

	"github.com/giantswarm/beekeeper/internal/platform"
)

type Journal struct {
	Lease               *LeaseGrant   `json:"lease,omitempty"`
	LeasePending        *LeaseRequest `json:"leasePending,omitempty"`
	Completed           bool          `json:"completed,omitempty"`
	Messages            MessageLedger `json:"messages,omitempty"`
	ActivationAttempted bool          `json:"activationAttempted"`
	RunnerPID           int           `json:"runnerPID,omitempty"`
	RunnerStarted       time.Time     `json:"runnerStarted,omitempty"`
	LastActive          time.Time     `json:"lastActive,omitzero"`
	Request             StartRequest  `json:"request"`
	Digest              string        `json:"digest"`
	Handle              Handle        `json:"handle"`
	Phase               string        `json:"phase"`
	Ready               bool          `json:"ready"`
	InTurn              bool          `json:"inTurn,omitempty"`
	StopRequest         *StopRequest  `json:"stopRequest,omitempty"`
	StopAcknowledged    bool          `json:"stopAcknowledged"`
	LastError           string        `json:"lastError,omitempty"`
	// Thread is the codex thread the first turn created; every later turn
	// resumes it and the codex guard binds tool calls to it. Claude's
	// conversation is the request's own identity and leaves it empty.
	Thread string `json:"thread,omitempty"`
}

// Server operates only on journals it created. It never discovers or adopts
// another conversation, process or unit by name.
type Server struct {
	Config                                                HostConfig
	Root, Executable, ConfigPath, Version, SecurityDigest string
	Rules                                                 []byte
	Launcher                                              platform.Launcher
	Command                                               HostCommand
	Combined                                              HostCommand
	Resources                                             func(context.Context) (Resources, error)
	GuardProbe                                            func() error
	Authorize                                             func(context.Context, StartRequest) error
	Callback                                              CommandRunner
	Now                                                   func() time.Time
	OwnUnit                                               func() string
}

func (s *Server) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}
func UnitName(r Ref) string {
	return fmt.Sprintf("beekeeper-execution-%s-%d.service", r.Execution, r.Generation)
}

func (s *Server) journalPath(r Ref) (string, error) {
	if err := r.Validate(); err != nil {
		return "", err
	}
	if r.Host != s.Config.Identity {
		return "", errors.New("execution host identity mismatch")
	}
	if err := ownedDirectory(s.Root); err != nil {
		return "", err
	}
	root, err := filepath.EvalSymlinks(s.Root)
	if err != nil {
		return "", err
	}
	if root != filepath.Clean(s.Root) {
		return "", errors.New("execution journal root must not be a symlink")
	}
	return filepath.Join(root, string(r.Execution)+".json"), nil
}

func (s *Server) lock(ctx context.Context, r Ref) (*flock.Flock, string, error) {
	p, err := s.journalPath(r)
	if err != nil {
		return nil, "", err
	}
	l := flock.New(p + ".lock")
	ok, err := l.TryLockContext(ctx, 20*time.Millisecond)
	if err != nil || !ok {
		return nil, "", errors.New("execution journal is busy")
	}
	return l, p, nil
}

func (s *Server) Read(r Ref) (Journal, error) {
	var j Journal
	p, err := s.journalPath(r)
	if err != nil {
		return j, err
	}
	if err := readJSON(p, &j); err != nil {
		return j, err
	}
	if j.Handle.Ref != r || j.Request.Ref != r || j.Handle.Unit != UnitName(r) || j.Request.Digest() != j.Digest || j.Handle.ConversationID != j.Request.ConversationID {
		return j, errors.New("execution ownership or generation mismatch")
	}
	return j, nil
}

func (s *Server) update(ctx context.Context, r Ref, f func(*Journal) error) error {
	l, p, err := s.lock(ctx, r)
	if err != nil {
		return err
	}
	defer func() { _ = l.Unlock() }()
	j, err := s.Read(r)
	if err != nil {
		return err
	}
	if err := f(&j); err != nil {
		return err
	}
	return atomicFile(p, mustJSON(j))
}

func (s *Server) Check(ctx context.Context) (Capabilities, error) {
	var c Capabilities
	p, err := s.InstalledPolicy()
	if err != nil {
		return c, err
	}
	if s.Launcher == nil || !s.Launcher.Available() {
		return c, errors.New("execution host has no user-scoped service supervisor")
	}
	if s.GuardProbe == nil {
		return c, errors.New("execution guard conformance probe is missing")
	}
	if err := s.GuardProbe(); err != nil {
		return c, err
	}
	if err := s.workerExecutable(); err != nil {
		return c, err
	}
	harnesses, unavailable := map[string]string{}, map[string]string{}
	for name, h := range s.Config.Harnesses {
		if err := s.probe(ctx, name, h); err != nil {
			unavailable[name] = err.Error()
			continue
		}
		harnesses[name] = h.Version
	}
	if len(harnesses) == 0 {
		var reasons []string
		for name, reason := range unavailable {
			reasons = append(reasons, name+": "+reason)
		}
		slices.Sort(reasons)
		return c, errors.New(strings.Join(reasons, "; "))
	}
	if len(unavailable) == 0 {
		unavailable = nil
	}
	if s.Resources == nil {
		return c, errors.New("execution host resource observation is unavailable")
	}
	resources, err := s.Resources(ctx)
	if err != nil {
		return c, err
	}
	if err := resources.Validate(s.now()); err != nil {
		return c, err
	}
	return Capabilities{Publication: true, Protocol: Protocol, Identity: s.Config.Identity, Version: s.Version, PolicyDigest: p.Digest(), Harnesses: harnesses, Unavailable: unavailable, Resources: resources}, nil
}

func (s *Server) Start(ctx context.Context, r StartRequest) (Handle, error) {
	if err := r.Validate(); err != nil {
		return Handle{}, err
	}
	co, ok := s.Config.Coordinators[r.Coordinator]
	if !ok {
		return Handle{}, errors.New("untrusted coordinating instance")
	}
	l, p, err := s.lock(ctx, r.Ref)
	if err != nil {
		return Handle{}, err
	}
	defer func() { _ = l.Unlock() }()
	j, err := s.Read(r.Ref)
	fresh := errors.Is(err, os.ErrNotExist)
	if err != nil && !fresh {
		return Handle{}, err
	}
	if !fresh {
		if j.Digest != r.Digest() {
			return Handle{}, errors.New("start request identity reused with different content")
		}
		if j.StopRequest != nil {
			return Handle{}, errors.New("execution has a durable stop request")
		}
		switch j.Phase {
		case Running, Exited:
			return j.Handle, nil
		case Activating:
			// A service may have run even when its start reply was lost.
			// Only inspection can reconcile this; never launch a second one.
			return j.Handle, errors.New("activation outcome unknown; inspect or recover this execution, do not start a replacement")
		case Failed:
			return j.Handle, errors.New("execution preparation failed; its workspace is preserved")
		}
	}
	// Serialize admission across executions as well as retries of one start.
	admission := flock.New(filepath.Join(s.Root, "admission.lock"))
	ok, err = admission.TryLockContext(ctx, 20*time.Millisecond)
	if err != nil || !ok {
		return Handle{}, errors.New("host admission is busy")
	}
	defer func() { _ = admission.Unlock() }()
	caps, err := s.Check(ctx)
	if err != nil {
		return Handle{}, err
	}
	if caps.PolicyDigest != r.PolicyDigest {
		return Handle{}, errors.New("execution policy digest mismatch")
	}
	if caps.Harnesses[r.Harness] == "" {
		return Handle{}, errors.New("execution host does not offer harness " + r.Harness + ": " + caps.Unavailable[r.Harness])
	}
	blocks := caps.Resources.Blocks
	if r.Builds {
		blocks = append(append([]string(nil), blocks...), caps.Resources.BuildBlocks...)
	}
	if len(blocks) > 0 {
		return Handle{}, errors.New("execution host has no room: " + strings.Join(blocks, "; "))
	}
	if err := s.room(ctx, r.Ref); err != nil {
		return Handle{}, err
	}
	if fresh {
		j = Journal{Request: r, Digest: r.Digest(), Phase: Preparing, Handle: Handle{Ref: r.Ref, Repository: r.Repository, Unit: UnitName(r.Ref), ConversationID: r.ConversationID}}
		j.Request.RegistrationReceipt = ""
		if err := atomicFile(p, mustJSON(j)); err != nil {
			return Handle{}, err
		}
	}
	if j.Phase == Preparing {
		if err := s.workspace(ctx, &j); err != nil {
			j.Phase, j.LastError = Failed, "workspace preparation failed; inspect the preserved workspace"
			if e := atomicFile(p, mustJSON(j)); e != nil {
				return j.Handle, e
			}
			return j.Handle, err
		}
		j.Phase = Prepared
		if err := atomicFile(p, mustJSON(j)); err != nil {
			return j.Handle, err
		}
	}
	if r.RegistrationReceipt == "" {
		return j.Handle, nil
	}
	if err := Verify(r, co.PublicKey, s.now()); err != nil {
		return j.Handle, err
	}
	if err := s.authorize(ctx, r); err != nil {
		return j.Handle, err
	}
	j.Request.RegistrationReceipt, j.Phase = r.RegistrationReceipt, Activating
	j.ActivationAttempted = true
	if err := atomicFile(p, mustJSON(j)); err != nil {
		return j.Handle, err
	}
	u := platform.Unit{Name: j.Handle.Unit, Dir: j.Handle.Workspace, Argv: []string{s.Executable, "--config", s.ConfigPath, "agents", "execution-run", string(r.Ref.Execution)}, Env: []string{EnvID + "=" + string(r.Ref.Execution), EnvCoordinator + "=" + r.Coordinator, "BEEKEEPER_CONFIG=" + s.ConfigPath}, TermIsSuccess: true, StopTimeout: time.Minute}
	if err := s.Launcher.Start(u); err != nil {
		return j.Handle, errors.New("remote unit launch outcome is unknown; inspect the recorded execution")
	}
	return j.Handle, nil
}

func (s *Server) authorize(ctx context.Context, r StartRequest) error {
	if s.Authorize != nil {
		return s.Authorize(ctx, r)
	}
	co := s.Config.Coordinators[r.Coordinator]
	var allowed ActivationDecision
	if err := CoordinatorCall(ctx, co.Callback, string(s.Config.Identity), "activate", r.Ref, r, r.ControlKey, &allowed, s.Callback); err != nil {
		return err
	}
	if !allowed.Allowed {
		return errors.New("coordinator did not authorize activation")
	}
	return nil
}

func (s *Server) room(ctx context.Context, own Ref) error {
	busy, err := s.busy(ctx, own)
	if err != nil {
		return err
	}
	if busy >= s.Config.MaxBusy {
		return errors.New("execution host maxBusy reached")
	}
	return nil
}

// Quiesce holds the host's admission, so that no execution starts until
// release, and counts the executions that still run. An update replaces the
// binary and the policy every running execution is pinned to: it goes ahead
// only at zero, before release.
func (s *Server) Quiesce(ctx context.Context) (busy int, release func(), err error) {
	if err := ownedDirectory(s.Root); err != nil {
		return 0, nil, err
	}
	admission := flock.New(filepath.Join(s.Root, "admission.lock"))
	wait, cancel := context.WithTimeout(ctx, quiesceWait)
	defer cancel()
	ok, err := admission.TryLockContext(wait, 20*time.Millisecond)
	if err != nil || !ok {
		return 0, nil, errors.New("host admission is busy")
	}
	release = func() { _ = admission.Unlock() }
	if busy, err = s.busy(ctx, Ref{}); err != nil {
		release()
		return 0, nil, err
	}
	return busy, release, nil
}

// quiesceWait bounds Quiesce's wait for a start that holds the admission.
var quiesceWait = 30 * time.Second

// busy counts the executions other than own that run or may run.
func (s *Server) busy(ctx context.Context, own Ref) (int, error) {
	paths, err := filepath.Glob(filepath.Join(s.Root, "*.json"))
	if err != nil {
		return 0, err
	}
	busy := 0
	for _, p := range paths {
		if _, err := uuid.Parse(strings.TrimSuffix(filepath.Base(p), ".json")); err != nil {
			continue
		}
		var j Journal
		if err := readJSON(p, &j); err != nil {
			return 0, errors.New("cannot establish execution host capacity")
		}
		if j.Handle.Ref == own || j.StopAcknowledged {
			continue
		}
		if j.Phase == Exited || j.Phase == Failed {
			o, err := s.Inspect(ctx, j.Handle.Ref)
			if err != nil {
				return 0, errors.New("cannot establish owned unit state")
			}
			if o.ProcessState == Exited || o.ProcessState == Failed {
				continue
			}
		}
		busy++
	}
	return busy, nil
}

func (s *Server) Inspect(ctx context.Context, r Ref) (Observation, error) {
	j, err := s.Read(r)
	if err != nil {
		return Observation{}, err
	}
	o := Observation{Handle: j.Handle, ProcessState: j.Phase, Reachability: Reachable, ObservedAt: s.now(), ReachabilityAt: s.now(), LastActive: j.LastActive, Ready: j.Ready, StopRequested: j.StopRequest != nil, StopAcknowledged: j.StopAcknowledged, LastError: j.LastError}
	if fi, err := os.Lstat(filepath.Join(s.Root, string(r.Execution)+".log")); err == nil && fi.Mode().IsRegular() && fi.ModTime().UTC().After(o.LastActive) {
		o.LastActive = fi.ModTime().UTC()
	}
	o.Lease = j.Lease
	o.Messages, o.MessageSequence = j.Messages.Status(), j.Messages.Sequence
	if j.StopAcknowledged {
		o.ProcessState = Exited
		return o, nil
	}
	if j.Phase == Prepared || j.Phase == Failed && !j.ActivationAttempted || j.Phase == Preparing {
		return o, nil
	}
	if s.Launcher == nil {
		o.ProcessState = Unknown
		return o, nil
	}
	switch s.Launcher.State(ctx, j.Handle.Unit) {
	case unitActive, Activating, "reloading", "deactivating":
		o.ProcessState = Running
		o.InTurn = j.InTurn
	case unitInactive, Failed:
		if j.StopRequest != nil {
			if err := s.update(ctx, r, func(current *Journal) error {
				current.StopAcknowledged, current.Phase, current.InTurn = true, Exited, false
				return nil
			}); err != nil {
				return Observation{}, err
			}
			o.StopAcknowledged, o.ProcessState = true, Exited
			return o, nil
		}
		if j.Phase == Failed {
			o.ProcessState = Failed
		} else if j.Ready {
			o.ProcessState = Exited
		} else {
			o.ProcessState = Failed
			o.LastError = "harness ended before accepting its first task"
		}
	default:
		o.ProcessState = Unknown
	}
	return o, nil
}

func (s *Server) Stop(ctx context.Context, r Ref, q StopRequest) (Ack, error) {
	if q.ID == "" || len(q.ID) > 128 || q.Reason == "" || len(q.Reason) > 1024 {
		return Ack{}, errors.New("stop needs an idempotency key and reason")
	}
	l, p, err := s.lock(ctx, r)
	if err != nil {
		return Ack{}, err
	}
	defer func() { _ = l.Unlock() }()
	j, err := s.Read(r)
	if err != nil {
		return Ack{}, err
	}
	if j.StopRequest != nil && j.StopRequest.ID == q.ID && *j.StopRequest != q {
		return Ack{}, errors.New("stop key reused with different content")
	}
	if j.StopAcknowledged {
		return Ack{ID: q.ID, Stage: Applied}, nil
	}
	j.StopRequest = &q
	if err := atomicFile(p, mustJSON(j)); err != nil {
		return Ack{}, err
	}
	if j.ActivationAttempted {
		if s.Launcher == nil {
			return Ack{}, errors.New("stop requested; remote supervisor unavailable")
		}
		state := s.Launcher.State(ctx, j.Handle.Unit)
		if state != unitInactive && state != Failed {
			if err := s.Launcher.Stop(ctx, j.Handle.Unit); err != nil {
				return Ack{}, errors.New("stop requested; termination acknowledgement pending")
			}
			state = s.Launcher.State(ctx, j.Handle.Unit)
		}
		if state != unitInactive && state != Failed {
			return Ack{}, errors.New("stop requested; termination is not confirmed")
		}
	}
	j.StopAcknowledged, j.Phase, j.InTurn = true, Exited, false
	if err := atomicFile(p, mustJSON(j)); err != nil {
		return Ack{}, err
	}
	return Ack{ID: q.ID, Stage: Applied}, nil
}

func (s *Server) Logs(_ context.Context, r Ref, q LogRequest) (LogPage, error) {
	if _, err := s.Read(r); err != nil {
		return LogPage{}, err
	}
	if q.Limit == 0 {
		q.Limit = 64 << 10
	}
	if q.Limit < 1 || q.Limit > MaxLog {
		return LogPage{}, errors.New("invalid log byte limit")
	}
	var offset int64
	var err error
	if q.After != "" {
		offset, err = strconv.ParseInt(q.After, 10, 64)
	}
	if err != nil || offset < 0 {
		return LogPage{}, errors.New("invalid log cursor")
	}
	p := filepath.Join(s.Root, string(r.Execution)+".log")
	fi, err := os.Lstat(p)
	if errors.Is(err, os.ErrNotExist) && offset == 0 {
		return LogPage{Next: "0"}, nil
	}
	if err != nil {
		return LogPage{}, err
	}
	if !fi.Mode().IsRegular() || offset > fi.Size() {
		return LogPage{}, errors.New("invalid owned log or cursor")
	}
	f, err := os.Open(p) //nolint:gosec // path derived solely from validated owned execution
	if err != nil {
		return LogPage{}, err
	}
	defer func() { _ = f.Close() }()
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return LogPage{}, err
	}
	b, err := io.ReadAll(io.LimitReader(f, int64(q.Limit)))
	return LogPage{Data: b, Next: strconv.FormatInt(offset+int64(len(b)), 10), Truncated: offset+int64(len(b)) < fi.Size()}, err
}

// Serve handles one bounded request. EOF and malformed data return a protocol
// refusal, never an empty successful reply. Diagnostics are not sent as data.
func (s *Server) Serve(ctx context.Context, input io.Reader, output io.Writer) error {
	b, err := io.ReadAll(io.LimitReader(input, MaxWire+1))
	if err != nil {
		return err
	}
	var req WireRequest
	reply := WireResponse{Protocol: Protocol}
	if err := Decode(b, &req); err != nil {
		reply.Error = "invalid execution request"
	} else {
		reply.RequestID, reply.Ref = req.RequestID, req.Ref
		if req.Protocol != Protocol || req.RequestID == "" {
			reply.Error = "unsupported execution protocol or request identity"
		} else {
			var value any
			switch req.Operation {
			case "check":
				value, err = s.Check(ctx)
			case "source-chunk":
				var chunk SourceChunk
				err = Decode(req.Payload, &chunk)
				if err == nil {
					err = s.ReceiveSource(ctx, req.Ref, chunk)
				}
				value = struct{}{}
			case "start":
				var r StartRequest
				err = Decode(req.Payload, &r)
				if err == nil && r.Ref != req.Ref {
					err = errors.New("request identity mismatch")
				}
				if err == nil {
					value, err = s.Start(ctx, r)
				}
			case inspectOperation:
				value, err = s.Inspect(ctx, req.Ref)
			case "sync-control":
				value, err = s.SyncControl(ctx, req.Ref, nil)
			case "stop":
				var q StopRequest
				err = Decode(req.Payload, &q)
				if err == nil {
					value, err = s.Stop(ctx, req.Ref, q)
				}
			case "logs":
				var q LogRequest
				err = Decode(req.Payload, &q)
				if err == nil {
					value, err = s.Logs(ctx, req.Ref, q)
				}
			case "message":
				var m Envelope
				err = Decode(req.Payload, &m)
				if err == nil {
					value, err = s.Message(ctx, req.Ref, m)
				}
			case "resolve-message":
				var q MessageResolution
				err = Decode(req.Payload, &q)
				if err == nil {
					value, err = s.ResolveMessage(ctx, req.Ref, q)
				}
			default:
				err = errors.New("unknown execution operation")
			}
			if err != nil {
				if req.Operation == inspectOperation && errors.Is(err, os.ErrNotExist) {
					reply.Code = "not-found"
				}
				if errors.Is(err, ErrUnsupported) {
					reply.Code = "unsupported"
				}
				reply.Error = err.Error()
			} else {
				reply.Payload, err = json.Marshal(value)
				if err != nil {
					reply.Error = "cannot encode execution result"
				}
			}
		}
	}
	return json.NewEncoder(output).Encode(reply)
}
