package secret

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path"
	"strings"

	"github.com/itchyny/gojq"

	"github.com/giantswarm/beekeeper/internal/guard"
)

// The key and the type of a kubernetes.io/dockerconfigjson Secret.
const (
	dockerConfigKey  = ".dockerconfigjson"
	dockerConfigType = "kubernetes.io/dockerconfigjson"
)

// DockerConfig is the kubernetes.io/dockerconfigjson shape a captured
// registry password takes: the registry it pulls from and the user it
// authenticates.
type DockerConfig struct {
	Registry string
	Username string
}

// check refuses an empty or malformed registry or user.
func (d DockerConfig) check() error {
	switch {
	case d.Registry == "" || strings.ContainsAny(d.Registry, " \t\n/"):
		return fmt.Errorf("--dockerconfigjson %q: a registry host, such as registry.example.io", d.Registry)
	case d.Username == "" || strings.ContainsAny(d.Username, ": \t\n"):
		return fmt.Errorf("--username %q: a user without a colon or a space", d.Username)
	}
	return nil
}

// value is the .dockerconfigjson that authenticates the user with password.
func (d DockerConfig) value(password string) (string, error) {
	auth := base64.StdEncoding.EncodeToString([]byte(d.Username + ":" + password))
	b, err := json.Marshal(map[string]any{"auths": map[string]any{
		d.Registry: map[string]string{"username": d.Username, "password": password, "auth": auth},
	}})
	return string(b), err
}

// CaptureOptions are the producer whose stdout a capture takes the value
// from and where the value goes besides the SOPS path.
type CaptureOptions struct {
	// Producer is the command that prints the value, run in this process.
	Producer []string
	// Env is the producer's environment ([ProducerEnv]).
	Env []string
	// Stderr receives the producer's stderr; nil discards it.
	Stderr io.Writer
	// JQ is a jq filter that picks the value out of the producer's JSON
	// output, one string; empty takes the whole output, its line end
	// trimmed.
	JQ string
	// Vault is the shared vault's field that holds the value first.
	Vault Ref
	// Docker writes the value as the .dockerconfigjson of a
	// kubernetes.io/dockerconfigjson Secret instead of at a path.
	Docker *DockerConfig
	// New is the Secret a file absent so far starts as.
	New *NewSecret
}

// CaptureResult is what a capture answers: where the value went, its
// length as written and its keyed fingerprint.
type CaptureResult struct {
	Key         string `json:"key"`
	Bytes       int    `json:"bytes"`
	Fingerprint string `json:"fingerprint"`
}

// Capture runs the producer and writes the value it prints (one string of
// its JSON output with opt.JQ) to the shared vault's field first when one
// is given, then into the SOPS path in o.Encode, or as a Secret's
// .dockerconfigjson with opt.Docker. Every check (the references, the
// creation rule, the filter) runs before the producer does: a credential
// that is printed once is not spent on a call that could not store it. The
// producer's stdout is never answered.
func (o *Ops) Capture(ctx context.Context, dst Ref, opt CaptureOptions) (CaptureResult, error) {
	switch {
	case dst.Op != "" || dst.IsKube():
		return CaptureResult{}, fmt.Errorf("%s: capture writes a SOPS file", dst)
	case opt.Docker == nil && dst.Path == "":
		return CaptureResult{}, fmt.Errorf("%s: capture writes a SOPS path, file#path, or a Secret's .dockerconfigjson with --dockerconfigjson", dst)
	case opt.Docker != nil && dst.Path != "":
		return CaptureResult{}, fmt.Errorf("%s: --dockerconfigjson writes the Secret's %s, name the file alone", dst, dockerConfigKey)
	case len(opt.Producer) == 0:
		return CaptureResult{}, errors.New("no producer: capture <file#path> -- <producer…>")
	}
	if opt.Docker != nil {
		if !o.Encode.IsZero() {
			return CaptureResult{}, errors.New("--encode and --dockerconfigjson: the shape encodes the value itself")
		}
		if err := opt.Docker.check(); err != nil {
			return CaptureResult{}, err
		}
	}
	if opt.Vault != (Ref{}) {
		if opt.Vault.Op == "" {
			return CaptureResult{}, fmt.Errorf("%s: the vault copy is an op://<vault>/<item>/<field>", opt.Vault)
		}
		if err := o.checkVault(opt.Vault); err != nil {
			return CaptureResult{}, err
		}
	}
	if opt.New != nil {
		if err := opt.New.check(dst.File); err != nil {
			return CaptureResult{}, err
		}
	}
	var query *gojq.Code
	if opt.JQ != "" {
		var err error
		if query, err = compileJQ(opt.JQ); err != nil {
			return CaptureResult{}, err
		}
	}
	doc, keys, err := o.captureTarget(ctx, dst, opt)
	if err != nil {
		return CaptureResult{}, err
	}
	out, err := produce(ctx, opt)
	if err != nil {
		return CaptureResult{}, err
	}
	v, err := extract(out, opt.JQ, query)
	if err != nil {
		return CaptureResult{}, err
	}
	if opt.Vault != (Ref{}) {
		if err := o.storeVault(ctx, opt.Vault, v); err != nil {
			return CaptureResult{}, fmt.Errorf("the value is stored nowhere, run the producer again: %w", err)
		}
	}
	// the vault keeps the printed value, the SOPS path its written form
	if opt.Docker != nil {
		if v, err = opt.Docker.value(v); err != nil {
			return CaptureResult{}, err
		}
		if err := doc.set("type", dockerConfigType); err != nil {
			return CaptureResult{}, fmt.Errorf("%s: %w", dst.File, err)
		}
	} else {
		v = o.Encode.Apply(v)
	}
	key := Ref{File: dst.File, Path: strings.Join(keys, ".")}
	if err := doc.setKeys(keys, v); err != nil {
		return CaptureResult{}, fmt.Errorf("%s: %w", dst.File, err)
	}
	if err := o.encrypt(ctx, doc, dst.File); err != nil {
		if opt.Vault != (Ref{}) {
			return CaptureResult{}, fmt.Errorf("the vault holds the value, the SOPS file not (copy %s %s): %w", opt.Vault, key, err)
		}
		return CaptureResult{}, fmt.Errorf("the value is stored nowhere, run the producer again: %w", err)
	}
	return CaptureResult{Key: key.String(), Bytes: len(v), Fingerprint: o.Fingerprint(v)}, nil
}

// captureTarget is the document a captured value goes into and the keys
// of its path there, one key each: dst's path, or a Secret's
// stringData..dockerconfigjson with opt.Docker, checked against the
// creation rule.
func (o *Ops) captureTarget(ctx context.Context, dst Ref, opt CaptureOptions) (*document, []string, error) {
	if opt.Docker == nil {
		doc, dst, err := o.target(ctx, dst, opt.New)
		if err != nil {
			return nil, nil, err
		}
		return doc, strings.Split(dst.Path, "."), nil
	}
	doc, err := o.document(ctx, dst.File, opt.New)
	if err != nil {
		return nil, nil, err
	}
	if k, _ := doc.get("kind"); k != secretKind {
		return nil, nil, fmt.Errorf("%s: --dockerconfigjson writes a Secret; start one in an absent file with --name and --namespace", dst.File)
	}
	keys := []string{"stringData", dockerConfigKey}
	if err := checkRule(dst.File, keys); err != nil {
		return nil, nil, err
	}
	return doc, keys, nil
}

// compileJQ compiles a jq filter; its error names the filter, never a value.
func compileJQ(filter string) (*gojq.Code, error) {
	q, err := gojq.Parse(filter)
	if err != nil {
		return nil, fmt.Errorf("--jq %q: %w", filter, err)
	}
	code, err := gojq.Compile(q)
	if err != nil {
		return nil, fmt.Errorf("--jq %q: %w", filter, err)
	}
	return code, nil
}

// produce runs the producer with its environment, its stderr passed on,
// and answers its stdout: a non-zero exit or nothing on stdout refuses.
func produce(ctx context.Context, opt CaptureOptions) ([]byte, error) {
	name := opt.Producer[0]
	c := exec.CommandContext(ctx, name, opt.Producer[1:]...) //nolint:gosec // the caller's producer is the purpose
	c.Env = opt.Env
	c.Stderr = opt.Stderr
	var out bytes.Buffer
	c.Stdout = &out
	if err := c.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return nil, fmt.Errorf("the producer %s exited %d: nothing stored", name, ee.ExitCode())
		}
		return nil, fmt.Errorf("the producer %s: %w", name, err)
	}
	if len(bytes.TrimSpace(out.Bytes())) == 0 {
		return nil, fmt.Errorf("the producer %s printed nothing on stdout: nothing stored", name)
	}
	return out.Bytes(), nil
}

// extract is the value in a producer's stdout: the one string query picks
// from its JSON, else the whole output without its line end. Its errors
// name the filter and the shape of what it found, never a value.
func extract(out []byte, filter string, query *gojq.Code) (string, error) {
	if query == nil {
		return strings.TrimRight(string(out), "\r\n"), nil
	}
	var in any
	if err := json.Unmarshal(out, &in); err != nil {
		return "", errors.New("the producer's stdout is no JSON document: --jq reads one")
	}
	iter := query.Run(in)
	v, ok := iter.Next()
	if !ok {
		return "", fmt.Errorf("--jq %q picks nothing from the producer's output", filter)
	}
	if _, failed := v.(error); failed {
		// gojq's message may quote what it read
		return "", fmt.Errorf("--jq %q fails on the producer's output (a JSON %s)", filter, jsonKind(in))
	}
	s, isString := v.(string)
	switch {
	case !isString:
		return "", fmt.Errorf("--jq %q picks a JSON %s, not a string", filter, jsonKind(v))
	case s == "":
		return "", fmt.Errorf("--jq %q picks an empty string", filter)
	}
	if _, more := iter.Next(); more {
		return "", fmt.Errorf("--jq %q picks more than one value: pick one, such as .[0]", filter)
	}
	return s, nil
}

// jsonKind names the kind of a decoded JSON value.
func jsonKind(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case map[string]any:
		return "object"
	case []any:
		return "array"
	case string:
		return "string"
	case bool:
		return "boolean"
	}
	return "number"
}

// ProducerEnv is environ without the variables that carry a secret: the
// vault's credentials, sops' age identity and those whose names match one
// of the globs of drop (secret.env).
func ProducerEnv(environ, drop []string) []string {
	out := make([]string, 0, len(environ))
	for _, kv := range environ {
		name, _, _ := strings.Cut(kv, "=")
		if guard.VaultVar.MatchString(name) || name == envAgeKey || globbed(drop, name) {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// globbed reports whether name matches one of the globs.
func globbed(globs []string, name string) bool {
	for _, g := range globs {
		if ok, _ := path.Match(g, name); ok {
			return true
		}
	}
	return false
}
