package secret

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
)

// CmdRef starts the reference of a value a producer command generates,
// cmd://<command>#<path>: the command one of [Producers], run in
// beekeeper's process, the path a dotted key of its JSON or YAML stdout
// (none: the whole stdout without its line end).
const CmdRef = "cmd://"

// Producers are the commands a cmd:// reference and secret capture may
// run, each the words its argv starts with; every other command is
// refused. The list is beekeeper's own, no configuration widens it.
var Producers = [][]string{
	{"az", "acr", "token", "credential", "generate"},
	{"openssl", "genpkey"},
	{"openssl", "rand"},
}

// producerDenied are the options, with one dash or two, that make an
// allowed producer put its answer anywhere but its stdout: az's request
// and response log on stderr, the generated password in it; openssl's
// file a session could read.
var producerDenied = []string{"debug", "verbose", "out"}

// parseCmdRef reads cmd://<command>#<path>.
func parseCmdRef(s string) (Ref, error) {
	line, path, _ := strings.Cut(strings.TrimPrefix(s, CmdRef), "#")
	words := strings.Fields(line)
	if err := checkProducer(words); err != nil {
		return Ref{}, fmt.Errorf("%q: %w", s, err)
	}
	return Ref{Cmd: strings.Join(words, " "), Path: path}, nil
}

// checkProducer refuses a command none of [Producers] starts, and an
// argument of producerDenied.
func checkProducer(words []string) error {
	allowed := slices.ContainsFunc(Producers, func(p []string) bool {
		return len(words) >= len(p) && slices.Equal(words[:len(p)], p)
	})
	if !allowed {
		names := make([]string, len(Producers))
		for i, p := range Producers {
			names[i] = strings.Join(p, " ")
		}
		return fmt.Errorf("beekeeper runs only the producers %q", names)
	}
	for _, w := range words {
		flag, _, _ := strings.Cut(w, "=")
		if name := strings.TrimLeft(flag, "-"); name != flag && slices.Contains(producerDenied, name) {
			return fmt.Errorf("%s: a producer runs without it, it logs the credential", flag)
		}
	}
	return nil
}

// readCmd runs the producer a cmd:// reference names, with the
// environment minus every variable that carries a secret ([ProducerEnv]),
// and reads the value at its path in the producer's stdout. The producer's
// stderr is discarded, never answered: a failure names its exit code alone.
func (o *Ops) readCmd(ctx context.Context, r Ref) (string, error) {
	if !o.Produce {
		return "", fmt.Errorf("%s: a producer generates a new credential on every run: cmd:// is a source of copy only", r)
	}
	words := strings.Fields(r.Cmd)
	if err := checkProducer(words); err != nil {
		return "", fmt.Errorf("%s: %w", r, err)
	}
	out, err := produce(ctx, CaptureOptions{Producer: words, Env: ProducerEnv(os.Environ(), o.SecretEnv)})
	if err != nil {
		return "", err
	}
	if r.Path == "" {
		return strings.TrimRight(string(out), "\r\n"), nil
	}
	v, err := valueAt(out, r.Path)
	if err != nil {
		return "", fmt.Errorf("%s: %w", r, err)
	}
	if v == "" || v == "null" {
		// a JSON null reads as the scalar null: az's unrequested password2
		return "", fmt.Errorf("%s: the producer printed no value at %s: nothing stored", r, r.Path)
	}
	return v, nil
}

// CopyDocker writes the one value src names as the password of a
// kubernetes.io/dockerconfigjson Secret in file: its stringData holds
// .dockerconfigjson, the registry's auths entry. An absent file starts as
// the Secret nw names. Every check runs before the value is read, so a
// producer's credential is not spent on a write that could not happen. It
// answers the length of the .dockerconfigjson written.
func (o *Ops) CopyDocker(ctx context.Context, src Ref, file string, d DockerConfig, nw *NewSecret) (int, error) {
	switch {
	case !src.Single():
		return 0, fmt.Errorf("%s: --dockerconfigjson takes one value (file#path, op://…, k8s://…, file://…#path or cmd://…)", src)
	case !o.Encode.IsZero():
		return 0, errors.New("--encode and --dockerconfigjson: the shape encodes the value itself")
	}
	if err := d.check(); err != nil {
		return 0, err
	}
	if nw != nil {
		if err := nw.check(file); err != nil {
			return 0, err
		}
	}
	doc, keys, err := o.dockerTarget(ctx, file, nw)
	if err != nil {
		return 0, err
	}
	v, err := o.value(ctx, src)
	if err != nil {
		return 0, err
	}
	if v, err = d.fill(doc, file, keys, v); err != nil {
		return 0, err
	}
	return len(v), o.encrypt(ctx, doc, file)
}
