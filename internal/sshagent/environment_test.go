package sshagent

import (
	"net"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestEnvironment(t *testing.T) {
	dir := t.TempDir()
	live := filepath.Join(dir, "ssh-agent.socket")
	forwarded := filepath.Join(dir, "forwarded")
	for _, path := range []string{live, forwarded} {
		listener, err := net.Listen("unix", path)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = listener.Close() }()
	}
	regular := filepath.Join(dir, "regular")
	if err := os.WriteFile(regular, nil, 0600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, runtime, inherited, want string }{
		{"stale login", dir, filepath.Join(dir, "gone"), live},
		{"prefer session", dir, forwarded, live},
		{"missing inherited", dir, "", live},
		{"forwarded fallback", dir + "-missing", forwarded, forwarded},
		{"stale without fallback", dir + "-missing", filepath.Join(dir, "gone"), ""},
		{"regular file rejected", "", regular, ""},
		{"unset", "", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := []string{"PATH=/bin", "XDG_RUNTIME_DIR=" + tc.runtime, "SSH_AUTH_SOCK=" + tc.inherited}
			original := slices.Clone(env)
			got := Environment(env)
			if got[len(got)-1] != "SSH_AUTH_SOCK="+tc.want {
				t.Fatalf("socket entry = %q", got[len(got)-1])
			}
			if !slices.Equal(env, original) || got[0] != "PATH=/bin" {
				t.Fatal("environment mutated")
			}
		})
	}
}
