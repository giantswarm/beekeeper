package knownhosts

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

func TestParseableDropsInvalidHostEntries(t *testing.T) {
	good := hostLine(t, "git.example")
	_, _, goodKey, _, _, err := ssh.ParseKnownHosts([]byte(good))
	if err != nil {
		t.Fatal(err)
	}
	tests := map[string]string{
		"unknown marker":       "@unknown " + good,
		"invalid hash":         hostLine(t, "|1|bad|bad"),
		"unsupported hash":     hostLine(t, "|2|c2FsdA==|aGFzaA=="),
		"invalid bracket host": hostLine(t, "[broken"),
		"empty negation":       hostLine(t, "!"),
		"unicode comment":      "\u00a0# comment",
		"long entry":           good + " " + strings.Repeat("x", 64*1024),
		"long comment":         "#" + strings.Repeat("x", 1024*1024),
	}
	for name, bad := range tests {
		t.Run(name, func(t *testing.T) {
			for _, withUnknownKey := range []bool{false, true} {
				label := "alone"
				if withUnknownKey {
					label = "with unknown key"
				}
				t.Run(label, func(t *testing.T) {
					dir := t.TempDir()
					src, dst := filepath.Join(dir, "source"), filepath.Join(dir, "out")
					lines := []string{good, bad}
					wantDropped := 1
					if withUnknownKey {
						lines = append(lines, unknownLine("pq.example"))
						wantDropped++
					}
					write(t, src, lines...)
					if _, err := knownhosts.New(src); err == nil {
						t.Fatal("fixture must be rejected by the consumer's parser")
					}
					dropped, err := Parseable(dst, src)
					if err != nil || dropped != wantDropped {
						t.Fatalf("dropped %d, err %v; want %d, nil", dropped, err, wantDropped)
					}
					cb, err := knownhosts.New(dst)
					if err != nil {
						t.Fatal(err)
					}
					if err := cb("git.example:22", &addr{}, goodKey); err != nil {
						t.Fatalf("valid host was lost: %v", err)
					}
					b, err := os.ReadFile(src) //nolint:gosec // synthetic temp fixture
					if err != nil || string(b) != strings.Join(lines, "\n")+"\n" {
						t.Fatalf("source changed: %v", err)
					}
				})
			}
		})
	}
}
