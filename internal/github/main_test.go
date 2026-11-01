package github

import (
	"os"
	"testing"

	"github.com/giantswarm/beekeeper/internal/testenv"
)

func TestMain(m *testing.M) {
	testenv.Setup()
	os.Exit(m.Run())
}
