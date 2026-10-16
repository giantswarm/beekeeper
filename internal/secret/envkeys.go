package secret

import (
	"fmt"
	"regexp"
	"strings"
)

// An env-style field is a vault field, an item's notes for one, whose text
// holds KEY=VALUE lines: op://<vault>/<item>/<field>#<KEY> names the value
// of one line, which beekeeper reads in its own process like a field's.

// EnvKey is one KEY=VALUE line of an env-style field: the field, the key
// and the length of its value in bytes, never the value.
type EnvKey struct {
	Field  string `json:"field"`
	Key    string `json:"key"`
	Length int    `json:"length"`
}

// envKeyName is a key an env-style line may carry.
var envKeyName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.-]*$`)

// envLine is one KEY=VALUE line of text.
type envLine struct {
	key, value string
}

// envLines are the KEY=VALUE lines of text, in order: blank lines, comments
// and lines without a key are skipped, an "export " in front is dropped,
// and a value in matching single or double quotes loses them.
func envLines(text string) []envLine {
	var out []envLine
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		k, v, ok := strings.Cut(line, "=")
		k = strings.TrimSpace(k)
		if !ok || !envKeyName.MatchString(k) {
			continue
		}
		v = strings.TrimSpace(v)
		if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
			v = v[1 : len(v)-1]
		}
		out = append(out, envLine{key: k, value: v})
	}
	return out
}

// envValue is the value of key among text's KEY=VALUE lines; a key absent
// or named on more than one line is refused, naming field and key alone.
func envValue(text, field, key string) (string, error) {
	var vs []string
	for _, l := range envLines(text) {
		if l.key == key {
			vs = append(vs, l.value)
		}
	}
	switch len(vs) {
	case 0:
		return "", fmt.Errorf("%s: no line %s=… (secret items --env-keys lists its keys)", field, key)
	case 1:
		return vs[0], nil
	}
	return "", fmt.Errorf("%s: %d lines carry %s, name a field with one", field, len(vs), key)
}

// envKeys are the keys of a field's KEY=VALUE lines with their values'
// lengths; the value goes out of scope here.
func envKeys(field, text string) []EnvKey {
	var out []EnvKey
	for _, l := range envLines(text) {
		out = append(out, EnvKey{Field: field, Key: l.key, Length: len(l.value)})
	}
	return out
}

// splitEnvKey cuts op://<vault>/<item>/<field>#<KEY> into the field's
// reference and the key, "" when the reference names a whole field.
func splitEnvKey(ref string) (field, key string) {
	slash := strings.LastIndex(ref, "/")
	if i := strings.LastIndex(ref, "#"); i > slash {
		return ref[:i], ref[i+1:]
	}
	return ref, ""
}
