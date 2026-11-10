package secret_test

import (
	"context"
	"errors"
	"io"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/internal/secret"
)

const (
	moveAppRef        = "op://Personal/app"
	movePersonalVault = "Personal"
	moveTeamVault     = "Team Shared"
)

func TestMoveItemUsesAppAndDiscardsToolOutput(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "failure"}[fail], func(t *testing.T) {
			t.Setenv("OP_SESSION_test", "cached-session")
			t.Setenv("OP_SERVICE_ACCOUNT_TOKEN", "service-token")
			calls := 0
			o := &secret.Ops{Vault: movePersonalVault, MoveVaults: []string{moveTeamVault}, Env: []string{"OP_SESSION_broker=broker-session"}}
			o.Run = func(_ context.Context, _ string, env []string, in io.Reader, name string, args ...string) ([]byte, error) {
				calls++
				want := []string{opItemCommand, "move", "application", "--current-vault", movePersonalVault, "--destination-vault", moveTeamVault}
				if name != "op" || !reflect.DeepEqual(args, want) || in != nil {
					t.Fatalf("unexpected native move: %s %v", name, args)
				}
				settings := map[string]string{}
				for _, entry := range env {
					k, v, _ := strings.Cut(entry, "=")
					settings[k] = v
				}
				for _, k := range []string{"OP_SESSION_test", "OP_SESSION_broker", "OP_SERVICE_ACCOUNT_TOKEN", "OP_CONNECT_TOKEN", "OP_CONNECT_HOST"} {
					if v, ok := settings[k]; !ok || v != "" {
						t.Errorf("credential setting %s was not cleared", k)
					}
				}
				if !slices.Contains(env, "OP_BIOMETRIC_UNLOCK_ENABLED=true") {
					t.Error("desktop authentication not enabled")
				}
				if fail {
					return []byte(password), errors.New(password)
				}
				return []byte(password), nil
			}
			err := o.MoveItem(context.Background(), "op://Personal/application", moveTeamVault)
			if (err != nil) != fail || calls != 1 {
				t.Fatalf("move outcome: %v; calls %d", err, calls)
			}
			if err != nil && (strings.Contains(err.Error(), password) || errors.Unwrap(err) != nil) {
				t.Fatal("tool failure leaked")
			}
		})
	}
}

func TestMoveItemRejectsBeforeCallingOp(t *testing.T) {
	for _, tc := range []struct {
		source, destination string
		allowed             []string
		reason              string
	}{
		{moveAppRef, "Unlisted", []string{moveTeamVault}, "secret.moveVaults"},
		{moveAppRef, moveTeamVault, nil, "secret.moveVaults"},
		{"op://Unlisted/app", moveTeamVault, []string{moveTeamVault}, "secret.vaults"},
		{"op://Personal/app/password", moveTeamVault, []string{moveTeamVault}, "op://"},
		{"op://Personal/-", moveTeamVault, []string{moveTeamVault}, "item option"},
		{moveAppRef, movePersonalVault, []string{movePersonalVault}, "same vault"},
	} {
		t.Run(tc.source+tc.destination, func(t *testing.T) {
			o := &secret.Ops{Vault: movePersonalVault, MoveVaults: tc.allowed, Run: func(context.Context, string, []string, io.Reader, string, ...string) ([]byte, error) {
				t.Fatal("op ran for refused move")
				return nil, nil
			}}
			err := o.MoveItem(context.Background(), tc.source, tc.destination)
			if err == nil || !strings.Contains(err.Error(), tc.reason) {
				t.Fatalf("expected %s refusal, got %v", tc.reason, err)
			}
		})
	}
}
