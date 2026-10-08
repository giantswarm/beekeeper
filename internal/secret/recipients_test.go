package secret

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"filippo.io/age"
)

func TestCreationRuleRecipients(t *testing.T) {
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	other, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	r, o := id.Recipient().String(), other.Recipient().String()
	for name, tc := range map[string]struct {
		age  string
		want []string
	}{
		"one":        {r, []string{r}},
		"two":        {r + ", " + o + ",", []string{r, o}},
		"none":       {"", nil},
		"ssh":        {"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIExample", nil},
		"not an age": {r + ",age1notarecipient", nil},
	} {
		t.Run(name, func(t *testing.T) {
			if got := (creationRule{Age: tc.age}).recipients(); !slices.Equal(got, tc.want) {
				t.Errorf("recipients of %q = %v, want %v", tc.age, got, tc.want)
			}
		})
	}
}

func TestRecipients(t *testing.T) {
	id := isolateAge(t)
	other, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	r, o := id.Recipient().String(), other.Recipient().String()
	const graveler, glean = "management-clusters/graveler/.*", "management-clusters/glean/.*"
	dir := t.TempDir()
	rules := fmt.Sprintf("creation_rules:\n  - path_regex: %s\n    age: %s\n  - path_regex: %s\n    age: %s\n  - path_regex: '\\.sops\\.yaml$'\n    age: %s, %s\n    encrypted_regex: ^(data|stringData)$\n  - path_regex: 'plain/.*'\n", graveler, r, glean, o, r, o)
	if err := os.WriteFile(filepath.Join(dir, ".sops.yaml"), []byte(rules), 0o600); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(dir, "management-clusters", "glean")
	if err := os.MkdirAll(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	f := &itemTools{items: map[string]string{AgeItemTitle(r): id.String()}}
	ops := &Ops{Run: f.run, Vault: ageVault, Token: "t"}
	ctx := context.Background()

	rs, err := ops.Recipients(ctx, sub)
	if err != nil {
		t.Fatal(err)
	}
	item, none := fmt.Sprintf("the vault item %q", AgeItemTitle(r)), fmt.Sprintf("none: the vault Shared holds no item %q, whose password field is the AGE-SECRET-KEY-1… identity", AgeItemTitle(o))
	want := []Recipient{{graveler, r, item}, {glean, o, none}, {`\.sops\.yaml$`, r, item}, {`\.sops\.yaml$`, o, none}}
	if !slices.Equal(rs, want) {
		t.Errorf("recipients of the directory = %+v, want %+v", rs, want)
	}
	if !rs[1].Missing() || rs[0].Missing() {
		t.Errorf("Missing = %v, %v", rs[0].Missing(), rs[1].Missing())
	}
	if len(f.calls) != 1 || f.calls[0] != opItemList {
		t.Errorf("calls = %q: want the vault's item listing once, no read", f.calls)
	}
	raw, _ := json.Marshal(rs)
	if strings.Contains(string(raw), id.String()) || strings.Contains(string(raw), other.String()) {
		t.Fatal("the listing carries an identity")
	}

	// an absent file answers its creation rule's recipient, an encrypted
	// one its metadata's
	rs, err = ops.Recipients(ctx, filepath.Join(sub, "app.sops.yaml"))
	if err != nil || len(rs) != 1 || rs[0].Recipient != o || !rs[0].Missing() {
		t.Errorf("recipients of an absent file = %+v, %v", rs, err)
	}
	enc := sopsFileAt(t, filepath.Join(sub, "enc.sops.yaml"), "", r)
	rs, err = ops.Recipients(ctx, enc)
	if err != nil || len(rs) != 1 || rs[0] != (Recipient{enc, r, item}) {
		t.Errorf("recipients of an encrypted file = %+v, %v", rs, err)
	}

	// an entry of secret.ageIdentities names the source, the vault unasked
	f.calls = nil
	ops.Ages = []AgeIdentity{{Recipient: o, Ref: FileRef + "/keys/other.txt"}}
	rs, err = ops.Recipients(ctx, filepath.Join(dir, "management-clusters"))
	if err != nil || rs[1].Identity != "secret.ageIdentities (file:///keys/other.txt)" || rs[0].Identity != item {
		t.Errorf("recipients with an entry = %+v, %v", rs, err)
	}
	if len(f.calls) != 1 {
		t.Errorf("calls = %q", f.calls)
	}

	// one of sops' own sources
	t.Setenv(envAgeKey, other.String())
	rs, err = ops.Recipients(ctx, dir)
	if err != nil || rs[1].Identity != "sops' own sources" {
		t.Errorf("recipients with SOPS_AGE_KEY = %+v, %v", rs, err)
	}

	// without a vault the way to an identity is named
	rs, err = (&Ops{Run: f.run, Ages: ops.Ages}).Recipients(ctx, dir)
	if err != nil || !strings.Contains(rs[0].Identity, "none: no entry of secret.ageIdentities names it, and no shared vault (secret.vault)") {
		t.Errorf("recipients without a vault = %+v, %v", rs, err)
	}

	for name, path := range map[string]string{
		"a rule without a recipient": filepath.Join(dir, "plain", "x.yaml"),
		"no .sops.yaml":              t.TempDir(),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ops.Recipients(ctx, path); err == nil {
				t.Error("no error")
			}
		})
	}
}
