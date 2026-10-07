package secret_test

import (
	"context"
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/internal/secret"
	"github.com/giantswarm/beekeeper/internal/secret/secrettest"
)

// A handover reads every credential first and writes them as NAME=value
// lines: a reference that does not answer, a value no variable carries or a
// name that is none refuses the set, with nothing written and no value in
// the error.
func TestHandover(t *testing.T) {
	const second = "op://Shared/spark/credential"
	tools := secrettest.New(map[string]string{vaultRef: token, second: password, "op://Shared/empty/field": "", "op://Shared/lines/field": "two\nlines"})
	ref := func(s string) secret.Ref {
		r, err := secret.ParseRef(s)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	creds := []secret.Credential{{Name: "API_TOKEN", Ref: ref(vaultRef)}, {Name: "SPARK_API_KEY", Ref: ref(second)}}
	h, err := ops(tools).Handover(context.Background(), creds)
	if err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	if _, err := h.WriteTo(&out); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); got != "API_TOKEN="+token+"\nSPARK_API_KEY="+password+"\n" {
		t.Errorf("wrote %q", got)
	}
	for name, bad := range map[string][]secret.Credential{
		"unknown reference": {{Name: "A", Ref: ref(vaultRef)}, {Name: "B", Ref: ref("op://Shared/none/field")}},
		"no variable name":  {{Name: "not a name", Ref: ref(vaultRef)}},
		"a whole file":      {{Name: "A", Ref: secret.Ref{File: "x.sops.yaml"}}},
		"another vault":     {{Name: "A", Ref: ref("op://Other/item/field")}},
		"empty value":       {{Name: "A", Ref: ref("op://Shared/empty/field")}},
		"two lines":         {{Name: "A", Ref: ref("op://Shared/lines/field")}},
	} {
		h, err := ops(tools).Handover(context.Background(), bad)
		if err == nil || h != nil {
			t.Errorf("%s: %v, %v; want a refusal", name, h, err)
			continue
		}
		noValue(t, name, err.Error())
	}
}
