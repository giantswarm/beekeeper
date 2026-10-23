package secret

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"slices"
	"strconv"
	"strings"
)

// CmdRef starts the reference of a value a producer command generates,
// cmd://<command>#<path>: the command one of [Producers], run in
// beekeeper's process, the path a dotted key of its JSON or YAML stdout
// (none: the whole stdout without its line end).
const CmdRef = "cmd://"

// Producer is a command a cmd:// reference and secret capture may run:
// the words its argv starts with and every option it may take after them.
type Producer struct {
	Words []string
	// Options are the options it takes, exactly as spelled; true takes a
	// value, as the next word or after "=".
	Options map[string]bool
	// Operand, when set, accepts the one operand it takes.
	Operand func(string) bool
}

// Producers are the commands a cmd:// reference and secret capture may
// run; every other command, and every option a producer does not list, is
// refused: az's --debug and --verbose log the generated password on
// stderr, openssl's -out writes a file a session could read, its -engine,
// -provider and -config load code. The list is beekeeper's own, no
// configuration widens it.
var Producers = []Producer{
	{Words: []string{"az", "acr", "token", "credential", "generate"}, Options: map[string]bool{
		"--registry": true, "-r": true, "--name": true, "-n": true, "--resource-group": true, "-g": true,
		"--subscription": true, "--password1": false, "--password2": false,
		"--expiration": true, "--expiration-in-days": true,
		"--output": true, "-o": true, "--query": true, "--only-show-errors": false,
	}},
	{Words: []string{"openssl", "genpkey"}, Options: map[string]bool{"-algorithm": true, "-pkeyopt": true}},
	{Words: []string{"openssl", "rand"}, Options: map[string]bool{"-hex": false, "-base64": false}, Operand: isCount},
}

// isCount reports whether s is a positive decimal number.
func isCount(s string) bool {
	n, err := strconv.Atoi(s)
	return err == nil && n > 0
}

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
// option or operand that producer does not take.
func checkProducer(words []string) error {
	i := slices.IndexFunc(Producers, func(p Producer) bool {
		return len(words) >= len(p.Words) && slices.Equal(words[:len(p.Words)], p.Words)
	})
	if i < 0 {
		names := make([]string, len(Producers))
		for i, p := range Producers {
			names[i] = strings.Join(p.Words, " ")
		}
		return fmt.Errorf("beekeeper runs only the producers %q", names)
	}
	p := Producers[i]
	operands := 0
	for args := words[len(p.Words):]; len(args) > 0; args = args[1:] {
		w := args[0]
		if !strings.HasPrefix(w, "-") {
			if operands++; p.Operand == nil || operands > 1 || !p.Operand(w) {
				return fmt.Errorf("%q: %s takes no such operand", w, strings.Join(p.Words, " "))
			}
			continue
		}
		name, _, inline := strings.Cut(w, "=")
		takes, ok := p.Options[name]
		switch {
		case !ok:
			return fmt.Errorf("%s: %s runs only with the options %q", name, strings.Join(p.Words, " "), slices.Sorted(maps.Keys(p.Options)))
		case !takes && inline:
			return fmt.Errorf("%s takes no value", name)
		case takes && !inline:
			// az takes --debug wherever it stands, the value's place too
			if len(args) < 2 || strings.HasPrefix(args[1], "-") {
				return fmt.Errorf("%s takes a value", name)
			}
			args = args[1:]
		}
	}
	if p.Operand != nil && operands == 0 {
		return fmt.Errorf("%s takes an operand", strings.Join(p.Words, " "))
	}
	return nil
}

// readCmd runs the producer a cmd:// reference names, from the path
// beekeeper resolves and with a clean environment ([ProducerEnv]), and
// reads the value at its path in the producer's stdout. The producer's
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
