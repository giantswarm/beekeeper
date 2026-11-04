package secret

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConsumerRefusesAmbiguousKubectlContext(t *testing.T) {
	for _, flags := range []string{
		"--context lab --context production", "--context=lab --context=production", "--context lab --context=production", "--context=lab --context production",
		"--namespace --context lab", "--container --context=lab", "--unknown --context lab", "--context lab --stdin=false",
	} {
		t.Run(flags, func(t *testing.T) {
			argv := strings.Fields("kubectl exec -i " + flags + " pod -- gh secret set EXAMPLE")
			if err := Consumer(argv); err == nil {
				t.Fatal("ambiguous consumer accepted")
			}
			calls := 0
			o := &Ops{Vault: "Shared", Token: "synthetic-token", Run: func(context.Context, string, []string, io.Reader, string, ...string) ([]byte, error) {
				calls++
				return nil, errors.New("unexpected tool")
			}}
			if _, _, err := o.CopyToConsumer(context.Background(), Ref{Op: "op://Shared/example/password"}, argv, Stdin{}); err == nil {
				t.Fatal("copy accepted ambiguous context")
			}
			file := filepath.Join(t.TempDir(), "new.sops.yaml")
			if _, err := o.Set(context.Background(), Ref{File: file, Path: "password"}, SetOptions{Literal: "synthetic-value", Consumer: argv}); err == nil {
				t.Fatal("set accepted ambiguous context")
			}
			if calls != 0 {
				t.Fatal("ambiguous context reached a source or tool")
			}
			if _, err := os.Stat(file); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("ambiguous context wrote destination")
			}
		})
	}
	for _, flags := range []string{"--context lab -n team -c app", "--namespace=team --context=lab", "--context lab --request-timeout 10s --quiet"} {
		argv := strings.Fields("kubectl exec -i " + flags + " pod -- gh secret set EXAMPLE")
		if err := Consumer(argv); err != nil || ConsumerContext(argv) != "lab" {
			t.Fatalf("ordinary context refused: %v", err)
		}
	}
}
