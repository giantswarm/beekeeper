package central

import (
	"net/http"
	"testing"
)

// SetBaseTransport makes tr what each call's transport is cloned from, for
// the test.
func SetBaseTransport(t *testing.T, tr *http.Transport) {
	t.Helper()
	prev := baseTransport
	baseTransport = tr
	t.Cleanup(func() { baseTransport = prev })
}
