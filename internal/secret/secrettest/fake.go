// Package secrettest is a fake sops and op for the tests of beekeeper
// secret: its "encryption" is base64 under a sops metadata block, so a
// test sees a plaintext value on disk as one and secret takes the file for
// a sops file, and its vault is a map.
package secrettest

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

// header is the fake ciphertext's sops metadata block: the base64 of the
// plaintext follows it as the block's one value.
const header = "sops:\n  fake: "

// sops is the fake's name for the tool.
const sops = "sops"

const (
	// passwordPurpose is a Password item's category and its password
	// field's purpose.
	passwordPurpose = "PASSWORD"
	create          = "create"
	get             = "get"
)

// Tools are the fake tools and what they were asked.
type Tools struct {
	mu sync.Mutex
	// Vault maps op://vault/item/field to its value.
	Vault map[string]string
	// Calls are the command lines run, stdin left out.
	Calls []string
	// Tokens are the service account tokens op calls were given.
	Tokens []string
	// Signed is whether the person's own op session is signed in: op
	// without a service account token then runs as the person.
	Signed bool
	// Vaults map a vault's name to its ID.
	Vaults map[string]string
	// Accounts map a service account to the vault grants it was created with.
	Accounts map[string]string
}

// AccountToken is the token the fake answers for a new service account.
func AccountToken(name string) string { return "ops_planted-token-of-" + name }

// Kubeconfig is what the fake kind answers for a cluster's kubeconfig.
func Kubeconfig(cluster string) string { return "kubeconfig of kind-" + cluster }

// New is the fake with a vault.
func New(vault map[string]string) *Tools {
	if vault == nil {
		vault = map[string]string{}
	}
	return &Tools{Vault: vault}
}

// Encrypt is the fake ciphertext of a plaintext document.
func Encrypt(plain string) []byte {
	return []byte(header + base64.StdEncoding.EncodeToString([]byte(plain)) + "\n")
}

// Run is the secret.Runner.
func (t *Tools) Run(_ context.Context, dir string, env []string, stdin io.Reader, name string, args ...string) ([]byte, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.Calls = append(t.Calls, strings.Join(append([]string{name}, args...), " "))
	if name == "op" {
		token := ""
		for _, e := range env {
			if v, ok := strings.CutPrefix(e, "OP_SERVICE_ACCOUNT_TOKEN="); ok {
				token = v
			}
		}
		switch {
		case token != "":
			t.Tokens = append(t.Tokens, token)
		case !t.Signed:
			return nil, errors.New("exit 1 (not signed in)")
		case args[0] == "vault":
			return t.vault(args[1:])
		case args[0] == "service-account" && len(args) > 2 && args[1] == create:
			if t.Accounts == nil {
				t.Accounts = map[string]string{}
			}
			t.Accounts[args[2]] = args[slices.Index(args, "--vault")+1]
			return []byte(AccountToken(args[2]) + "\n"), nil
		}
	}
	switch {
	case name == sops && slices.Contains(args, "decrypt"):
		raw, err := os.ReadFile(args[len(args)-1])
		if err != nil {
			return nil, errors.New("exit 128 (no such file)")
		}
		enc, ok := strings.CutPrefix(string(raw), header)
		if !ok {
			return nil, errors.New("exit 1 (not a sops file)")
		}
		return base64.StdEncoding.DecodeString(strings.TrimSpace(enc))
	case name == sops && len(args) > 2 && args[0] == "unset":
		return nil, unset(args[len(args)-2], args[len(args)-1])
	case name == sops && slices.Contains(args, "encrypt"):
		if !slices.Contains(args, "--filename-override") {
			return nil, errors.New("exit 1 (no creation rule)")
		}
		if _, err := os.Stat(filepath.Join(dir, ".sops.yaml")); err != nil {
			return nil, errors.New("exit 1 (no .sops.yaml in the working directory)")
		}
		plain, err := io.ReadAll(stdin)
		if err != nil {
			return nil, err
		}
		return Encrypt(string(plain)), nil
	case name == "op" && args[0] == "read":
		v, ok := t.Vault[args[len(args)-1]]
		if !ok {
			return nil, errors.New("exit 1 (isn't an item)")
		}
		return []byte(v), nil
	case name == "op" && args[0] == "item":
		return t.item(args[1:], stdin)
	case name == "kind" && len(args) == 4 && args[0] == "get" && args[1] == "kubeconfig":
		return []byte(Kubeconfig(args[3])), nil
	}
	return nil, fmt.Errorf("exit 2 (the fake does not know %s %s)", name, strings.Join(args, " "))
}

func (t *Tools) vault(args []string) ([]byte, error) {
	if t.Vaults == nil {
		t.Vaults = map[string]string{}
	}
	id, ok := t.Vaults[args[1]]
	switch {
	case args[0] == get && !ok:
		return nil, errors.New(`exit 1 (isn't a vault)`)
	case args[0] == create && ok:
		return nil, errors.New("exit 1 (vault exists)")
	case args[0] == create:
		id = "vid-" + args[1]
		t.Vaults[args[1]] = id
	}
	return json.Marshal(map[string]string{"id": id, "name": args[1]})
}

// builtins are op's built-in item fields by id: their purpose and type,
// which op's validator requires.
var builtins = map[string][2]string{
	"password":   {passwordPurpose, "CONCEALED"},
	"username":   {"USERNAME", "STRING"},
	"notesPlain": {"NOTES", "STRING"},
}

func (t *Tools) item(args []string, stdin io.Reader) ([]byte, error) {
	vault := args[slices.Index(args, "--vault")+1]
	type field struct {
		ID      string `json:"id"`
		Label   string `json:"label"`
		Type    string `json:"type"`
		Purpose string `json:"purpose,omitempty"`
		Value   string `json:"value"`
	}
	type item struct {
		ID       string  `json:"id"`
		Title    string  `json:"title"`
		Category string  `json:"category,omitempty"`
		Fields   []field `json:"fields"`
	}
	items := map[string]*item{}
	for ref, v := range t.Vault {
		parts := strings.SplitN(strings.TrimPrefix(ref, "op://"), "/", 3)
		if parts[0] != vault {
			continue
		}
		it, ok := items[parts[1]]
		if !ok {
			it = &item{ID: "id-" + parts[1], Title: parts[1]}
			items[parts[1]] = it
		}
		f := field{ID: parts[2], Label: parts[2], Type: "CONCEALED", Value: v}
		if b, ok := builtins[f.ID]; ok {
			f.Purpose, f.Type = b[0], b[1]
		}
		it.Fields = append(it.Fields, f)
	}
	switch args[0] {
	case "list":
		var out []item
		for _, it := range items {
			out = append(out, item{ID: it.ID, Title: it.Title})
		}
		return json.Marshal(out)
	case get:
		for _, it := range items {
			if it.ID == args[1] {
				return json.Marshal(it)
			}
		}
		return nil, errors.New("exit 1 (isn't an item)")
	case create, "edit":
		if slices.Contains(args, "--template") {
			return nil, fmt.Errorf("exit 1 (cannot %s an item from template and stdin at the same time)", args[0])
		}
		if args[0] == create && !slices.Contains(args, "-") {
			return nil, errors.New("exit 1 (no piped item: create takes it after \"-\")")
		}
		raw, err := io.ReadAll(stdin)
		if err != nil {
			return nil, err
		}
		var it item
		if err := json.Unmarshal(raw, &it); err != nil || it.Title == "" {
			return nil, errors.New("exit 1 (no template on stdin)")
		}
		var refused []string
		password := false
		for _, f := range it.Fields {
			password = password || f.Purpose == passwordPurpose
			if b, ok := builtins[f.ID]; ok && (f.Purpose != b[0] || f.Type != b[1]) {
				refused = append(refused, fmt.Sprintf("field %q must have purpose %s and type %s", f.ID, b[0], b[1]))
			}
		}
		if it.Category == passwordPurpose && !password {
			refused = append(refused, "a Password item must have a field with purpose PASSWORD")
		}
		if len(refused) > 0 {
			return nil, fmt.Errorf("exit 1 ([ERROR] unable to process line 1: Validation: (validateVaultItem failed to Validate), "+
				"Couldn't validate the item: \"[ItemValidator] has found %d errors, 0 warnings: \nDetails:\nErrors:\n%s\")", len(refused), strings.Join(refused, "\n"))
		}
		for _, f := range it.Fields {
			t.Vault["op://"+vault+"/"+it.Title+"/"+f.Label] = f.Value
		}
		return bytes.TrimSpace(raw), nil
	}
	return nil, errors.New("exit 2 (unknown op item command)")
}

// index is one key of sops' index syntax, ["a"][0]["b"].
var index = regexp.MustCompile(`\[("(?:[^"\\]|\\.)*"|\d+)\]`)

// unset removes the key at a sops index from a fake SOPS file, failing like
// sops on an absent one.
func unset(file, path string) error {
	raw, err := os.ReadFile(file) //nolint:gosec // the test's file
	if err != nil {
		return errors.New("exit 128 (no such file)")
	}
	enc, ok := strings.CutPrefix(string(raw), header)
	if !ok {
		return errors.New("exit 1 (not a sops file)")
	}
	plain, err := base64.StdEncoding.DecodeString(strings.TrimSpace(enc))
	if err != nil {
		return err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(plain, &doc); err != nil {
		return err
	}
	n := doc.Content[0]
	keys := index.FindAllStringSubmatch(path, -1)
	for i, m := range keys {
		k := m[1]
		if strings.HasPrefix(k, `"`) {
			k, _ = strconv.Unquote(k)
		}
		at := -1
		switch n.Kind {
		case yaml.MappingNode:
			for j := 0; j+1 < len(n.Content); j += 2 {
				if n.Content[j].Value == k {
					at = j
				}
			}
		case yaml.SequenceNode:
			if j, err := strconv.Atoi(k); err == nil && j < len(n.Content) {
				at = j
			}
		}
		if at < 0 {
			return errors.New("exit 1 (key not found)")
		}
		if i < len(keys)-1 {
			if n.Kind == yaml.MappingNode {
				at++
			}
			n = n.Content[at]
			continue
		}
		if n.Kind == yaml.MappingNode {
			n.Content = slices.Delete(n.Content, at, at+2)
		} else {
			n.Content = slices.Delete(n.Content, at, at+1)
		}
	}
	out, err := yaml.Marshal(&doc)
	if err != nil {
		return err
	}
	return os.WriteFile(file, Encrypt(string(out)), 0o600)
}
