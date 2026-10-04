// Package secrettest is a fake sops and op for the tests of beekeeper
// secret: its "encryption" is base64 behind a header, so a test sees a
// plaintext value on disk as one, and its vault is a map.
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
	"slices"
	"strings"
	"sync"
)

const header = "FAKESOPS\n"

// Tools are the fake tools and what they were asked.
type Tools struct {
	mu sync.Mutex
	// Vault maps op://vault/item/field to its value.
	Vault map[string]string
	// Calls are the command lines run, stdin left out.
	Calls []string
	// Tokens are the service account tokens op calls were given.
	Tokens []string
}

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
			token, _ = strings.CutPrefix(e, "OP_SERVICE_ACCOUNT_TOKEN=")
		}
		if token == "" {
			return nil, errors.New("exit 1 (not signed in)")
		}
		t.Tokens = append(t.Tokens, token)
	}
	switch {
	case name == "sops" && slices.Contains(args, "decrypt"):
		raw, err := os.ReadFile(args[len(args)-1])
		if err != nil {
			return nil, errors.New("exit 128 (no such file)")
		}
		enc, ok := strings.CutPrefix(string(raw), header)
		if !ok {
			return nil, errors.New("exit 1 (not a sops file)")
		}
		return base64.StdEncoding.DecodeString(strings.TrimSpace(enc))
	case name == "sops" && slices.Contains(args, "encrypt"):
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

func (t *Tools) item(args []string, stdin io.Reader) ([]byte, error) {
	vault := args[slices.Index(args, "--vault")+1]
	type field struct {
		ID    string `json:"id"`
		Label string `json:"label"`
		Type  string `json:"type"`
		Value string `json:"value"`
	}
	type item struct {
		ID     string  `json:"id"`
		Title  string  `json:"title"`
		Fields []field `json:"fields"`
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
		it.Fields = append(it.Fields, field{ID: parts[2], Label: parts[2], Type: "CONCEALED", Value: v})
	}
	switch args[0] {
	case "list":
		var out []item
		for _, it := range items {
			out = append(out, item{ID: it.ID, Title: it.Title})
		}
		return json.Marshal(out)
	case "get":
		for _, it := range items {
			if it.ID == args[1] {
				return json.Marshal(it)
			}
		}
		return nil, errors.New("exit 1 (isn't an item)")
	case "create", "edit":
		raw, err := io.ReadAll(stdin)
		if err != nil {
			return nil, err
		}
		var it item
		if err := json.Unmarshal(raw, &it); err != nil || it.Title == "" {
			return nil, errors.New("exit 1 (no template on stdin)")
		}
		for _, f := range it.Fields {
			t.Vault["op://"+vault+"/"+it.Title+"/"+f.Label] = f.Value
		}
		return bytes.TrimSpace(raw), nil
	}
	return nil, errors.New("exit 2 (unknown op item command)")
}
