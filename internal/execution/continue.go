package execution

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gofrs/flock"

	"github.com/giantswarm/beekeeper/internal/platform"
)

const continueOperation = "continue"

// ContinueRequest asks a host to run an exited execution again. Receipt is a
// fresh coordinator signature over the execution's own start request: a
// registration receipt lives a minute, a parked worker much longer.
type ContinueRequest struct {
	Receipt string `json:"receipt"`
}

// Continuer is a transport that can run an exited execution again.
type Continuer interface {
	Continue(context.Context, Ref, ContinueRequest) (Handle, error)
}

func (t SSHTransport) Continue(ctx context.Context, r Ref, q ContinueRequest) (Handle, error) {
	var x Handle
	err := t.Call(ctx, continueOperation, r, q, &x)
	if err == nil && (x.Ref != r || x.Unit != UnitName(r)) {
		err = errors.New("continue handle identity mismatch")
	}
	return x, err
}

// CanContinue says harness can take a conversation up again in a new process.
// omp's runner starts a fresh session and has no way back into an old one.
func CanContinue(harness string) bool {
	return harness == HarnessClaude || harness == HarnessCodex || harness == HarnessAgy
}

// continuing says this launch takes an accepted conversation up again: only a
// run that accepted its first task is Ready before its runner starts.
func (j Journal) continuing() bool { return j.Ready }

// blocked says a delivery no new turn may pass: replay stays with the
// operator (agents messages resolve).
func (l MessageLedger) blocked() bool {
	for _, d := range l.Entries {
		if d.Stage == Uncertain || d.Stage == Failed {
			return true
		}
	}
	return false
}

// Continue launches the same unit on the same journal, workspace and
// conversation; it never creates another execution. The runner takes the
// conversation up again and its first turn is the next stored message. What
// a stop ended stays ended, and a retirement already moved the workspace.
func (s *Server) Continue(ctx context.Context, r Ref, q ContinueRequest) (Handle, error) {
	l, p, err := s.lock(ctx, r)
	if err != nil {
		return Handle{}, err
	}
	defer func() { _ = l.Unlock() }()
	j, err := s.Read(r)
	if err != nil {
		return Handle{}, err
	}
	if j.Phase == Running {
		return j.Handle, nil
	}
	if err := s.continuable(ctx, j); err != nil {
		return j.Handle, err
	}
	co, ok := s.Config.Coordinators[j.Request.Coordinator]
	if !ok {
		return j.Handle, errors.New("execution coordinator is not trusted")
	}
	signed := j.Request
	signed.RegistrationReceipt = q.Receipt
	if err := Verify(signed, co.PublicKey, s.now()); err != nil {
		return j.Handle, err
	}
	admission := flock.New(filepath.Join(s.Root, admissionLock))
	ok, err = admission.TryLockContext(ctx, 20*time.Millisecond)
	if err != nil || !ok {
		return j.Handle, errors.New("host admission is busy")
	}
	defer func() { _ = admission.Unlock() }()
	policy, err := s.InstalledPolicy()
	if err != nil || policy.Digest() != j.BoundPolicy() {
		return j.Handle, errors.New("execution rules or guards changed since it ran")
	}
	if _, err := s.boundTo(j.Request.BundleDigest); err != nil {
		return j.Handle, err
	}
	caps, err := s.Check(ctx)
	if err != nil {
		return j.Handle, err
	}
	blocks := caps.Resources.Blocks
	if j.Request.Builds {
		blocks = append(append([]string(nil), blocks...), caps.Resources.BuildBlocks...)
	}
	if len(blocks) > 0 {
		return j.Handle, errors.New("execution host has no room: " + strings.Join(blocks, "; "))
	}
	if err := s.room(ctx, r); err != nil {
		return j.Handle, err
	}
	j.Request.RegistrationReceipt, j.Phase = q.Receipt, Activating
	j.Completed, j.InTurn, j.LastError = false, false, ""
	if err := atomicFile(p, mustJSON(j)); err != nil {
		return j.Handle, err
	}
	u := platform.Unit{Name: j.Handle.Unit, Dir: j.Handle.Workspace, Argv: []string{s.Executable, "--config", s.ConfigPath, "agents", "execution-run", string(r.Execution)}, Env: []string{EnvID + "=" + string(r.Execution), EnvCoordinator + "=" + j.Request.Coordinator, "BEEKEEPER_CONFIG=" + s.ConfigPath}, TermIsSuccess: true, StopTimeout: time.Minute}
	if err := s.Launcher.Start(u); err != nil {
		return j.Handle, errors.New("remote unit launch outcome is unknown; inspect the recorded execution")
	}
	return j.Handle, nil
}

// continuable refuses what must not run again. Activating stays open for a
// continuation whose launch failed: its unit is confirmed gone below.
func (s *Server) continuable(ctx context.Context, j Journal) error {
	switch {
	case j.StopRequest != nil || j.StopAcknowledged:
		return errors.New("execution was stopped")
	case j.Retirement != nil:
		return errors.New("execution is retired or retiring")
	case !j.Request.Messaging || !CanContinue(j.Request.Harness):
		return ErrUnsupported
	case !j.continuing():
		return errors.New("execution never accepted its first task")
	case j.Phase != Exited && j.Phase != Failed && j.Phase != Activating:
		return errors.New("execution has no ended process to continue")
	case j.Request.Harness != HarnessClaude && j.Thread == "":
		return errors.New("execution recorded no conversation to continue")
	case j.Messages.blocked():
		return errors.New("execution has a failed or uncertain delivery; resolve it first")
	case s.Launcher == nil:
		return errors.New("execution host has no user-scoped service supervisor")
	}
	if state := s.Launcher.State(ctx, j.Handle.Unit); state != unitInactive && state != Failed {
		return errors.New("execution unit is active or unknown")
	}
	if fi, err := os.Lstat(j.Handle.Workspace); err != nil || !fi.IsDir() {
		return errors.New("execution workspace is gone")
	}
	return nil
}
