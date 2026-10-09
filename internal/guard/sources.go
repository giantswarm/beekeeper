package guard

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"
)

// The index's sources are read by beekeeper's own process: the decrypted
// values stay in its memory, are fingerprinted and dropped, and no command
// it runs prints them anywhere.

// Reference prefixes of the values a source indexes; a rebuild drops them.
const (
	SOPSRef = "sops://"
	OpRef   = "op://"
)

// IndexSOPS adds every string value of each SOPS file, decrypted with
// sops -d, under sops://<file>#<key path>. A file sops cannot decrypt is
// named in the error and the others are still indexed.
func IndexSOPS(ctx context.Context, ix *Index, files []string) (int, error) {
	n := 0
	var errs []error
	for _, f := range files {
		out, err := quietRun(ctx, nil, nil, "sops", "-d", "--output-type", "json", f)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", f, err))
			continue
		}
		v, err := decodeJSON(out)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: sops answered no JSON", f))
			continue
		}
		leaves(v, "", func(key, value string) {
			n += ix.Add(SOPSRef+f+"#"+key, value)
		})
	}
	return n, errors.Join(errs...)
}

// IndexVault adds the concealed fields (passwords, credentials, keys) of
// every item in the 1Password vault, read with op item list and op item get
// --reveal, under op://<vault>/<item>/<field>. env is added to op's
// environment: the service account's token for the shared vault.
func IndexVault(ctx context.Context, ix *Index, vault string, env []string) (int, error) {
	list, err := quietRun(ctx, nil, env, "op", "item", "list", "--vault", vault, "--format", "json")
	if err != nil {
		return 0, fmt.Errorf("op item list --vault %q: %w", vault, err)
	}
	items, err := quietRun(ctx, bytes.NewReader(list), env, "op", "item", "get", "-", "--reveal", "--format", "json")
	if err != nil {
		return 0, fmt.Errorf("op item get in %q: %w", vault, err)
	}
	type field struct {
		Label   string `json:"label"`
		Type    string `json:"type"`
		Purpose string `json:"purpose"`
		Value   string `json:"value"`
	}
	type item struct {
		Title  string  `json:"title"`
		Fields []field `json:"fields"`
	}
	n := 0
	dec := json.NewDecoder(bytes.NewReader(items))
	for {
		var it item
		if err := dec.Decode(&it); errors.Is(err, io.EOF) {
			return n, nil
		} else if err != nil {
			return n, fmt.Errorf("op item get in %q: an answer that is no item", vault)
		}
		for _, f := range it.Fields {
			if f.Type == "CONCEALED" || f.Type == "SSHKEY" || f.Purpose == "PASSWORD" {
				n += ix.Add(OpRef+vault+"/"+it.Title+"/"+f.Label, f.Value)
			}
		}
	}
}

// leaves calls fn with the dotted key path and value of every string in v.
func leaves(v any, path string, fn func(key, value string)) {
	join := func(k string) string {
		if path == "" {
			return k
		}
		return path + "." + k
	}
	switch t := v.(type) {
	case string:
		fn(path, t)
	case []any:
		for i, e := range t {
			leaves(e, join(fmt.Sprint(i)), fn)
		}
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			leaves(t[k], join(k), fn)
		}
	}
}

// quietRun runs a command, its environment plus env, and returns its
// output. Its error names the exit status and the first stderr line,
// redacted: a tool may echo what it read.
func quietRun(ctx context.Context, stdin io.Reader, env []string, name string, args ...string) ([]byte, error) {
	c := exec.CommandContext(ctx, name, args...) //nolint:gosec // sops and op with the configured sources
	if env != nil {
		c.Env = append(os.Environ(), env...)
	}
	c.Stdin = stdin
	var stderr bytes.Buffer
	c.Stderr = &stderr
	out, err := c.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return nil, fmt.Errorf("exit %d (%s)", ee.ExitCode(), FirstLine(stderr.String()))
		}
		return nil, err
	}
	return out, nil
}

// FirstLine is a tool's first stderr line, cut short and stripped of what
// a pattern would redact.
func FirstLine(s string) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	if len(s) > 160 {
		s = s[:160] + "…"
	}
	empty := &Index{}
	s, _ = empty.Redact(s)
	return s
}

// Message is a tool's whole stderr on one line, stripped of what a pattern
// would redact: a refusal's reason is often past its first line.
func Message(s string) string {
	empty := &Index{}
	s, _ = empty.Redact(strings.Join(strings.Fields(s), " "))
	return s
}
