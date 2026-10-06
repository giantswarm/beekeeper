package secret

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"

	"filippo.io/age"
	"gopkg.in/yaml.v3"

	"github.com/giantswarm/beekeeper/internal/guard"
)

// AgeIdentity is where beekeeper finds the age identity of a SOPS file
// none of sops' own sources holds: a field of the shared vault or an
// identity file on the host, read in beekeeper's process and given to the
// one sops call's environment alone.
type AgeIdentity struct {
	// Recipient is the age recipient (age1…) the identity decrypts for.
	Recipient string
	// Path matches the file's absolute path; nil matches by recipient only.
	Path *regexp.Regexp
	// Ref is the op:// field or the file:// identity file that holds the
	// identity (AGE-SECRET-KEY-1…, an identity file's comments allowed).
	Ref string
}

// FileRef starts the reference of an identity file on the host.
const FileRef = "file://"

// ErrNoAgeIdentity marks a SOPS file whose age recipients no identity
// beekeeper can reach decrypts for.
var ErrNoAgeIdentity = errors.New("no age identity")

// The environment variables sops reads its age identities from.
const (
	envAgeKey     = "SOPS_AGE_KEY"
	envAgeKeyFile = "SOPS_AGE_KEY_FILE"
)

// opaqueAgeEnv are sops' identity sources beekeeper cannot look into: with
// one set, sops alone finds out whether it decrypts.
var opaqueAgeEnv = []string{"SOPS_AGE_KEY_CMD", "SOPS_AGE_SSH_PRIVATE_KEY_FILE"}

// sopsKeys are the key groups of a SOPS file other than age: with any of
// them, sops may decrypt without an age identity.
var sopsKeys = []string{"kms", "gcp_kms", "azure_kv", "hc_vault", "pgp", "key_groups"}

// ageEnv is the environment the sops call that decrypts file needs: none
// when sops finds an identity itself or beekeeper cannot tell, the
// identity of the matching [AgeIdentity] when one is configured, and
// [ErrNoAgeIdentity] naming the recipients and the sources checked
// otherwise.
func (o *Ops) ageEnv(ctx context.Context, file string) ([]string, error) {
	id, recipients, err := o.ageIdentity(file)
	if id == nil || err != nil {
		return nil, err
	}
	key, err := o.ageKey(ctx, id.Ref)
	if err != nil {
		return nil, fmt.Errorf("%s: the age identity of secret.ageIdentities: %w", file, err)
	}
	// the parse error is dropped: it may quote the line it failed on
	parsed, err := age.ParseIdentities(strings.NewReader(key))
	if err != nil {
		return nil, fmt.Errorf("%s: %s holds no age identity (AGE-SECRET-KEY-1…)", file, id.Ref)
	}
	var held []string
	for _, p := range parsed {
		ident, ok := p.(*age.X25519Identity)
		if !ok {
			continue
		}
		r := ident.Recipient().String()
		if slices.Contains(recipients, r) {
			return []string{envAgeKey + "=" + ident.String()}, nil
		}
		held = append(held, r)
	}
	if len(held) == 0 {
		return nil, fmt.Errorf("%s: %s holds no age identity (AGE-SECRET-KEY-1…)", file, id.Ref)
	}
	return nil, fmt.Errorf("%s: %s holds the identity of %s, not of the file's recipients %s", file, id.Ref, strings.Join(held, ", "), strings.Join(recipients, ", "))
}

// ageKey reads the identity ref holds: an op:// field of the shared vault
// or a file:// identity file, read here and nowhere else.
func (o *Ops) ageKey(ctx context.Context, ref string) (string, error) {
	path, ok := strings.CutPrefix(ref, FileRef)
	if !ok {
		return o.value(ctx, Ref{Op: ref})
	}
	raw, err := os.ReadFile(path) //nolint:gosec // the identity file secret.ageIdentities names
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

// AgeNeedsVault reports whether decrypting one of the SOPS files args name,
// relative to dir (the working directory when empty), takes an age
// identity from the shared vault.
func (o *Ops) AgeNeedsVault(dir string, args []string) bool {
	return o.ageNeeds(dir, args, func(id *AgeIdentity) bool { return strings.HasPrefix(id.Ref, guard.OpRef) })
}

// AgeNeedsIdentity reports whether decrypting one of the SOPS files args
// name, relative to dir (the working directory when empty), takes an
// identity of secret.ageIdentities, from the vault or a file.
func (o *Ops) AgeNeedsIdentity(dir string, args []string) bool {
	return o.ageNeeds(dir, args, func(*AgeIdentity) bool { return true })
}

// ageNeeds reports whether one of the SOPS files args name, relative to
// dir, takes an identity of secret.ageIdentities that match accepts.
func (o *Ops) ageNeeds(dir string, args []string, match func(*AgeIdentity) bool) bool {
	if len(o.Ages) == 0 {
		return false
	}
	for _, a := range args {
		if strings.HasPrefix(a, "-") || strings.HasPrefix(a, guard.OpRef) {
			continue
		}
		r, err := ParseRef(a)
		if err != nil {
			continue
		}
		if dir != "" && !filepath.IsAbs(r.File) {
			r.File = filepath.Join(dir, r.File)
		}
		if id, _, _ := o.ageIdentity(r.File); id != nil && match(id) {
			return true
		}
	}
	return false
}

// ageIdentity is the configured identity decrypting file takes, with the
// file's recipients: nil when the file has no age recipients beekeeper can
// check (none, an SSH or plugin recipient, another key group), when one of
// sops' own sources holds an identity for one of them, or when one of
// those sources is opaque to beekeeper.
func (o *Ops) ageIdentity(file string) (*AgeIdentity, []string, error) {
	recipients := ageRecipients(file)
	if len(recipients) == 0 {
		return nil, nil, nil
	}
	checked, found := localAgeIdentities(recipients)
	if found {
		return nil, nil, nil
	}
	abs, err := filepath.Abs(file)
	if err != nil {
		return nil, nil, err
	}
	for i, id := range o.Ages {
		if slices.Contains(recipients, id.Recipient) || id.Path != nil && id.Path.MatchString(abs) {
			return &o.Ages[i], recipients, nil
		}
	}
	return nil, nil, fmt.Errorf("%s: %w for its recipients %s: checked %s and secret.ageIdentities; "+
		"an entry there (recipient or pathRegex, and ref: op://<vault>/<item>/<field> or file://<identity file> holding the AGE-SECRET-KEY-1… identity) gives beekeeper one",
		file, ErrNoAgeIdentity, strings.Join(recipients, ", "), strings.Join(checked, ", "))
}

// ageRecipients are the age recipients in file's sops metadata, nil when
// it has none, any other key group, or a recipient that is not X25519.
func ageRecipients(file string) []string {
	raw, err := os.ReadFile(file) //nolint:gosec // a SOPS file the caller named; its metadata is plaintext
	if err != nil {
		return nil
	}
	var doc struct {
		SOPS map[string]any `yaml:"sops"`
	}
	if yaml.Unmarshal(raw, &doc) != nil || doc.SOPS == nil {
		return nil
	}
	for _, k := range sopsKeys {
		if v, ok := doc.SOPS[k].([]any); ok && len(v) > 0 {
			return nil
		}
	}
	entries, _ := doc.SOPS["age"].([]any)
	var out []string
	for _, e := range entries {
		m, _ := e.(map[string]any)
		r, _ := m["recipient"].(string)
		if _, err := age.ParseX25519Recipient(r); err != nil {
			return nil
		}
		out = append(out, r)
	}
	return out
}

// localAgeIdentities checks sops' own identity sources for one of
// recipients: what it checked, each with what it found, and whether one
// holds an identity (or is opaque to beekeeper, which counts as found).
func localAgeIdentities(recipients []string) ([]string, bool) {
	for _, e := range opaqueAgeEnv {
		if os.Getenv(e) != "" {
			return nil, true
		}
	}
	var checked []string
	check := func(name, keys string, present bool) bool {
		switch {
		case !present:
			checked = append(checked, name+" (unset)")
		case matchesAny(keys, recipients):
			return true
		default:
			checked = append(checked, name+" (no identity for them)")
		}
		return false
	}
	key, ok := os.LookupEnv(envAgeKey)
	if check(envAgeKey, key, ok && key != "") {
		return checked, true
	}
	file := os.Getenv(envAgeKeyFile)
	name := envAgeKeyFile
	if file != "" {
		name += "=" + file
	}
	if keys, ok := readKeys(file); check(name, keys, ok) {
		return checked, true
	}
	if dir, err := sopsConfigDir(); err == nil {
		def := filepath.Join(dir, "sops", "age", "keys.txt")
		keys, ok := readKeys(def)
		if check(def, keys, ok) {
			return checked, true
		}
		if !ok {
			checked[len(checked)-1] = def + " (absent)"
		}
	}
	return checked, false
}

// sopsConfigDir is the directory sops looks for age/keys.txt under: as
// os.UserConfigDir, but XDG_CONFIG_HOME on macOS too when it is set.
func sopsConfigDir() (string, error) {
	if dir := os.Getenv("XDG_CONFIG_HOME"); runtime.GOOS == "darwin" && dir != "" {
		return dir, nil
	}
	return os.UserConfigDir()
}

// readKeys is the content of an identity file, false when there is none.
func readKeys(path string) (string, bool) {
	if path == "" {
		return "", false
	}
	raw, err := os.ReadFile(path) //nolint:gosec // sops' identity file, read in beekeeper's process
	if err != nil {
		return "", false
	}
	return string(raw), true
}

// matchesAny reports whether keys, identities one per line as sops reads
// them, hold one for any of recipients; a plugin identity counts, since
// only its plugin knows what it decrypts.
func matchesAny(keys string, recipients []string) bool {
	s := bufio.NewScanner(strings.NewReader(keys))
	for s.Scan() {
		line := strings.TrimSpace(s.Text())
		if strings.HasPrefix(line, "AGE-PLUGIN-") {
			return true
		}
		id, err := age.ParseX25519Identity(line)
		if err == nil && slices.Contains(recipients, id.Recipient().String()) {
			return true
		}
	}
	return false
}
