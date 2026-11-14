package cmd

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeWatchResolver struct {
	stubOK, upstreamOK, recovered    bool
	resetErr                         error
	resets, stubReads, upstreamReads int
}

func (f *fakeWatchResolver) Servers(context.Context) ([]string, error) {
	return []string{"192.0.2.1"}, nil
}
func (f *fakeWatchResolver) Lookup(_ context.Context, server string) error {
	ok := f.upstreamOK
	if server == "127.0.0.53" {
		f.stubReads++
		ok = f.stubOK || (f.resets > 0 && f.recovered)
	} else {
		f.upstreamReads++
	}
	if ok {
		return nil
	}
	return errors.New("DNS timeout")
}
func (f *fakeWatchResolver) Reset(context.Context) error { f.resets++; return f.resetErr }

func TestWatchResolverDiscrimination(t *testing.T) {
	for _, tc := range []struct {
		name              string
		resolver          fakeWatchResolver
		want              string
		resets, stubReads int
	}{
		{"wedge repaired", fakeWatchResolver{upstreamOK: true, recovered: true}, "local resolver wedged, upstream DNS fine; reset succeeded; stub answers afterwards", 1, 2},
		{"wedge persists", fakeWatchResolver{upstreamOK: true}, "local resolver wedged, upstream DNS fine; reset succeeded; stub still failing afterwards", 1, 2},
		{"reset denied", fakeWatchResolver{upstreamOK: true, resetErr: errors.New("denied")}, "local resolver wedged, upstream DNS fine; reset failed; stub still failing afterwards", 1, 2},
		{"outage", fakeWatchResolver{}, "DNS OUTAGE: local stub and upstream DNS failed; no reset attempted", 0, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, out := onceWatch(t, nil)
			w.ResolverState.Resolver = &tc.resolver
			// Independent readers arrive concurrently, as in a running watch.
			var wg sync.WaitGroup
			for _, line := range []string{"ALERTS fixture unreachable: timed out", "PAGERDUTY unreachable: timed out", "GitHub budget unknown: timed out"} {
				wg.Go(func() { w.EmitNow("fixture", "%s", line) })
			}
			wg.Wait()
			got := out.String()
			if strings.Count(got, "\n") != 1 || !strings.Contains(got, tc.want) {
				t.Fatalf("output %q, want one line containing %q", got, tc.want)
			}
			if tc.resolver.resets != tc.resets || tc.resolver.stubReads != tc.stubReads || tc.resolver.upstreamReads != 1 {
				t.Fatalf("calls: reset=%d stub=%d upstream=%d", tc.resolver.resets, tc.resolver.stubReads, tc.resolver.upstreamReads)
			}
			// A new interval probes again instead of retaining a stale diagnosis.
			tc.resolver.stubOK = true
			w.ResolverState.Checked = time.Now().Add(-time.Minute)
			w.EmitNow("fixture", "ALERTS fixture unreachable: service down")
			if !strings.Contains(out.String(), "ALERTS fixture unreachable: service down") {
				t.Fatal("healthy DNS still suppressed a target failure")
			}
		})
	}
}

func TestWatchResolverHealthyStubKeepsTargetFailure(t *testing.T) {
	w, out := onceWatch(t, nil)
	f := &fakeWatchResolver{stubOK: true, upstreamOK: true}
	w.ResolverState.Resolver = f
	w.EmitNow("alerts", "ALERTS fixture unreachable: service down")
	if !strings.Contains(out.String(), "ALERTS fixture unreachable: service down") || f.resets != 0 || f.upstreamReads != 0 {
		t.Fatalf("output %q, resolver %+v", out.String(), f)
	}
}

func TestWatchResolverLeavesUnrelatedEventsAlone(t *testing.T) {
	w, out := onceWatch(t, nil)
	f := &fakeWatchResolver{}
	w.ResolverState.Resolver = f
	w.EmitNow("report", "WORKER REPORT: service unreachable, i/o timeout")
	if !strings.Contains(out.String(), "WORKER REPORT:") || f.stubReads != 0 {
		t.Fatalf("unrelated event probed DNS: %q", out.String())
	}
}
