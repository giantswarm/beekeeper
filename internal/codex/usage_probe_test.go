package codex

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
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

// The deadline ends the probe even while a child of the app-server keeps its
// standard output open.
func TestProbeUsageDeadlineWithAChildHoldingStdout(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	c := exec.CommandContext(ctx, "sh", "-c", "sleep 5; true")
	c.WaitDelay = time.Second // as ProbeUsage sets it
	start := time.Now()
	if _, err := probeUsage(c, time.Now()); err == nil {
		t.Fatal("expected deadline error")
	}
	if took := time.Since(start); took > 3*time.Second {
		t.Fatalf("the probe outlived its deadline by %s", took)
	}
}

// A refused account usage request names its endpoint, status and error
// code, never its body; only a request that may pass is sent again.
func TestUsageRequestErrorNamesTheCause(t *testing.T) {
	for _, tc := range []struct {
		name, reply, want string
		transient         bool
	}{
		{"401 with a code", `{"code":-32603,"message":"failed to fetch codex rate limits: GET https://chatgpt.com/backend-api/wham/usage?account=x failed: 401 Unauthorized; content-type=application/json; body={\"error\":{\"message\":\"secret account text\",\"code\":\"token_expired\"}}"}`,
			"GET https://chatgpt.com/backend-api/wham/usage answered 401 Unauthorized (token_expired)", false},
		{"403 with a detail code", `{"code":-32603,"message":"GET https://chatgpt.com/backend-api/wham/usage failed: 403 Forbidden; content-type=application/json; body={\"detail\":{\"code\":\"account_deactivated\",\"email\":\"secret@example.com\"}}"}`,
			"GET https://chatgpt.com/backend-api/wham/usage answered 403 Forbidden (account_deactivated)", false},
		{"502 with a page", `{"code":-32603,"message":"failed to fetch codex rate limits: GET https://chatgpt.com/backend-api/wham/usage failed: 502 Bad Gateway; content-type=text/html; body=<html>secret account text</html>"}`,
			"GET https://chatgpt.com/backend-api/wham/usage answered 502 Bad Gateway", true},
		{"429", `{"code":-32603,"message":"failed to fetch codex rate limits: GET https://chatgpt.com/backend-api/wham/usage failed: 429 Too Many Requests; content-type=; body="}`,
			"GET https://chatgpt.com/backend-api/wham/usage answered 429 Too Many Requests", true},
		{"unanswered", `{"code":-32603,"message":"failed to fetch codex rate limits: error sending request for url (https://chatgpt.com/backend-api/wham/usage)"}`,
			"error sending request for url (https://chatgpt.com/backend-api/wham/usage)", true},
		{"no login", `{"code":-32600,"message":"codex account authentication required to read rate limits"}`,
			"codex account authentication required to read rate limits", false},
		{"no message", `{"code":-32603}`, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := usageRequestError([]byte(tc.reply))
			if err.Cause != tc.want || err.Transient != tc.transient || UsageRequestTransient(err) != tc.transient {
				t.Fatalf("cause %q (transient %t), want %q (transient %t)", err.Cause, err.Transient, tc.want, tc.transient)
			}
			if strings.Contains(err.Error(), "secret") || !strings.HasPrefix(err.Error(), "codex account usage request failed") {
				t.Fatalf("error %q", err.Error())
			}
		})
	}
	long := usageRequestError([]byte(`{"message":"` + strings.Repeat("é", 200) + `"}`))
	if len(long.Cause) > maxUsageCause+len("…") || !utf8.ValidString(long.Cause) {
		t.Fatalf("long cause: %d bytes, valid %t", len(long.Cause), utf8.ValidString(long.Cause))
	}
}

// The probe hands the app-server's error reply on as the cause.
func TestProbeUsageReturnsTheRefusal(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c := exec.CommandContext(ctx, "sh", "-c", `
 read -r init
 echo '{"id":1,"result":{}}'
 read -r ready
 read -r request
 echo '{"id":2,"error":{"code":-32603,"message":"failed to fetch codex rate limits: GET https://chatgpt.com/backend-api/wham/usage failed: 401 Unauthorized; content-type=; body={\"code\":\"token_expired\"}"}}'
 read -r keepalive
 `)
	_, err := probeUsage(c, time.Now())
	var re *UsageRequestError
	if !errors.As(err, &re) || re.Cause != "GET https://chatgpt.com/backend-api/wham/usage answered 401 Unauthorized (token_expired)" {
		t.Fatalf("refusal: %v", err)
	}
}
