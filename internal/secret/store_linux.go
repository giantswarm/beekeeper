//go:build linux && !nosystemd

package secret

import (
	"context"
	"errors"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/godbus/dbus/v5"

	"github.com/giantswarm/beekeeper/internal/platform"
)

// The freedesktop Secret Service on the session bus.
const (
	secretsName = "org.freedesktop.secrets"
	secretsPath = dbus.ObjectPath("/org/freedesktop/secrets")
)

// CredentialStore asks the session bus for the state of the person's
// credential store, without starting one: absent while nobody serves the
// Secret Service, locked while every collection is, ready with an unlocked
// collection. Unknown, with the error, where the bus cannot be asked.
func CredentialStore(ctx context.Context) (StoreState, error) {
	addr, err := platform.SessionBus()
	if err != nil {
		return StoreUnknown, err
	}
	conn, err := dbus.Connect(addr, dbus.WithContext(ctx))
	if err != nil {
		return StoreUnknown, err
	}
	defer func() { _ = conn.Close() }()
	var owned bool
	if err := conn.BusObject().CallWithContext(ctx, "org.freedesktop.DBus.NameHasOwner", 0, secretsName).Store(&owned); err != nil {
		return StoreUnknown, err
	}
	if !owned {
		return StoreAbsent, nil
	}
	var collections []dbus.ObjectPath
	if err := conn.Object(secretsName, secretsPath).StoreProperty("org.freedesktop.Secret.Service.Collections", &collections); err != nil {
		return StoreUnknown, err
	}
	for _, c := range collections {
		var locked bool
		if err := conn.Object(secretsName, c).StoreProperty("org.freedesktop.Secret.Collection.Locked", &locked); err != nil {
			return StoreUnknown, err
		}
		if !locked {
			return StoreReady, nil
		}
	}
	return StoreLocked, nil
}

// Uptime is how long the machine is up, from /proc/uptime.
func Uptime() (time.Duration, error) {
	b, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return 0, err
	}
	f, _, _ := strings.Cut(strings.TrimSpace(string(b)), " ")
	s, err := strconv.ParseFloat(f, 64)
	if err != nil {
		return 0, errors.New("no uptime in /proc/uptime")
	}
	return time.Duration(s * float64(time.Second)), nil
}
