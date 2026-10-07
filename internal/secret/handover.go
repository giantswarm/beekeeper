package secret

import (
	"context"
	"fmt"
	"io"
	"regexp"
	"strings"
)

// Credential is a value a process beekeeper starts gets in its environment:
// the variable Name, from Ref.
type Credential struct {
	Name string
	Ref  Ref
}

// envName is a variable's name.
var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// Handover holds the values of a process's credentials, read from their
// references, until they are written into its environment: NAME=value
// lines for a process that reads its environment that way before it runs,
// an omp agent's shell from its inbox. The values live here and go to the
// writer alone.
type Handover struct {
	lines string
}

// Handover reads every credential of creds now, so that a reference that
// does not answer, a name that is no variable's or a value that is empty or
// more than one line, which no variable carries, refuses the whole set
// before anything runs on it.
func (o *Ops) Handover(ctx context.Context, creds []Credential) (*Handover, error) {
	var b strings.Builder
	for _, c := range creds {
		if !envName.MatchString(c.Name) {
			return nil, fmt.Errorf("%q is no variable name", c.Name)
		}
		v, err := o.value(ctx, c.Ref)
		if err != nil {
			return nil, err
		}
		if v == "" || strings.ContainsAny(v, "\n\x00") {
			return nil, fmt.Errorf("%s: empty or more than one line: no value a variable carries", c.Ref)
		}
		b.WriteString(c.Name + "=" + v + "\n")
	}
	return &Handover{lines: b.String()}, nil
}

// WriteTo writes the NAME=value lines to w.
func (h *Handover) WriteTo(w io.Writer) (int64, error) {
	n, err := io.WriteString(w, h.lines)
	return int64(n), err
}
