package secret

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestStdinRejectsNullBeforeSideEffects(t *testing.T) {
	for _, template := range []string{"null", " \t\nnull \r\n"} {
		t.Run(template, func(t *testing.T) {
			in := Stdin{Template: template, Field: "password"}
			const want = "--stdin-json: no JSON object"
			check := func(t *testing.T, err error) {
				t.Helper()
				if err == nil || err.Error() != want {
					t.Fatalf("error = %v, want %q", err, want)
				}
			}
			t.Run("Check", func(t *testing.T) { check(t, in.Check()) })
			t.Run("input", func(t *testing.T) {
				got, err := in.input("test-value")
				check(t, err)
				if got != "" {
					t.Fatalf("input = %q, want empty", got)
				}
			})
			for _, operation := range []string{"CopyToConsumer", "Set"} {
				t.Run(operation, func(t *testing.T) {
					calls := 0
					o := &Ops{Run: func(context.Context, string, []string, io.Reader, string, ...string) ([]byte, error) {
						calls++
						return nil, errors.New("unexpected Runner call")
					}}
					dst := Ref{File: filepath.Join(t.TempDir(), "destination.sops.yaml"), Path: agePasswordPath}
					consumer := []string{"sh", "-c", "exit 99", "--password-stdin"}
					var err error
					if operation == "CopyToConsumer" {
						_, _, err = o.CopyToConsumer(context.Background(), Ref{Op: "op://Shared/app/password"}, consumer, in)
					} else {
						_, err = o.Set(context.Background(), dst, SetOptions{Length: 32, Charset: "alnum", Consumer: consumer, Stdin: in})
					}
					check(t, err)
					if calls != 0 {
						t.Errorf("Runner called %d times", calls)
					}
					if _, err := os.Stat(dst.File); !errors.Is(err, os.ErrNotExist) {
						t.Errorf("destination stat = %v, want absent", err)
					}
				})
			}
		})
	}
}
