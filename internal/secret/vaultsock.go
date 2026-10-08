package secret

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"
)

// The person hands the vault session to the broker over a Unix socket in
// the runtime directory: mode 0600 in a 0700 directory, which the agent
// sandbox can neither reach (it blocks every Unix socket) nor write. A
// request is one JSON line, the answer one JSON line; nothing on it ever
// answers a session, only whether one is held.

// The socket's operations.
const (
	VaultUnlock = "unlock"
	VaultLock   = "lock"
	VaultStatus = "status"
)

// VaultRequest is one ask of the keeper's socket.
type VaultRequest struct {
	Op    string `json:"op"`
	Name  string `json:"name,omitempty"`
	Token string `json:"token,omitempty"`
}

// VaultState is the keeper's answer.
type VaultState struct {
	Unlocked bool      `json:"unlocked"`
	Since    time.Time `json:"since,omitzero"`
	Until    time.Time `json:"until,omitzero"`
	// Error is why the last sign-in failed for good, after its retries.
	Error string `json:"error,omitempty"`
	// Retrying is what the running sign-in waits on or retries after, and
	// when it looks again; empty once it unlocked or failed for good.
	Retrying string `json:"retrying,omitempty"`
	// Dropped is why op stopped taking the last session, at DroppedAt;
	// empty once a sign-in unlocked again.
	Dropped   string    `json:"dropped,omitempty"`
	DroppedAt time.Time `json:"droppedAt,omitzero"`
}

// maxVaultRequest bounds what the keeper reads of one request.
const maxVaultRequest = 8 << 10

// SocketPath is the keeper's socket under the runtime directory.
func SocketPath() (string, error) {
	dir := os.Getenv("XDG_RUNTIME_DIR")
	if dir == "" {
		return "", errors.New("no XDG_RUNTIME_DIR: the vault keeper's socket lives in the person's runtime directory")
	}
	return filepath.Join(dir, "beekeeper", "vault.sock"), nil
}

// ServeVault answers the keeper's socket at path until ctx ends.
func ServeVault(ctx context.Context, path string, k *Keeper) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if err := os.Chmod(filepath.Dir(path), 0o700); err != nil { //nolint:gosec // a directory: 0700 is the user's alone
		return err
	}
	_ = os.Remove(path)
	l, err := net.Listen("unix", path)
	if err != nil {
		return err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = l.Close()
		return err
	}
	go func() {
		<-ctx.Done()
		_ = l.Close()
	}()
	for {
		c, err := l.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		go answerVault(c, k)
	}
}

func answerVault(c net.Conn, k *Keeper) {
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	var st VaultState
	var req VaultRequest
	line, err := bufio.NewReaderSize(c, maxVaultRequest).ReadSlice('\n')
	switch {
	case err != nil:
		st.Error = "a request is one JSON line"
	case json.Unmarshal(line, &req) != nil:
		st.Error = "a request is one JSON line"
	case req.Op == VaultUnlock:
		if err := k.Unlock(req.Name, req.Token, time.Now()); err != nil {
			st.Error = err.Error()
		}
	case req.Op == VaultLock:
		k.Lock()
	case req.Op != VaultStatus:
		st.Error = fmt.Sprintf("unknown request %q", req.Op)
	}
	msg := st.Error
	st = k.State()
	st.Error = msg
	_ = json.NewEncoder(c).Encode(st)
}

// AskVault sends req to the keeper at path and returns its state. The
// listener must be this very binary, run as this user (Peer): a session
// goes to beekeeper's broker and nowhere else.
func AskVault(path string, req VaultRequest) (VaultState, error) {
	var st VaultState
	c, err := net.DialTimeout("unix", path, 5*time.Second)
	if err != nil {
		return st, fmt.Errorf("no vault keeper answers at %s: beekeeper-sandbox.service runs it (beekeeper install): %w", path, err)
	}
	defer func() { _ = c.Close() }()
	if err := Peer(c); err != nil {
		return st, fmt.Errorf("%s: %w: nothing sent", path, err)
	}
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	if err := json.NewEncoder(c).Encode(req); err != nil {
		return st, err
	}
	if err := json.NewDecoder(c).Decode(&st); err != nil {
		return st, fmt.Errorf("the vault keeper's answer: %w", err)
	}
	if st.Error != "" {
		return st, errors.New(st.Error)
	}
	return st, nil
}

// VaultWait is one call that waits on the person's unlock.
type VaultWait struct {
	Who   string    `json:"who"`
	Ref   string    `json:"ref"`
	Since time.Time `json:"since"`
}

// WaitsPath is the broker's list of waiting calls, next to the keeper's
// socket: the watch and beekeeper status read it, no sandboxed session
// writes it.
func WaitsPath() (string, error) {
	p, err := SocketPath()
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(p), "vault-waits.json"), nil
}

// ReadWaits reads the waiting calls; none when the file is absent.
func ReadWaits(path string) ([]VaultWait, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // the broker's own list
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var ws []VaultWait
	return ws, json.Unmarshal(raw, &ws)
}

// WriteWaits replaces the list of waiting calls, through a rename.
func WriteWaits(path string, ws []VaultWait) error {
	if ws == nil {
		ws = []VaultWait{}
	}
	raw, err := json.Marshal(ws)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return writeSecretFile(path, raw)
}

// StatePath is the broker's vault state, next to the keeper's socket:
// whether it holds a session, since and until when, never the session. The
// watch reads it.
func StatePath() (string, error) {
	p, err := SocketPath()
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(p), "vault-state.json"), nil
}

// ReadState reads the broker's vault state; locked when the file is absent.
func ReadState(path string) (VaultState, error) {
	var st VaultState
	raw, err := os.ReadFile(path) //nolint:gosec // the broker's own state
	if errors.Is(err, os.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return st, err
	}
	return st, json.Unmarshal(raw, &st)
}

// WriteState replaces the broker's vault state, through a rename.
func WriteState(path string, st VaultState) error {
	raw, err := json.Marshal(st)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return writeSecretFile(path, raw)
}
