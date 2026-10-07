package secret

import (
	"context"
	"errors"
	"testing"
)

// The cause of a failed sign-in: the command's bound and a missing command
// first, then the credential store's state whatever the command said, then
// the command's own words.
func TestClassifySignin(t *testing.T) {
	rejected := errors.New("op-unlock: exit status 1: op-unlock: 1Password rejected the password from entry 'x' (rotated? update the entry)")
	for _, tc := range []struct {
		name  string
		err   error
		store StoreState
		want  SigninCause
	}{
		{"the command's bound", context.DeadlineExceeded, StoreReady, SigninTimeout},
		{"killed by its unit", errors.New("systemd-run --user: signal: killed: "), StoreReady, SigninTimeout},
		{"no command", errors.New(`exec: "op-unlock": executable file not found in $PATH`), StoreUnknown, SigninCommand},
		{"a rejection with the store unlocked", rejected, StoreReady, SigninRejected},
		{"a rejection with no bus to ask", rejected, StoreUnknown, SigninRejected},
		{"a rejection with the store locked", rejected, StoreLocked, SigninStoreLocked},
		{"a rejection with no store up", rejected, StoreAbsent, SigninStoreAbsent},
		{"the network", errors.New("op-unlock: exit status 1: dial tcp: lookup my.1password.com: no such host"), StoreReady, SigninNetwork},
		{"the store by the command's words", errors.New("secret-tool: The name org.freedesktop.secrets was not provided by any .service files"), StoreUnknown, SigninStore},
		{"no known pattern", errors.New("op-unlock: exit status 2: no terminal to ask"), StoreReady, SigninUnknown},
	} {
		if got := ClassifySignin(tc.err, tc.store); got != tc.want {
			t.Errorf("%s: %s, want %s", tc.name, got, tc.want)
		}
	}
	if !SigninRejected.Rejected() || SigninStoreAbsent.Rejected() || SigninNetwork.Rejected() || SigninUnknown.Rejected() {
		t.Error("only a rejection is for the person")
	}
}
