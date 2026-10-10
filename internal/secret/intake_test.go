package secret_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/secret"
	"github.com/giantswarm/beekeeper/internal/secret/secrettest"
)

const (
	storedValue = "planted-Stored-Value-3e9a1c"
	storeRef    = "op://Shared/app/client-secret"
	keyRef      = "op://Shared/app/private-key"
	// madeUpKey is shaped like a private key file; no key was ever in it.
	madeUpKey = "-----BEGIN RSA PRIVATE KEY-----\nplanted-key\n-----END RSA PRIVATE KEY-----\n" //nolint:gosec // no credential
)

// A store writes the field through op's stdin, creating the item and
// keeping its other fields, and answers length and fingerprint; it refuses
// what no field takes before op runs.
func TestStoreValueWritesTheFieldOffTheCommandLine(t *testing.T) {
	tools := secrettest.New(nil)
	o := &secret.Ops{Run: tools.Run, Vault: "Shared", Token: "sa-token", Fingerprint: func(v string) string { return fmt.Sprintf("fp-%d", len(v)) }}
	ctx := context.Background()
	s, err := o.StoreValue(ctx, secret.Ref{Op: storeRef}, storedValue)
	if err != nil || s != (secret.Stored{Ref: storeRef, Bytes: len(storedValue), Fingerprint: fmt.Sprintf("fp-%d", len(storedValue))}) {
		t.Fatalf("store = %+v, %v", s, err)
	}
	pem := madeUpKey
	if s, err := o.StoreValue(ctx, secret.Ref{Op: keyRef}, pem); err != nil || s.Bytes != len(pem) {
		t.Fatalf("a second field: %+v, %v", s, err)
	}
	if tools.Vault[storeRef] != storedValue || tools.Vault[keyRef] != pem {
		t.Errorf("the vault holds %q", tools.Vault)
	}
	if lines := strings.Join(tools.Calls, "\n"); strings.Contains(lines, storedValue) || strings.Contains(lines, "planted-key") {
		t.Errorf("a command line carries the value:\n%s", lines)
	}
	calls := len(tools.Calls)
	for _, bad := range []struct {
		ref  secret.Ref
		v    string
		want string
	}{
		{secret.Ref{File: aFile, Path: "x"}, storedValue, "op://<vault>/<item>/<field>"},
		{secret.Ref{Op: storeRef}, "", "empty"},
		{secret.Ref{Op: storeRef}, strings.Repeat("x", secret.MaxStoreBytes+1), "more than a field takes"},
		{secret.Ref{Op: "op://Other/app/x"}, storedValue, "only the shared vault"},
	} {
		if _, err := o.StoreValue(ctx, bad.ref, bad.v); err == nil || !strings.Contains(err.Error(), bad.want) {
			t.Errorf("%s with %d bytes: %v, want %q", bad.ref, len(bad.v), err, bad.want)
		}
	}
	if len(tools.Calls) != calls {
		t.Errorf("a refused store ran op: %q", tools.Calls[calls:])
	}
	if err := o.Writable(secret.Ref{Op: storeRef}); err != nil {
		t.Errorf("writable: %v", err)
	}
	if err := o.Writable(secret.Ref{File: aFile}); err == nil {
		t.Error("a SOPS path is writable")
	}
	if err := (&secret.Ops{Run: tools.Run}).Writable(secret.Ref{Op: storeRef}); !errors.Is(err, secret.ErrVault) {
		t.Errorf("without a vault: %v", err)
	}
}

// The clipboard is read trimmed and empty when nothing is copied, and
// cleared after the store; the newest download is found by its time, the
// file read within the field's size and shredded.
func TestClipboardAndTheFilesAValueComesFrom(t *testing.T) {
	ctx := context.Background()
	var calls []string
	empty := true
	run := func(_ context.Context, _ string, _ []string, _ io.Reader, name string, args ...string) ([]byte, error) {
		calls = append(calls, name+" "+strings.Join(args, " "))
		switch name {
		case "wl-paste":
			if empty {
				return nil, errors.New("wl-paste: exit 1 (Nothing is copied)")
			}
			return []byte("  " + storedValue + "\n"), nil
		case "wl-copy":
			empty = true
			return nil, nil
		}
		return nil, errors.New(name + ": exit 127 (executable file not found)")
	}
	if v, err := secret.Clipboard(ctx, run); err != nil || v != "" {
		t.Errorf("an empty clipboard: %q, %v", v, err)
	}
	empty = false
	if v, err := secret.Clipboard(ctx, run); err != nil || v != storedValue {
		t.Errorf("the clipboard: %q, %v", v, err)
	}
	if err := secret.ClearClipboard(ctx, run); err != nil || !empty || calls[len(calls)-1] != "wl-copy --clear" {
		t.Errorf("clear: %v, empty %v, %q", err, empty, calls)
	}
	missing := func(context.Context, string, []string, io.Reader, string, ...string) ([]byte, error) {
		return nil, errors.New("wl-paste: exit 127 (executable file not found)")
	}
	if _, err := secret.Clipboard(ctx, missing); err == nil || !strings.Contains(err.Error(), "wl-clipboard") {
		t.Errorf("without wl-paste: %v", err)
	}

	dir := t.TempDir()
	old := filepath.Join(dir, "old-app.2026-01-01.private-key.pem")
	key := filepath.Join(dir, "app.2026-10-10.private-key.pem")
	pem := madeUpKey
	for p, body := range map[string]string{old: "stale", key: pem, filepath.Join(dir, "empty.pem"): " \n", filepath.Join(dir, "large.pem"): strings.Repeat("x", secret.MaxStoreBytes+1)} {
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	past := time.Now().Add(-time.Hour)
	if err := os.Chtimes(old, past, past); err != nil {
		t.Fatal(err)
	}
	if f, err := secret.NewestFile(dir, "*.private-key.pem"); err != nil || f != key {
		t.Errorf("newest = %q, %v", f, err)
	}
	if f, err := secret.NewestFile(dir, "*.nothing"); err != nil || f != "" {
		t.Errorf("no match = %q, %v", f, err)
	}
	if v, err := secret.ReadValueFile(key); err != nil || v != pem {
		t.Errorf("read = %q, %v", v, err)
	}
	if _, err := secret.ReadValueFile(filepath.Join(dir, "empty.pem")); err == nil || !strings.Contains(err.Error(), "empty") {
		t.Errorf("an empty file: %v", err)
	}
	if _, err := secret.ReadValueFile(filepath.Join(dir, "large.pem")); err == nil || !strings.Contains(err.Error(), "more than a field takes") {
		t.Errorf("a large file: %v", err)
	}
	if err := secret.Shred(key); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(key); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the shredded file: %v", err)
	}
	if _, err := os.Stat(old); err != nil {
		t.Errorf("the other download: %v", err)
	}
}
