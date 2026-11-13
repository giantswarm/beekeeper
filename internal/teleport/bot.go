package teleport

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"os"
	"path/filepath"

	"github.com/giantswarm/beekeeper/internal/proc"
)

// BotStatus reads only the public certificate emitted by tbot. It never reads
// the bot's private key or onboarding configuration.
func BotStatus(certificate string) (Profile, error) {
	raw, err := os.ReadFile(filepath.Clean(certificate))
	if err != nil {
		return Profile{}, errors.New("bot certificate unavailable")
	}
	for len(raw) > 0 {
		block, rest := pem.Decode(raw)
		if block == nil {
			break
		}
		raw = rest
		if block.Type != "CERTIFICATE" {
			continue
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			break
		}
		return Profile{Username: cert.Subject.CommonName, ValidUntil: cert.NotAfter}, nil
	}
	return Profile{}, errors.New("bot certificate invalid")
}

// BotRunning checks supervision without exposing logs, tokens or diagnostics.
func BotRunning(ctx context.Context) error {
	if err := proc.CommandContext(ctx, "systemctl", "--user", "is-active", "--quiet", "beekeeper-teleport.service").Run(); err != nil {
		return errors.New("tbot user service is not active; inspect beekeeper-teleport.service")
	}
	return nil
}
