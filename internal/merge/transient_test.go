package merge

import (
	"context"
	"errors"
	"fmt"
	"net"
	"syscall"
	"testing"
)

func TestTransient(t *testing.T) {
	for _, c := range []struct {
		err  error
		want bool
	}{
		{nil, false},
		{context.Canceled, false},
		{errors.New("HTTP 401: Bad credentials"), false},
		{errors.New("context gazelle: exit status 1: error: You must be logged in to the server (Unauthorized)"), false},
		{errors.New("parse helmreleases: unexpected end of JSON input"), false},
		{&net.DNSError{Err: "no such host", Name: "api.github.com"}, true},
		{fmt.Errorf("probe: %w", context.DeadlineExceeded), true},
		{fmt.Errorf("dial: %w", syscall.ECONNREFUSED), true},
		{fmt.Errorf("read: %w", syscall.ECONNRESET), true},
		{errors.New("Get \"https://api.github.com/rate_limit\": dial tcp: lookup api.github.com on 127.0.0.53:53: server misbehaving"), true},
		{errors.New("gh: error connecting to api.github.com\ncheck your internet connection or https://githubstatus.com"), true},
		{errors.New("dial tcp 140.82.121.5:443: i/o timeout"), true},
		{errors.New("net/http: TLS handshake timeout"), true},
		{errors.New("context gazelle: signal: killed: "), true},
		{errors.New("context gazelle: exit status 1: E1015 couldn't get current server API group list: context deadline exceeded"), true},
		{errors.New("context gazelle: exit status 1: Unable to connect to the server: read tcp 10.0.0.2:51234->1.2.3.4:443: read: connection reset by peer"), true},
		{errors.New("context gazelle: exit status 1: The connection to the server 127.0.0.1:9 was refused - did you specify the right host or port?"), true},
		{errors.New("ERROR: unable to connect to proxy teleport.giantswarm.io:443"), true},
		{errors.New("HTTP 502: Bad Gateway"), true},
	} {
		if got := Transient(c.err); got != c.want {
			t.Errorf("Transient(%v) = %v, want %v", c.err, got, c.want)
		}
	}
}
