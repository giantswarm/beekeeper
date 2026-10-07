package secret

import (
	"context"
	"errors"
	"os/exec"
	"regexp"
	"strings"
)

// The broker's sign-in through secret.signinCommand fails for reasons that
// pass by themselves, seconds after a boot above all: the person's
// credential store is not up or unlocked yet, the network is not, the
// command's own bound. The keeper retries those with a backoff; only a
// password 1Password rejects while the store answered, which no retry
// mends, is for the person.

// StoreState is the state of the person's credential store on the session
// bus (the freedesktop Secret Service: KeePassXC, GNOME Keyring, KWallet),
// as CredentialStore reads it.
type StoreState int

const (
	// StoreUnknown is a store the keeper cannot ask: no session bus, or a
	// probe that failed.
	StoreUnknown StoreState = iota
	// StoreAbsent is a session bus nobody serves the Secret Service on:
	// the store's program is not up, or the store is no Secret Service.
	StoreAbsent
	// StoreLocked is a Secret Service whose every collection is locked.
	StoreLocked
	// StoreReady is a Secret Service with an unlocked collection.
	StoreReady
)

func (s StoreState) String() string {
	switch s {
	case StoreAbsent:
		return "no credential store answers on the session bus"
	case StoreLocked:
		return "the credential store is locked"
	case StoreReady:
		return "the credential store is unlocked"
	}
	return "the credential store cannot be asked"
}

// SigninCause is why a sign-in failed, as the keeper tells it from the
// command's error and the credential store's state when the command ran.
type SigninCause int

const (
	// SigninUnknown is a failure with no known pattern.
	SigninUnknown SigninCause = iota
	// SigninRejected is a password 1Password rejected while the credential
	// store answered: the entry the command reads is wrong or rotated.
	SigninRejected
	// SigninStoreLocked is a credential store that was locked.
	SigninStoreLocked
	// SigninStoreAbsent is a session bus with no credential store on it.
	SigninStoreAbsent
	// SigninStore is a credential store the command could not read, by the
	// command's own words.
	SigninStore
	// SigninNetwork is a network that did not reach 1Password.
	SigninNetwork
	// SigninTimeout is a command that did not finish within its bound.
	SigninTimeout
	// SigninCommand is a command that could not run at all.
	SigninCommand
)

func (c SigninCause) String() string {
	switch c {
	case SigninRejected:
		return "1Password rejected the password"
	case SigninStoreLocked:
		return StoreLocked.String()
	case SigninStoreAbsent:
		return StoreAbsent.String()
	case SigninStore:
		return "the credential store answered no password"
	case SigninNetwork:
		return "the network did not reach 1Password"
	case SigninTimeout:
		return "the sign-in did not finish in time"
	case SigninCommand:
		return "the sign-in command could not run"
	}
	return "the sign-in command failed"
}

// Rejected reports whether the cause is one for the person: a rejection
// that no retry mends.
func (c SigninCause) Rejected() bool { return c == SigninRejected }

var (
	// rejectedWords is a password 1Password did not take, in op's words and
	// in a helper's.
	rejectedWords = regexp.MustCompile(`(?i)rejected the password|incorrect password|password is incorrect|wrong password|invalid (?:password|credentials)|unauthorized|\b401\b|authentication failed`)
	// storeWords is a credential store the command could not read.
	storeWords = regexp.MustCompile(`(?i)secret-tool|keyring|secret service|org\.freedesktop\.secrets|not provided by any \.service|cannot autolaunch d-bus|session bus|credential store|no such secret|no password|empty password|is locked|database locked`)
	// networkWords is a network that did not reach 1Password.
	networkWords = regexp.MustCompile(`(?i)no such host|network is unreachable|no route to host|connection refused|connection reset|dial tcp|i/o timeout|tls handshake|name resolution|could not resolve|unable to connect|dns`)
)

// ClassifySignin tells why a sign-in failed: from err, the command's
// failure, and store, the credential store's state when it ran. A store
// that was locked or absent is the cause whatever the command said: a
// helper that read no password reports what 1Password said to the empty
// one. A rejection counts with the store unlocked, or unknown.
func ClassifySignin(err error, store StoreState) SigninCause {
	msg := err.Error()
	switch {
	case errors.Is(err, context.DeadlineExceeded) || strings.Contains(msg, "context deadline exceeded") || strings.Contains(msg, "signal: killed"):
		return SigninTimeout
	case errors.Is(err, exec.ErrNotFound) || strings.Contains(msg, "executable file not found") || strings.Contains(msg, "no such file or directory"):
		return SigninCommand
	case store == StoreLocked:
		return SigninStoreLocked
	case store == StoreAbsent:
		return SigninStoreAbsent
	case networkWords.MatchString(msg):
		return SigninNetwork
	case rejectedWords.MatchString(msg):
		return SigninRejected
	case storeWords.MatchString(msg):
		return SigninStore
	}
	return SigninUnknown
}
