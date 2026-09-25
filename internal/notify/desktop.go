package notify

import (
	"context"
	"errors"
	"os"
	"path/filepath"

	"github.com/godbus/dbus/v5"
)

// Message is one desktop notification.
type Message struct {
	Summary string
	Body    string
	// Urgency is low, normal or critical.
	Urgency string
}

// Sender delivers a message and returns the id the notification service
// gave it.
type Sender interface {
	Send(ctx context.Context, m Message) (uint32, error)
}

// Desktop sends to org.freedesktop.Notifications on the session bus. It
// connects on the first message and again after a failed one, so a service
// that starts later is found.
type Desktop struct {
	conn *dbus.Conn
}

const (
	service = "org.freedesktop.Notifications"
	object  = "/org/freedesktop/Notifications"
)

// urgencies are the Desktop Notifications Specification's urgency levels.
var urgencies = map[string]byte{Low: 0, Normal: 1, Critical: 2}

// Send calls Notify with the urgency hint and the server's default timeout.
func (d *Desktop) Send(ctx context.Context, m Message) (uint32, error) {
	if d.conn == nil {
		addr, err := sessionBus()
		if err != nil {
			return 0, err
		}
		c, err := dbus.Connect(addr, dbus.WithContext(ctx))
		if err != nil {
			return 0, err
		}
		d.conn = c
	}
	hints := map[string]dbus.Variant{"urgency": dbus.MakeVariant(urgencies[m.Urgency])}
	var id uint32
	err := d.conn.Object(service, object).CallWithContext(ctx, service+".Notify", 0,
		"beekeeper", uint32(0), "", m.Summary, m.Body, []string{}, hints, int32(-1)).Store(&id)
	if err != nil {
		_ = d.Close()
	}
	return id, err
}

// Close drops the connection.
func (d *Desktop) Close() error {
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
