//go:build !linux || nosystemd

package secret

import (
	"context"
	"time"

	"github.com/giantswarm/beekeeper/internal/platform"
)

// CredentialStore is the Secret Service on the session bus, a Linux
// desktop's; elsewhere the store cannot be asked.
func CredentialStore(context.Context) (StoreState, error) {
	return StoreUnknown, &platform.NotAvailableError{Part: "secret.CredentialStore"}
}

// Uptime reads /proc/uptime, a Linux file; elsewhere it is unknown.
func Uptime() (time.Duration, error) {
	return 0, &platform.NotAvailableError{Part: "secret.Uptime"}
}
