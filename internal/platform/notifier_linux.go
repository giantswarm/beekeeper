//go:build linux && !nosystemd

package platform

import (
	"context"
	"errors"
	"os"
	"path/filepath"

	"github.com/godbus/dbus/v5"

	"github.com/giantswarm/beekeeper/internal/notify"
)

// desktop sends to org.freedesktop.Notifications on the session bus. It
// connects on the first message and again after a failed one, so a service
// that starts later is found.
type desktop struct {
	conn *dbus.Conn
}

const (
	service = "org.freedesktop.Notifications"
	object  = "/org/freedesktop/Notifications"
)

// urgencies are the Desktop Notifications Specification's urgency levels.
var urgencies = map[string]byte{notify.Low: 0, notify.Normal: 1, notify.Critical: 2}

// Send calls Notify with the urgency hint and the server's default timeout.
// A connection the bus closed underneath (a bus restart) is replaced once
// within the send, so the message is not lost.
func (d *desktop) Send(ctx context.Context, m notify.Message) (uint32, error) {
	id, closed, err := d.notify(ctx, m)
	if closed {
		id, _, err = d.notify(ctx, m)
	}
	return id, err
}

// notify is one Notify call, on the open connection or a new one; a failed
// call drops the connection and reports whether it failed because the
// connection was closed. A connection closed since the last call is replaced
// first: godbus cancels its context synchronously on close, while a call on
// it may fail with the reader's error rather than dbus.ErrClosed.
func (d *desktop) notify(ctx context.Context, m notify.Message) (uint32, bool, error) {
	if d.conn != nil && !d.conn.Connected() {
		_ = d.Close()
	}
	if d.conn == nil {
		c, err := connect()
		if err != nil {
			return 0, false, err
		}
		d.conn = c
	}
	hints := map[string]dbus.Variant{"urgency": dbus.MakeVariant(urgencies[m.Urgency])}
	var id uint32
	err := d.conn.Object(service, object).CallWithContext(ctx, service+".Notify", 0,
		"beekeeper", uint32(0), "", m.Summary, m.Body, []string{}, hints, int32(-1)).Store(&id)
	if err != nil {
		closed := errors.Is(err, dbus.ErrClosed) || !d.conn.Connected()
		_ = d.Close()
		return id, closed, err
	}
	return id, false, nil
}

// connect opens the connection a desktop keeps. It takes no send's
// context: godbus closes a connection when the context it was opened with
// ends, and each send's context ends with the send.
func connect() (*dbus.Conn, error) {
	addr, err := sessionBus()
	if err != nil {
		return nil, err
	}
	return dbus.Connect(addr)
}

// Close drops the connection.
func (d *desktop) Close() error {
	if d.conn == nil {
		return nil
	}
	err := d.conn.Close()
	d.conn = nil
	return err
}

// sessionBus is the session bus address: DBUS_SESSION_BUS_ADDRESS, else the
// user bus socket in XDG_RUNTIME_DIR. Unlike the library's default it never
// autolaunches a bus through dbus-launch.
func sessionBus() (string, error) {
	if a := os.Getenv("DBUS_SESSION_BUS_ADDRESS"); a != "" {
		return a, nil
	}
	if dir := os.Getenv("XDG_RUNTIME_DIR"); dir != "" {
		return "unix:path=" + filepath.Join(dir, "bus"), nil
	}
	return "", errors.New("no session bus: neither DBUS_SESSION_BUS_ADDRESS nor XDG_RUNTIME_DIR is set")
}
