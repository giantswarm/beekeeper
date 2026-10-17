package secret_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/secret"
	"github.com/giantswarm/beekeeper/internal/secret/secrettest"
)

// The listing answers titles and field labels with their lengths, never a
// value, also in its JSON form.
func TestItemsAnswerTitlesFieldsAndLengthsOnly(t *testing.T) {
	tools := secrettest.New(map[string]string{
		"op://Dev Common/Backstage OpenAI/credential": password,
		"op://Dev Common/Backstage OpenAI/username":   "svc",
		"op://Dev Common/graveler.agekey/notesPlain":  token,
		vaultRef: token,
	})
	o := ops(tools)
	o.AgeVaults = []string{"Dev Common"}
	all, err := o.Items(context.Background(), "Dev Common", secret.ItemsOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 || all[0].Title != "Backstage OpenAI" || all[1].Title != "graveler.agekey" || all[0].Fields != nil {
		t.Errorf("items = %+v", all)
	}
	got, err := o.Items(context.Background(), "Dev Common", secret.ItemsOptions{Match: "openai", Fields: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || len(got[0].Fields) != 2 {
		t.Fatalf("matched items = %+v", got)
	}
	lengths := map[string]int{}
	for _, f := range got[0].Fields {
		lengths[f.Label] = f.Length
	}
	if lengths["credential"] != len(password) || lengths["username"] != 3 {
		t.Errorf("field lengths = %v", lengths)
	}
	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range []string{password, token, "svc"} {
		if strings.Contains(string(raw), v) {
			t.Errorf("the listing carries a value: %s", raw)
		}
	}
}

// A vault outside secret.vault, secret.vaults and secret.ageVaults is
// refused before op runs, naming the vaults listed.
func TestItemsRefuseAVaultOutsideTheAllowlist(t *testing.T) {
	tools := secrettest.New(map[string]string{otherVaultRef: password})
	o := ops(tools)
	o.Vaults, o.AgeVaults = []string{"Team"}, []string{"Dev Common"}
	_, err := o.Items(context.Background(), "Private", secret.ItemsOptions{Fields: true})
	if err == nil || !strings.Contains(err.Error(), `["Shared" "Team" "Dev Common"]`) {
		t.Errorf("another vault = %v", err)
	}
	for _, v := range []string{"", "Shared/api"} {
		if _, err := o.Items(context.Background(), v, secret.ItemsOptions{}); err == nil {
			t.Errorf("vault %q listed", v)
		}
	}
	o.Token = ""
	if _, err := o.Items(context.Background(), shared, secret.ItemsOptions{}); !errors.Is(err, secret.ErrVault) {
		t.Errorf("no service account = %v", err)
	}
	if len(tools.Calls) != 0 {
		t.Errorf("op ran: %q", tools.Calls)
	}
}

// Every op call of a listing has its own deadline: a vault of many items
// is bounded per read, never by one deadline over the whole listing.
func TestItemsBoundEachReadOnItsOwn(t *testing.T) {
	tools := secrettest.New(map[string]string{vaultRef: token, "op://Shared/b/credential": password, "op://Shared/c/credential": password})
	o := ops(tools)
	var deadlines []time.Time
	o.Run = func(ctx context.Context, dir string, env []string, stdin io.Reader, name string, args ...string) ([]byte, error) {
		d, ok := ctx.Deadline()
		if !ok {
			t.Errorf("%s %v runs without a deadline", name, args)
		}
		deadlines = append(deadlines, d)
		return tools.Run(ctx, dir, env, stdin, name, args...)
	}
	if _, err := o.Items(context.Background(), shared, secret.ItemsOptions{Fields: true}); err != nil {
		t.Fatal(err)
	}
	if len(deadlines) != 4 {
		t.Fatalf("op calls = %d, want the listing and three reads", len(deadlines))
	}
	for i := 1; i < len(deadlines); i++ {
		if !deadlines[i].After(deadlines[i-1]) {
			t.Errorf("call %d shares an earlier call's deadline", i)
		}
	}
}
