package secret

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"
)

// basicPrefix starts the encoding of an HTTP Basic credential's user.
const basicPrefix = "basic:"

// Encoding transforms a value before it is written, inside beekeeper's
// process: a consumer that injects a value verbatim (a gateway's
// "Authorization: Basic <value>") needs the encoded form, which no caller
// could otherwise make without reading the value. The zero Encoding writes
// the value as it is.
type Encoding struct {
	// Base64 encodes the value in standard base64.
	Base64 bool `json:"base64,omitempty"`
	// User, when set, encodes base64("<user>:<value>"), an HTTP Basic
	// credential.
	User string `json:"user,omitempty"`
}

// ParseEncoding reads base64 or basic:<user>; "" is the value as it is.
func ParseEncoding(s string) (Encoding, error) {
	switch {
	case s == "":
		return Encoding{}, nil
	case s == "base64":
		return Encoding{Base64: true}, nil
	case strings.HasPrefix(s, basicPrefix):
		user := strings.TrimPrefix(s, basicPrefix)
		if user == "" || strings.Contains(user, ":") {
			return Encoding{}, fmt.Errorf("--encode %q: basic:<user>, a user without a colon", s)
		}
		return Encoding{Base64: true, User: user}, nil
	}
	return Encoding{}, fmt.Errorf("--encode %q: one of base64, basic:<user>", s)
}

// IsZero is whether the encoding writes the value as it is.
func (e Encoding) IsZero() bool { return e == Encoding{} }

// String is the encoding as the flag names it, "" for none.
func (e Encoding) String() string {
	switch {
	case e.User != "":
		return basicPrefix + e.User
	case e.Base64:
		return "base64"
	}
	return ""
}

// Apply is v in the encoding.
func (e Encoding) Apply(v string) string {
	if e.User != "" {
		v = e.User + ":" + v
	}
	if e.Base64 {
		v = base64.StdEncoding.EncodeToString([]byte(v))
	}
	return v
}

// encoded reads the one value a single reference names in o.Encode, for a
// write or a fingerprint; the reads beekeeper makes for itself (an age
// identity, a rotation's old value) stay [Ops.value].
func (o *Ops) encoded(ctx context.Context, r Ref) (string, error) {
	v, err := o.value(ctx, r)
	if err != nil {
		return "", err
	}
	return o.Encode.Apply(v), nil
}
