package codex

import (
	"context"
	"errors"
	"os/exec"
	"testing"
	"time"
)

func TestProbeUsageProtocol(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c := exec.CommandContext(ctx, "sh", "-c", `
 read -r init
 case "$init" in *initialize*) ;; *) exit 1;; esac
 echo '{"id":1,"result":{}}'
 read -r ready
 case "$ready" in *initialized*) ;; *) exit 1;; esac
 read -r request
 case "$request" in *account/rateLimits/read*) ;; *) exit 1;; esac
 echo '{"method":"notification"}'
 echo '{"id":2,"result":{"rateLimits":{"primary":{"usedPercent":12,"windowDurationMins":300,"resetsAt":1793743200},"secondary":{"usedPercent":23,"windowDurationMins":10080,"resetsAt":1793829600}}}}'
 read -r keepalive
 `)
	at := time.Date(2026, 11, 3, 19, 0, 0, 0, time.UTC)
	r, err := probeUsage(c, at)
	if err != nil {
		t.Fatal(err)
	}
	if r.FiveHour.UsedPercent != 12 || r.SevenDay.UsedPercent != 23 || !r.At.Equal(at) {
		t.Fatalf("unexpected reading: %+v", r)
	}
}

func TestProbeUsageStops(t *testing.T) {
	for _, raw := range []string{
		`{"ordinaryUsageAllowed":false}`, `{"rateLimits":{"spendControlReached":true}}`,
		`{"rateLimits":{"rateLimitReached":true}}`, `{"rateLimits":{"rateLimitReachedType":"weekly"}}`,
	} {
		if _, err := parseProbeUsage([]byte(raw), time.Now()); !errors.Is(err, ErrUsageStopped) {
			t.Fatalf("%s: %v", raw, err)
		}
	}
}

func TestProbeUsageDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	c := exec.CommandContext(ctx, "sh", "-c", "read -r request; read -r more")
	if _, err := probeUsage(c, time.Now()); err == nil {
		t.Fatal("expected deadline error")
	}
}
