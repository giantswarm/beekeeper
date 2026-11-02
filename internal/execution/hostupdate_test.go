package execution

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestUpdateHostRunsSelfUpdateAndChecksTheAnswer(t *testing.T) {
	f := testFixture(t)
	digest := Digest([]byte("prepared"))
	for _, tc := range []struct {
		name   string
		answer any
		runErr error
		ok     bool
	}{
		{"pinned", HostUpdate{Protocol: Protocol, Identity: fixtureHost, Version: "v0.6.0", PolicyDigest: digest}, nil, true},
		{"busy", HostUpdate{Protocol: Protocol, Identity: fixtureHost, Version: "v0.5.0", Busy: 1}, nil, true},
		{"failed", HostUpdate{Protocol: Protocol, Identity: fixtureHost, Error: "no signed release"}, nil, false},
		{"other host", HostUpdate{Protocol: Protocol, Identity: "host-z", Version: "v0.6.0", PolicyDigest: digest}, nil, false},
		{"other release", HostUpdate{Protocol: Protocol, Identity: fixtureHost, Version: "v0.5.0", PolicyDigest: digest}, nil, false},
		{"no digest", HostUpdate{Protocol: Protocol, Identity: fixtureHost, Version: "v0.6.0"}, nil, false},
		{"older host", map[string]any{"error": "unknown command"}, nil, false},
		{"unreachable", nil, errors.New("connection refused"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var argv []string
			run := func(_ context.Context, a []string, stdin []byte) ([]byte, error) {
				argv = a
				if tc.runErr != nil {
					return nil, tc.runErr
				}
				return json.Marshal(tc.answer)
			}
			x, err := UpdateHost(context.Background(), f.host, "v0.6.0", run)
			if (err == nil) != tc.ok {
				t.Fatalf("UpdateHost() = %+v, %v", x, err)
			}
			want := quote(f.host.Executable) + " --config " + quote(f.host.Config) + " agents hosts self-update --protocol 1 '--version' 'v0.6.0'"
			if len(argv) == 0 || argv[len(argv)-1] != want || argv[len(argv)-2] != f.host.SSH {
				t.Errorf("ran %q, want the host's self-update %q", argv, want)
			}
			var down *UnreachableError
			if tc.runErr != nil && !errors.As(err, &down) {
				t.Errorf("an SSH failure is %T, want unreachable", err)
			}
			if tc.name == "failed" && !strings.Contains(err.Error(), "no signed release") {
				t.Errorf("the host's reason is lost: %v", err)
			}
		})
	}
}

func TestQuiesceHoldsAdmissionAndCountsRunningExecutions(t *testing.T) {
	f := testFixture(t)
	ctx := context.Background()
	prev := quiesceWait
	quiesceWait = 50 * time.Millisecond
	t.Cleanup(func() { quiesceWait = prev })
	busy, release, err := f.s.Quiesce(ctx)
	if err != nil || busy != 0 {
		t.Fatalf("Quiesce() = %d, %v", busy, err)
	}
	if _, _, err := f.s.Quiesce(ctx); err == nil {
		t.Fatal("admission was taken twice")
	}
	during, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	if _, err := f.s.Start(during, f.r); err == nil || !strings.Contains(err.Error(), "admission") {
		t.Fatalf("a start during an update: %v", err)
	}
	release()
	if _, err := f.s.Start(ctx, f.r); err != nil {
		t.Fatal(err)
	}
	busy, release, err = f.s.Quiesce(ctx)
	if err != nil || busy != 1 {
		t.Fatalf("Quiesce() with a started execution = %d, %v", busy, err)
	}
	release()
}

func TestHostCatalogMayLeaveTheDigestToTheState(t *testing.T) {
	f := testFixture(t)
	h := f.host
	h.PolicyDigest = ""
	if err := h.ValidateCatalog(); err != nil {
		t.Fatalf("an entry without a digest: %v", err)
	}
	if err := h.Validate(); err == nil {
		t.Fatal("a host without a pinned digest is admitted to a call")
	}
	h.PolicyDigest = "sha256:x"
	if err := h.ValidateCatalog(); err == nil {
		t.Fatal("an entry with an invalid digest")
	}
}
