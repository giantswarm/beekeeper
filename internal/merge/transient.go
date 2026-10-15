package merge

import (
	"context"
	"errors"
	"io"
	"net"
	"regexp"
	"syscall"
)

// transientWords are the error texts of a read that failed in transit, as
// gh, kubectl and tsh print them: the resolver, a refused, reset or
// unreachable connection, a TLS handshake or round trip that timed out, a
// read killed at its deadline, and a gateway's 5xx.
var transientWords = regexp.MustCompile(`(?i)no such host|server misbehaving|temporary failure in name resolution|` +
	`could not resolve|i/o timeout|connection refused|connection to the server \S+ was refused|connection reset|broken pipe|network is unreachable|no route to host|` +
	`tls handshake timeout|unexpected eof|error connecting to|deadline exceeded|client\.timeout exceeded|signal: killed|` +
	`couldn't get current server api group list|unable to connect to proxy|proxy .*(?:unreachable|unavailable)|` +
	`\b50[234]\b|bad gateway|service unavailable|gateway timeout`)

// Transient reports whether err is a read that failed in transit and may
// answer when sent again: a timeout, a resolver error, a refused or reset
// connection, an EOF mid-answer or a gateway's 5xx. An answer that refuses
// (no login, no access, a parse error) is not.
func Transient(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) {
		return false
	}
	var ne net.Error
	var dns *net.DNSError
	switch {
	case errors.As(err, &dns), errors.As(err, &ne) && ne.Timeout(),
		errors.Is(err, context.DeadlineExceeded), errors.Is(err, io.ErrUnexpectedEOF),
		errors.Is(err, syscall.ECONNREFUSED), errors.Is(err, syscall.ECONNRESET):
		return true
	}
	return transientWords.MatchString(err.Error())
}
