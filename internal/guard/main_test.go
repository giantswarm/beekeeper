package guard

import (
	"os"
	"testing"

	"github.com/giantswarm/beekeeper/internal/testenv"
)

func TestMain(m *testing.M) {
	testenv.Setup()
	os.Exit(m.Run())
}

// Shared fixtures of the guard tests: two lab resources and the hook
// event's session key.
const (
	labA       = "lab-a"
	labB       = "lab-b"
	sessionKey = "session_id"
)
