package cmd

import (
	"context"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/giantswarm/beekeeper/internal/proc"
)

// watchResolver deliberately queries the same name through the stub and the
// configured uplinks. No public fallback resolver can establish uplink health.
type watchResolver interface {
	Servers(context.Context) ([]string, error)
	Lookup(context.Context, string) error
	Reset(context.Context) error
}

type resolverWatch struct {
	mu       sync.Mutex
	resolver watchResolver
	checked  time.Time
	suppress bool
}

// resolverFailure coalesces concurrent target failures for one watch interval.
// The reset and its verification finish before any caller prints its failure.
func (w *watcher) resolverFailure(line string) bool {
	// Only failed network readings are candidates; a report or notification
	// can quote a timeout without representing a failed watch reading.
	targetFailure := (strings.HasPrefix(line, "ALERTS ") && strings.Contains(line, " unreachable")) ||
		strings.HasPrefix(line, "PAGERDUTY unreachable") ||
		strings.HasPrefix(line, "CENTRAL UNREACHABLE ") ||
		strings.HasPrefix(line, "GitHub budget unknown:") ||
		(strings.HasPrefix(line, "UPGRADES ") && strings.Contains(line, " unreadable:"))
	if !targetFailure {
		return false
	}
	r := &w.resolverState
	r.mu.Lock()
	defer r.mu.Unlock()
	if time.Since(r.checked) < 30*time.Second {
		return r.suppress
	}
	r.checked, r.suppress = time.Now(), false
	resolver := r.resolver
	if resolver == nil {
		resolver = systemWatchResolver{}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	servers, err := resolver.Servers(ctx)
	if err != nil || len(servers) == 0 {
		return false
	}
	if resolver.Lookup(ctx, "127.0.0.53") == nil {
		return false
	}
	r.suppress = true
	upstream := false
	for _, server := range servers {
		if resolver.Lookup(ctx, server) == nil {
			upstream = true
			break
		}
	}
	if !upstream {
		w.emitNow("resolver", "DNS OUTAGE: local stub and upstream DNS failed; no reset attempted")
		return true
	}
	result := "reset succeeded"
	if err := resolver.Reset(ctx); err != nil {
		result = "reset failed"
	}
	if resolver.Lookup(ctx, "127.0.0.53") == nil {
		result += "; stub answers afterwards"
	} else {
		result += "; stub still failing afterwards"
	}
	w.emitNow("resolver", "LOCAL RESOLVER: local resolver wedged, upstream DNS fine; %s", result)
	return true
}

type systemWatchResolver struct{}

func (systemWatchResolver) Servers(context.Context) ([]string, error) {
	stub, err := os.ReadFile("/etc/resolv.conf")
	if err != nil {
		return nil, err
	}
	usingStub := false
	for _, line := range strings.Split(string(stub), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[0] == "nameserver" && fields[1] == "127.0.0.53" {
			usingStub = true
		}
	}
	if !usingStub {
		return nil, nil
	}
	// systemd-resolved maintains this file with the configured per-link DNS
	// servers, rather than its loopback stub.
	data, err := os.ReadFile("/run/systemd/resolve/resolv.conf")
	if err != nil {
		return nil, err
	}
	var servers []string
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[0] == "nameserver" {
			ip := net.ParseIP(fields[1])
			if ip != nil && !ip.IsLoopback() {
				servers = append(servers, fields[1])
			}
		}
	}
	return servers, nil
}

func (systemWatchResolver) Lookup(ctx context.Context, server string) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	r := &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, net.JoinHostPort(server, "53"))
	}}
	// The root NS query works without choosing a target's private DNS name.
	records, err := r.LookupNS(ctx, ".")
	if err == nil && len(records) == 0 {
		return fmt.Errorf("empty DNS answer")
	}
	return err
}

func (systemWatchResolver) Reset(ctx context.Context) error {
	return proc.CommandContext(ctx, "busctl", "call", "org.freedesktop.resolve1", "/org/freedesktop/resolve1", "org.freedesktop.resolve1.Manager", "ResetServerFeatures").Run()
}
