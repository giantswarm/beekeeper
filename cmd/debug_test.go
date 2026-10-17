package cmd

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/internal/sandbox"
)

// An unheld environment is dropped, and at debug level the client says so:
// a scratch-broker proof's client without SANDBOX_RUNTIME went local.
func TestUnconfineUnheldSaysSoAtDebugLevel(t *testing.T) {
	for _, tc := range []struct {
		name          string
		runtime       string
		debug         string
		dropped, says bool
	}{
		{"unheld, debug", "", "1", true, true},
		{"unheld, quiet", "", "", true, false},
		{"held by the runtime, debug", "1", "1", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(sandbox.Env, "1")
			t.Setenv(sandbox.Brokered, "")
			t.Setenv(sandbox.Runtime, tc.runtime)
			t.Setenv(debugEnv, tc.debug)
			t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
			var w bytes.Buffer
			unconfineUnheld(&w)
			if _, kept := os.LookupEnv(sandbox.Env); kept == tc.dropped {
				t.Errorf("%s kept = %v, want dropped = %v", sandbox.Env, kept, tc.dropped)
			}
			says := strings.Contains(w.String(), "beekeeper: debug: "+sandbox.Env+" set without "+sandbox.Runtime)
			if says != tc.says {
				t.Errorf("debug line %q, want said = %v", w.String(), tc.says)
			}
		})
	}
}
