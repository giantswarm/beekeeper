package secret_test

import (
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/internal/secret"
	"github.com/giantswarm/beekeeper/internal/secret/secrettest"
)

const (
	storeAttachedPassword  = "-apassword"
	storeUsernameAttribute = "username"
	storeAttributeOption   = "--attributes"
	storeProtectedOption   = "-s"
	storeDatabase          = "database"
	storeEntry             = "applications/service"
	credentialStoreRef     = "store://" + storeEntry //nolint:gosec // a reference, no credential
	storeReader            = "read-entry"
	storePasswordAttribute = "password"
)

func TestStoreReference(t *testing.T) {
	for _, spec := range []string{credentialStoreRef, credentialStoreRef + "#username", credentialStoreRef + "#custom attribute"} {
		r, err := secret.ParseRef(spec)
		if err != nil || r.String() != spec || !r.Single() || !r.IsSource() || r.File != "" {
			t.Fatalf("reference %q: %+v, %v", spec, r, err)
		}
	}
	for _, spec := range []string{"store://", "store://#password", credentialStoreRef + "#", credentialStoreRef + "#a#b", "store://-option", "store://entry\nother"} {
		if _, err := secret.ParseRef(spec); err == nil {
			t.Errorf("accepted invalid store source %q", spec)
		}
	}
	if !secret.NeedsStore([]string{"copy", credentialStoreRef + "=stringData.token", "out.yaml"}) || secret.NeedsStore([]string{"copy", "in.yaml#x", "--", "tool", credentialStoreRef}) {
		t.Fatal("store broker routing misclassified")
	}
}

func TestStoreSourcesCopyCompareFingerprint(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		field string
		argv  []string
		want  []string
	}{
		{"", []string{storeReader, storeDatabase}, []string{storeDatabase, storeEntry}},
		{storeUsernameAttribute, []string{storeReader, storeDatabase}, []string{storeDatabase, storeAttributeOption, storeUsernameAttribute, storeEntry}},
		{storeUsernameAttribute, []string{storeReader, "-a", storePasswordAttribute, storeDatabase}, []string{storeDatabase, storeAttributeOption, storeUsernameAttribute, storeEntry}},
		{storeUsernameAttribute, []string{storeReader, "--attributes=password", storeDatabase}, []string{storeDatabase, storeAttributeOption, storeUsernameAttribute, storeEntry}},
		{storeUsernameAttribute, []string{storeReader, storeAttachedPassword, storeDatabase}, []string{storeDatabase, storeAttributeOption, storeUsernameAttribute, storeEntry}},
		{storeUsernameAttribute, []string{storeReader, "-sa", storePasswordAttribute, storeDatabase}, []string{storeProtectedOption, storeDatabase, storeAttributeOption, storeUsernameAttribute, storeEntry}},
		{storeUsernameAttribute, []string{storeReader, "-sapassword", storeDatabase}, []string{storeProtectedOption, storeDatabase, storeAttributeOption, storeUsernameAttribute, storeEntry}},
		{storeUsernameAttribute, []string{storeReader, "-sk/path-containing-a", storeDatabase}, []string{"-sk/path-containing-a", storeDatabase, storeAttributeOption, storeUsernameAttribute, storeEntry}},
		{storeUsernameAttribute, []string{storeReader, "-k", storeAttachedPassword, storeDatabase}, []string{"-k", storeAttachedPassword, storeDatabase, storeAttributeOption, storeUsernameAttribute, storeEntry}},
		{storeUsernameAttribute, []string{storeReader, "--", storeDatabase}, []string{storeAttributeOption, storeUsernameAttribute, "--", storeDatabase, storeEntry}},

		{"custom attribute", []string{storeReader, "-a", "{field}", storeDatabase}, []string{"-a", "custom attribute", storeDatabase, storeEntry}},
		{"", []string{storeReader, "-a", "{field}"}, []string{"-a", storePasswordAttribute, storeEntry}},
	} {
		t.Run(tc.field+strings.Join(tc.argv, " "), func(t *testing.T) {
			tools := secrettest.New(nil)
			o := ops(tools)
			o.Vault, o.Token = "", ""
			o.Store.Read = slices.Clone(tc.argv)
			o.Run = func(ctx context.Context, dir string, env []string, in io.Reader, name string, args ...string) ([]byte, error) {
				if name != tc.argv[0] {
					return tools.Run(ctx, dir, env, in, name, args...)
				}
				if !slices.Equal(args, tc.want) {
					t.Fatalf("unexpected command %s %q", name, args)
				}
				return []byte(token + "\n"), nil
			}
			spec := credentialStoreRef
			if tc.field != "" {
				spec += "#" + tc.field
			}
			src, err := secret.ParseRef(spec)
			if err != nil {
				t.Fatal(err)
			}
			var delivered string
			o.Apply = func(_ context.Context, _ []byte, _ secret.KubeTarget, v []byte) error {
				delivered = string(v)
				return nil
			}
			n, err := o.CopyToSecret(ctx, src, secret.KubeTarget{Context: labContext, Namespace: kagentNS, Name: "store-copy", Key: "store-token"})
			if err != nil || n != len(token) || delivered != token {
				t.Fatalf("delivery length %d, matches %t, error %v", n, delivered == token, err)
			}
			prints, err := o.Fingerprints(ctx, src)
			if err != nil || len(prints) != 1 || prints[0].Fingerprint != o.Fingerprint(token) {
				t.Fatalf("fingerprint: %+v, %v", prints, err)
			}
			verdicts, err := o.Compare(ctx, src, src)
			if err != nil || len(verdicts) != 1 || verdicts[0].State != secret.Equal {
				t.Fatalf("compare: %+v, %v", verdicts, err)
			}
			if !slices.Equal(o.Store.Read, tc.argv) {
				t.Fatal("read configuration mutated")
			}
			if _, err := o.CopyValue(ctx, src, src); err == nil {
				t.Fatal("store accepted as destination")
			}
		})
	}
}

func TestStoreReadFailuresHideDiagnostics(t *testing.T) {
	for _, tc := range []struct {
		name, ref  string
		configured bool
		fail       bool
	}{
		{"unconfigured", credentialStoreRef, false, false},
		{"missing entry", "store://missing", true, true},
		{"missing field", credentialStoreRef + "#missing", true, true},
		{"command fails", credentialStoreRef, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := ops(secrettest.New(nil))
			if tc.configured {
				o.Store.Read = []string{storeReader}
			}
			o.Run = func(context.Context, string, []string, io.Reader, string, ...string) ([]byte, error) {
				return []byte(token), errors.New("stdout and stderr: " + token)
			}
			src, err := secret.ParseRef(tc.ref)
			if err != nil {
				t.Fatal(err)
			}
			_, err = o.Fingerprints(context.Background(), src)
			if err == nil || strings.Contains(err.Error(), token) || !strings.Contains(err.Error(), tc.ref) {
				t.Fatalf("unsafe or missing failure: %v", err)
			}
			if !tc.configured && !strings.Contains(err.Error(), "secret.store.read") {
				t.Fatalf("missing configuration key: %v", err)
			}
		})
	}
}

func TestStoreSourceConsumerRedactsOutput(t *testing.T) {
	o := ops(secrettest.New(nil))
	o.Store.Read = []string{storeReader}
	o.Run = func(context.Context, string, []string, io.Reader, string, ...string) ([]byte, error) {
		return []byte(token + "\n"), nil
	}
	src, err := secret.ParseRef(credentialStoreRef)
	if err != nil {
		t.Fatal(err)
	}
	code, out, err := o.CopyToConsumer(context.Background(), src, []string{"sh", "-c", "cat >&2", "--secret=store-token=-"}, secret.Stdin{})
	if err != nil || code != 0 || strings.Contains(out, token) || !strings.Contains(out, "[redacted: "+credentialStoreRef+"]") {
		t.Fatalf("consumer: exit %d, safe output %t, error %v", code, !strings.Contains(out, token), err)
	}
}
