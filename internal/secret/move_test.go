package secret_test

import (
	"context"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/internal/secret"
)

func TestMoveItemUsesAppAndDiscardsToolOutput(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "failure"}[fail], func(t *testing.T) {
			t.Setenv("OP_SESSION_test", "cached-session")
			t.Setenv("OP_SERVICE_ACCOUNT_TOKEN", "service-token")
			calls := 0
			o := &secret.Ops{Vault: "Personal", MoveVaults: []string{"Team Shared"}, Env: []string{"OP_SESSION_broker=broker-session"}}
			o.Run = func(_ context.Context, _ string, env []string, in io.Reader, name string, args ...string) ([]byte, error) {
				calls++
				want := []string{"item", "move", "application", "--current-vault", "Personal", "--destination-vault", "Team Shared"}
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
				if settings["OP_BIOMETRIC_UNLOCK_ENABLED"] != "true" {
					t.Error("desktop authentication not enabled")
				}
				if fail {
					return []byte(password), errors.New(password)
				}
				return []byte(password), nil
			}
			err := o.MoveItem(context.Background(), "op://Personal/application", "Team Shared")
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
		{"op://Personal/app", "Unlisted", []string{"Team Shared"}, "secret.moveVaults"},
		{"op://Personal/app", "Team Shared", nil, "secret.moveVaults"},
		{"op://Unlisted/app", "Team Shared", []string{"Team Shared"}, "secret.vaults"},
		{"op://Personal/app/password", "Team Shared", []string{"Team Shared"}, "op://"},
		{"op://Personal/-", "Team Shared", []string{"Team Shared"}, "item option"},
		{"op://Personal/app", "Personal", []string{"Personal"}, "same vault"},
	} {
		t.Run(tc.source+tc.destination, func(t *testing.T) {
			o := &secret.Ops{Vault: "Personal", MoveVaults: tc.allowed, Run: func(context.Context, string, []string, io.Reader, string, ...string) ([]byte, error) {
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
