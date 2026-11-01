package execution

import (
	"os"
	"testing"

	"github.com/giantswarm/beekeeper/internal/testenv"
)

func TestMain(m *testing.M) {
	// The streaming fake harness receives an explicit execution identity from
	// the server. Dispatch it before isolating the parent test suite.
	if os.Getenv("BEEKEEPER_FAKE_STREAM") == "1" {
		os.Exit(m.Run())
	}
	testenv.Setup()
	os.Exit(m.Run())
}
