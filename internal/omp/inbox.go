package omp

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
)

// An omp agent beekeeper starts runs `omp --mode rpc`, omp's JSON-lines
// protocol on stdin and stdout, with its stdin on a FIFO of its own: its
// inbox. A message is one steer command written to the inbox: omp delivers
// it at the agent's next tool round while a turn runs, and starts a turn
// with it while the agent is idle. A message to an agent whose process is
// gone is refused (ErrNotRunning): nothing reads its inbox.

// ErrNotRunning is a message to an agent whose inbox no process reads.
var ErrNotRunning = errors.New("no omp process reads its inbox")

// InboxPath is the inbox of the agent started under id, in beekeeper's
// state folder.
func InboxPath(stateDir, id string) string {
	return filepath.Join(stateDir, "omp", id+".in")
}

// command is the rpc command a message is sent as.
type command struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

// steerLine is the inbox line that delivers msg.
func steerLine(msg string) ([]byte, error) {
	b, err := json.Marshal(command{Type: "steer", Message: strings.TrimSpace(msg)})
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

