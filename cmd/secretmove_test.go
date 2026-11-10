package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/sandbox"
)

func TestSecretMoveOutputAndAuditNeverContainToolValues(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprint(fail), func(t *testing.T) {
			a, _, _ := secretApp(t)
			a.cfg.Secret.MoveVaults = []string{"Team Shared"}
			a.cfg.Secret.TokenFile = "/must-not-read-service-token"
			t.Setenv(sandbox.Brokered, "1")
			secretRun = func(context.Context, string, []string, io.Reader, string, ...string) ([]byte, error) {
				if fail {
					return []byte(secretValue), errors.New(secretValue)
				}
				return []byte(secretValue), nil
			}
			out, err := runSecret(a, secretMoveOp, "op://Shared/db", "Team Shared")
			if (err != nil) != fail {
				t.Fatalf("move outcome: %v", err)
			}
			noSecret(t, "move output", fmt.Sprint(out, err))
			events, err := a.store.Events(0, nil)
			if err != nil {
				t.Fatal(err)
			}
			noSecret(t, "move audit", fmt.Sprint(events))
			if !strings.Contains(fmt.Sprint(events), "secret.move") {
				t.Fatal("missing move audit")
			}
		})
	}
}

func TestMoveBypassesVaultKeeperAndIsNeverRetried(t *testing.T) {
	a := vaultApp(t, time.Minute)
	calls := 0
	// A nil keeper deliberately proves that even a locked session is untouched.
	h := a.brokeredVault(nil, func(env []string) sandbox.Handler {
		if len(env) != 0 {
			t.Fatal("cached session handed to move")
		}
		return func(context.Context, int, sandbox.Request) (sandbox.Reply, error) {
			calls++
			return sandbox.Reply{Code: ExitVault, Err: "session expired"}, nil
		}
	})
	req := sandbox.Request{Op: sandbox.OpSecret, Args: []string{secretMoveOp, "op://Shared/db", "Team Shared"}}
	if err := brokeredSecretArgs(req.Args, true, false); err != nil {
		t.Fatal(err)
	}
	if _, err := h(context.Background(), 1, req); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("move ran %d times", calls)
	}
}

func TestSecretMoveRequiresBrokerWithoutSessionMode(t *testing.T) {
	a, _, _ := secretApp(t)
	t.Setenv(sandbox.Brokered, "")
	t.Setenv(sandbox.Env, "")
	a.cfg.Secret.MoveVaults = []string{"Team Shared"}
	secretRun = func(context.Context, string, []string, io.Reader, string, ...string) ([]byte, error) {
		t.Fatal("move ran in caller instead of broker")
		return nil, nil
	}
	_, err := runSecret(a, secretMoveOp, "op://Shared/db", "Team Shared")
	if err == nil || !strings.Contains(err.Error(), "no sandbox broker") {
		t.Fatalf("expected broker requirement: %v", err)
	}
}
