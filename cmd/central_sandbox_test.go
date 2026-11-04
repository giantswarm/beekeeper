package cmd

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/central"
	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/state"
)

func TestCentralSandboxExit(t *testing.T) {
	t.Setenv("BEEKEEPER_SANDBOX", "1")
	t.Setenv("BEEKEEPER_SANDBOX_BROKERED", "")
	a := &app{centralClient: central.New(config.Central{Context: "sandbox-context", Timeout: config.Duration{Duration: time.Second}}, func(context.Context, ...string) ([]byte, error) {
		t.Fatal("sandboxed command invoked muster")
		return nil, nil
	})}
	_, err := a.callCentral(context.Background(), state.Party{}, "hold_list", map[string]any{}, nil)
	var exit *exitError
	if !errors.As(err, &exit) || exit.code != ExitCentral || !strings.Contains(err.Error(), "central access is host-only") {
		t.Fatalf("got %v, want host-only error with exit %d", err, ExitCentral)
	}
}
