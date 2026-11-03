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

// Shared fixtures of the guard tests: two lab resources, the hook
// event's session key and the agy harness name.
const (
	agyHarness = "agy"
	labA       = "lab-a"
	labB       = "lab-b"
	sessionKey = "session_id"
)
