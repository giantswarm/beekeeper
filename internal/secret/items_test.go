package secret_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/secret"
	"github.com/giantswarm/beekeeper/internal/secret/secrettest"
)

const opItemCommand = "item"

const itemsCredential = "credential"

const itemsCreate = "create"

const itemsPasswordKind = "PASSWORD"

const itemsUsername = "username"

const itemsNotes = "notesPlain"

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
	if lengths[itemsCredential] != len(password) || lengths[itemsUsername] != 3 {
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
	var doneChannels []<-chan struct{}
	var mu sync.Mutex
	o.Run = func(ctx context.Context, dir string, env []string, stdin io.Reader, name string, args ...string) ([]byte, error) {
		_, ok := ctx.Deadline()
		if !ok {
			t.Errorf("%s %v runs without a deadline", name, args)
		}
		if err := ctx.Err(); err != nil {
			t.Errorf("%s %v starts with a cancelled context: %v", name, args, err)
		}
		mu.Lock()
		doneChannels = append(doneChannels, ctx.Done())
		mu.Unlock()
		return tools.Run(ctx, dir, env, stdin, name, args...)
	}
	if _, err := o.Items(context.Background(), shared, secret.ItemsOptions{Fields: true}); err != nil {
		t.Fatal(err)
	}
	if len(doneChannels) != 4 {
		t.Fatalf("op calls = %d, want the listing and three reads", len(doneChannels))
	}
	// Independent timeout contexts have distinct cancellation channels, even
	// when calls start in the same clock tick and their deadlines are equal.
	seen := make(map[<-chan struct{}]bool)
	for i, done := range doneChannels {
		if done == nil {
			t.Errorf("call %d has no cancellation channel", i)
		} else if seen[done] {
			t.Errorf("call %d shares an earlier call's timeout context", i)
		}
		seen[done] = true
	}
}

// Hundreds of reads overlap, stay bounded, and never return env values.
func TestItemsKeyPatternAcrossLargeVault(t *testing.T) {
	values := map[string]string{}
	for i := 0; i < 772; i++ {
		values[fmt.Sprintf("op://Shared/item%03d/notesPlain", i)] = "OTHER=hidden-other\nSERVICE_API_KEY=hidden-target"
	}
	tools := secrettest.New(values)
	o := ops(tools)
	var active, peak atomic.Int32
	o.Run = func(ctx context.Context, dir string, env []string, stdin io.Reader, name string, args ...string) ([]byte, error) {
		if len(args) > 1 && args[0] == opItemCommand && args[1] == "get" {
			n := active.Add(1)
			defer active.Add(-1)
			for p := peak.Load(); n > p; p = peak.Load() {
				if peak.CompareAndSwap(p, n) {
					break
				}
			}
			select {
			case <-time.After(10 * time.Millisecond):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		return tools.Run(ctx, dir, env, stdin, name, args...)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	got, err := o.Items(ctx, shared, secret.ItemsOptions{EnvKeys: true, Fields: true, KeyPattern: "api_key"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 772 {
		t.Fatalf("got %d items", len(got))
	}
	if p := peak.Load(); p < 2 || p > 8 {
		t.Fatalf("concurrent reads = %d", p)
	}
	for i, item := range got {
		if item.Title != fmt.Sprintf("item%03d", i) || len(item.EnvKeys) != 1 || item.EnvKeys[0].Key != "SERVICE_API_KEY" {
			t.Fatalf("unexpected item: %+v", item)
		}
	}
	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "hidden-") {
		t.Fatal("listing exposed a value")
	}
	missing, err := o.Items(context.Background(), shared, secret.ItemsOptions{EnvKeys: true, KeyPattern: "hidden-target"})
	if err != nil || len(missing) != 0 {
		t.Fatalf("matched values: %d items, %v", len(missing), err)
	}
}

func TestItemsKeyPatternRequiresEnvKeys(t *testing.T) {
	tools := secrettest.New(nil)
	_, err := ops(tools).Items(context.Background(), shared, secret.ItemsOptions{KeyPattern: "KEY"})
	if err == nil || len(tools.Calls) != 0 {
		t.Fatalf("invalid options: %v, calls %v", err, tools.Calls)
	}
}

func TestItemsParallelReadFailureReturnsNoResults(t *testing.T) {
	tools := secrettest.New(map[string]string{vaultRef: token})
	o := ops(tools)
	o.Run = func(ctx context.Context, dir string, env []string, stdin io.Reader, name string, args ...string) ([]byte, error) {
		if len(args) > 1 && args[1] == "get" {
			return nil, errors.New("read failed")
		}
		return tools.Run(ctx, dir, env, stdin, name, args...)
	}
	got, err := o.Items(context.Background(), shared, secret.ItemsOptions{EnvKeys: true})
	if !errors.Is(err, secret.ErrVault) || got != nil {
		t.Fatalf("result = %v, %v", got, err)
	}
}

// Sectioned fixtures exercise the same metadata and references as op, including
// custom fields whose labels coincide with built-in IDs, across an item edit.
func TestItemsFakePreservesFieldSections(t *testing.T) {
	const base = "op://Shared/widget/"
	values := map[string]string{
		"first/notes": "FIRST=one\n", "second/notes": "SECOND=two\n",
		"first/username": "CUSTOM=three\n", itemsUsername: "BUILTIN=four\n",
		itemsNotes: "PLAIN=five\n", itemsCredential: "original",
	}
	tools := secrettest.New(nil)
	tools.Types = map[string]string{}
	wantFields := map[string]secret.FieldInfo{}
	wantKeys := map[string]secret.EnvKey{}
	for name, value := range values {
		typ, purpose := "STRING", ""
		if name == itemsCredential {
			typ = concealedType
		}
		if name == itemsUsername {
			purpose = "USERNAME"
		}
		if name == itemsNotes {
			purpose = "NOTES"
		}
		tools.Vault[base+name], tools.Types[base+name] = value, typ
		section, label, found := strings.Cut(name, "/")
		if !found {
			section, label = "", name
		}
		wantFields[name] = secret.FieldInfo{Label: label, Section: section, Type: typ, Purpose: purpose, Length: len(value)}
		if typ == "STRING" {
			key, val, _ := strings.Cut(strings.TrimSpace(value), "=")
			wantKeys[name] = secret.EnvKey{Field: name, Key: key, Length: len(val)}
		}
	}
	o := ops(tools)
	ctx := context.Background()
	check := func() {
		t.Helper()
		items, err := o.Items(ctx, shared, secret.ItemsOptions{Fields: true, EnvKeys: true})
		if err != nil {
			t.Fatal(err)
		}
		if len(items) != 1 {
			t.Fatalf("items = %+v", items)
		}
		fields := map[string]secret.FieldInfo{}
		for _, f := range items[0].Fields {
			name := f.Label
			if f.Section != "" {
				name = f.Section + "/" + name
			}
			fields[name] = f
		}
		keys := map[string]secret.EnvKey{}
		for _, k := range items[0].EnvKeys {
			keys[k.Field] = k
		}
		if !reflect.DeepEqual(fields, wantFields) || len(items[0].Fields) != len(wantFields) {
			t.Errorf("fields = %+v, want %+v", fields, wantFields)
		}
		if !reflect.DeepEqual(keys, wantKeys) || len(items[0].EnvKeys) != len(wantKeys) {
			t.Errorf("env keys = %+v, want %+v", keys, wantKeys)
		}
	}
	check()
	if _, err := o.StoreFields(ctx, shared, "widget", []secret.ItemField{{Label: itemsCredential, Value: "updated"}}); err != nil {
		t.Fatal(err)
	}
	values[itemsCredential] = "updated"
	f := wantFields[itemsCredential]
	f.Length = len(values[itemsCredential])
	wantFields[itemsCredential] = f
	if len(tools.Vault) != len(values) || len(tools.Types) != len(values) {
		t.Errorf("edit changed field references: values=%v types=%v", tools.Vault, tools.Types)
	}
	for name, value := range values {
		if tools.Vault[base+name] != value || tools.Types[base+name] != wantFields[name].Type {
			t.Errorf("edit changed %s: value=%q type=%q", name, tools.Vault[base+name], tools.Types[base+name])
		}
	}
	check()
}

// Written items keep their category through listing, reading and editing.
func TestItemsMatchCategoryAfterCreateAndEdit(t *testing.T) {
	tools := secrettest.New(nil)
	o := ops(tools)
	ctx := context.Background()
	for _, phase := range []string{itemsCreate, "edit"} {
		t.Run(phase, func(t *testing.T) {
			if _, err := o.StoreFields(ctx, shared, "widget", []secret.ItemField{{Label: itemsCredential, Value: "synthetic-" + phase}}); err != nil {
				t.Fatal(err)
			}
			got, err := o.Items(ctx, shared, secret.ItemsOptions{Match: "password", Fields: true})
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != 1 || got[0].Title != "widget" || got[0].Category != itemsPasswordKind {
				t.Fatalf("matched items = %+v, want widget with category PASSWORD", got)
			}
			raw, err := tools.Run(ctx, "", []string{"OP_SERVICE_ACCOUNT_TOKEN=" + saToken}, nil, "op", "item", "get", got[0].ID, "--vault", shared, "--format", "json")
			if err != nil {
				t.Fatal(err)
			}
			var item struct{ Category string }
			if err := json.Unmarshal(raw, &item); err != nil {
				t.Fatal(err)
			}
			if item.Category != itemsPasswordKind {
				t.Fatalf("read category = %q, want PASSWORD", item.Category)
			}
			if tools.Vault["op://"+shared+"/widget/credential"] != "synthetic-"+phase {
				t.Fatal("credential was not updated")
			}
		})
	}
}
