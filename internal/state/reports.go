package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/giantswarm/beekeeper/internal/clock"
)

// ReportMessage is a durable command from a session to the supervisor. The
// filename orders completed submissions; the receipt commits with the effects.
type ReportMessage struct {
	ID      string          `json:"id"`
	By      Party           `json:"by"`
	At      time.Time       `json:"at"`
	Kind    string          `json:"kind"`
	Payload json.RawMessage `json:"payload"`
}

// ReportReceipt survives a crash between committing effects and removing the
// inbox file. Note is also the stable resolution of a pending note reference.
type ReportReceipt struct {
	At       time.Time `json:"at"`
	Note     int       `json:"note,omitempty"`
	Error    string    `json:"error,omitempty"`
	Notified bool      `json:"notified,omitempty"`
	By       Party     `json:"by"`
}

const reportsDir = "session-reports"

// QueueReport publishes a synced message without opening state.lock. The final
// rename and directory sync precede acknowledgement to the session.
func QueueReport(store Store, by Party, kind string, payload any) (ReportMessage, error) {
	m := ReportMessage{ID: fmt.Sprintf("%020d-%s", clock.Now().UnixNano(), uuid.NewString()), By: by, At: clock.Now().UTC(), Kind: kind}
	var err error
	m.Payload, err = json.Marshal(payload)
	if err != nil {
		return m, err
	}
	dir := filepath.Join(store.Dir(), reportsDir)
	if err = os.MkdirAll(dir, 0o700); err != nil {
		return m, err
	}
	if err = syncDirectory(store.Dir()); err != nil {
		return m, err
	}
	return m, writeJSON(filepath.Join(dir, m.ID+".json"), m)
}

// PendingReports returns completed publications in filename order. Partial
// temporary files are invisible. A corrupt publication is an error, not a skip.
func PendingReports(store Store) ([]ReportMessage, error) {
	dir := filepath.Join(store.Dir(), reportsDir)
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	var out []ReportMessage
	for _, e := range entries {
		if e.IsDir() || strings.HasPrefix(e.Name(), ".") || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		// Confine reads to the inbox, including when an entry is a symlink.
		raw, err := root.ReadFile(e.Name())
		if err != nil {
			return nil, err
		}
		var m ReportMessage
		if err = json.Unmarshal(raw, &m); err != nil {
			return nil, err
		}
		if m.ID+".json" != e.Name() {
			return nil, fmt.Errorf("invalid report filename %q", e.Name())
		}
		out = append(out, m)
	}
	return out, nil
}

// RemoveReport is called only after a durable receipt. A replay after a crash
// finds that receipt and removes the file without repeating its effects.
func RemoveReport(store Store, m ReportMessage) error {
	if filepath.Base(m.ID) != m.ID || m.ID == "." {
		return errors.New("invalid report id")
	}
	dir := filepath.Join(store.Dir(), reportsDir)
	if err := os.Remove(filepath.Join(dir, m.ID+".json")); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return syncDirectory(dir)
}
