package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCheckServeIdentityInput(t *testing.T) {
	dir := t.TempDir()
	present, missing := filepath.Join(dir, "input"), filepath.Join(dir, "missing")
	if err := os.WriteFile(present, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, raw, want string
	}{
		{"default need not exist", "", ""},
		{"explicit regular file", fmt.Sprintf("serve: {identity: {tokenFile: %q}}", present), ""},
		{"explicit missing file", fmt.Sprintf("serve: {identity: {tokenFile: %q}}", missing), "referenced file does not exist"},
		{"explicit directory", fmt.Sprintf("serve: {identity: {tokenFile: %q}}", dir), "referenced path is not a regular file"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			findings, err := Check(writeConfig(t, tc.raw))
			if err != nil {
				t.Fatal(err)
			}
			if tc.want == "" {
				if len(findings) != 0 {
					t.Fatalf("Check = %v", findings)
				}
			} else if len(findings) != 1 || !strings.HasPrefix(findings[0], "serve.identity.tokenFile: "+tc.want) {
				t.Fatalf("Check = %v, want %s", findings, tc.want)
			}
		})
	}
}

func TestCheckServeIdentityHomePath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	input := filepath.Join(home, "input")
	if err := os.WriteFile(input, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	p := writeConfig(t, "serve: {identity: {tokenFile: ~/input}}")
	if findings, err := Check(p); err != nil || len(findings) != 0 {
		t.Fatalf("Check = %v, %v", findings, err)
	}
	c, err := Load(p)
	if err != nil || c.Serve.Identity.TokenFile != input {
		t.Fatalf("identity token path must resolve to the checked file: %v", err)
	}
}
