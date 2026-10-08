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
	"time"

	"filippo.io/age"
	"gopkg.in/yaml.v3"

	"github.com/giantswarm/beekeeper/internal/guard"
)

// AgeIdentity is where beekeeper finds the age identity of a SOPS file
// none of sops' own sources holds: a field of the shared vault, an identity
// file on the host or an entry of the person's own credential store, read
// in beekeeper's process and given to the one sops call's environment alone.
type AgeIdentity struct {
	// Recipient is the age recipient (age1…) the identity decrypts for.
	Recipient string
	// Path matches the file's absolute path; nil matches by recipient only.
	Path *regexp.Regexp
	// Ref is the op:// field, the file:// identity file or the store://
	// entry that holds the identity (AGE-SECRET-KEY-1…, an identity file's
	// comments allowed).
	Ref string
	// item is the shared vault's item per recipient ([AgeItemTitle]), the
	// source of a recipient no entry names.
	item bool
}

// The shared vault holds the identity of a recipient no secret.ageIdentities
// entry names in one item per recipient: titled [AgeItemTitle], its password
// field the AGE-SECRET-KEY-1… identity. A person grants an installation's
// recipient once by creating the item; beekeeper reads it like any op://
// field and checks that it exists by the vault's item listing, metadata
// only.
const (
	ageItemPrefix = "sops age key "
	ageItemField  = "password"
)

// AgeItemTitle is the title of the vault item that holds recipient's
// identity.
func AgeItemTitle(recipient string) string { return ageItemPrefix + recipient }

// AgeItemRef is the op:// reference of recipient's identity in vault.
func AgeItemRef(vault, recipient string) string {
	return guard.OpRef + vault + "/" + AgeItemTitle(recipient) + "/" + ageItemField
}

// The references of an age identity besides a vault field.
const (
	// FileRef starts the reference of an identity file on the host.
	FileRef = "file://"
	// StoreRef starts the reference of an entry of the person's own
	// credential store; with no entry, the store's search finds it by the
	// file's recipients.
	StoreRef = "store://"
)

// Store is the person's own credential store, reached through the person's
// own commands: beekeeper never handles the store's password, and the
// store shows whatever unlock prompt it shows.
type Store struct {
	// Read prints the secret of the entry appended as its last argument.
	Read []string
	// Search prints the names of the entries matching the term appended as
	// its last argument, one per line, never a value.
	Search []string
}

// storeTimeout bounds one store command: long enough for the person to
// answer the store's own unlock prompt, never a hang.
const storeTimeout = 5 * time.Minute

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
	var keys []ageKey
	if id.item {
		keys, err = o.itemKeys(ctx, file, recipients)
	} else if keys, err = o.ageKeys(ctx, id.Ref, recipients); err != nil {
		err = fmt.Errorf("%s: the age identity of secret.ageIdentities: %w", file, err)
	}
	if err != nil {
		return nil, err
	}
	var held, from []string
	for _, k := range keys {
		from = append(from, k.source)
		// the parse error is dropped: it may quote the line it failed on
		parsed, err := age.ParseIdentities(strings.NewReader(k.value))
		if err != nil {
			continue
		}
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
	}
	if len(held) == 0 {
		return nil, fmt.Errorf("%s: %s holds no age identity (AGE-SECRET-KEY-1…)", file, strings.Join(from, ", "))
	}
	return nil, fmt.Errorf("%s: %s holds the identity of %s, not of the file's recipients %s", file, strings.Join(from, ", "), strings.Join(held, ", "), strings.Join(recipients, ", "))
}

// ageKey is a value that may hold an age identity, with where it came from.
type ageKey struct {
	source, value string
}

// ageKeys read the identities ref holds: an op:// field of the shared
// vault, a file:// identity file or a store:// entry of the person's own
// credential store (with no entry, every entry the store's search finds for
// one of recipients), read here and nowhere else.
func (o *Ops) ageKeys(ctx context.Context, ref string, recipients []string) ([]ageKey, error) {
	if path, ok := strings.CutPrefix(ref, FileRef); ok {
		raw, err := os.ReadFile(path) //nolint:gosec // the identity file secret.ageIdentities names
		if err != nil {
			return nil, err
		}
		return []ageKey{{ref, string(raw)}}, nil
	}
	entry, ok := strings.CutPrefix(ref, StoreRef)
	if !ok {
		v, err := o.value(ctx, Ref{Op: ref})
		return []ageKey{{ref, v}}, err
	}
	entries := []string{entry}
	if entry == "" {
		var err error
		if entries, err = o.storeSearch(ctx, recipients); err != nil {
			return nil, err
		}
	}
	out := make([]ageKey, 0, len(entries))
	for _, e := range entries {
		v, err := o.storeRun(ctx, o.Store.Read, e)
		if err != nil {
			return nil, err
		}
		out = append(out, ageKey{StoreRef + e, string(v)})
	}
	return out, nil
}

// storeSearch are the entries of the person's own credential store its
// search finds for recipients, each once.
func (o *Ops) storeSearch(ctx context.Context, recipients []string) ([]string, error) {
	var out []string
	for _, r := range recipients {
		found, err := o.storeRun(ctx, o.Store.Search, r)
		if err != nil {
			return nil, err
		}
		for e := range strings.Lines(string(found)) {
			if e = strings.TrimSpace(e); e != "" && !slices.Contains(out, e) {
				out = append(out, e)
			}
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("the person's credential store has no entry for %s", strings.Join(recipients, ", "))
	}
	return out, nil
}

// storeRun runs one of the store's commands on arg and returns its stdout,
// which only the caller sees.
func (o *Ops) storeRun(ctx context.Context, argv []string, arg string) ([]byte, error) {
	if len(argv) == 0 {
		return nil, errors.New("secret.store is not configured: a store:// reference takes secret.store.read, and an empty entry secret.store.search")
	}
	ctx, cancel := context.WithTimeout(ctx, storeTimeout)
	defer cancel()
	return o.Run(ctx, "", nil, nil, argv[0], append(argv[1:len(argv):len(argv)], arg)...)
}

// AgeNeedsVault reports whether decrypting one of the SOPS files args name,
// relative to dir (the working directory when empty), takes an age
// identity from the shared vault.
func (o *Ops) AgeNeedsVault(dir string, args []string) bool {
	return o.ageNeeds(dir, args, func(id *AgeIdentity) bool { return id.item || strings.HasPrefix(id.Ref, guard.OpRef) })
}

// AgeNeedsIdentity reports whether decrypting one of the SOPS files args
// name, relative to dir (the working directory when empty), takes an
// identity of secret.ageIdentities, from the vault, a file or the store.
func (o *Ops) AgeNeedsIdentity(dir string, args []string) bool {
	return o.ageNeeds(dir, args, func(*AgeIdentity) bool { return true })
}

// ageNeeds reports whether one of the SOPS files args name, relative to
// dir, takes an identity of secret.ageIdentities that match accepts.
func (o *Ops) ageNeeds(dir string, args []string, match func(*AgeIdentity) bool) bool {
	if len(o.Ages) == 0 && o.Vault == "" {
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

// ageIdentity is the identity decrypting file takes, with the file's
// recipients: the first configured entry that names one of them or the
// file's path, else the shared vault's item per recipient. nil when the
// file has no age recipients beekeeper can check (none, an SSH or plugin
// recipient, another key group), when one of sops' own sources holds an
// identity for one of them, or when one of those sources is opaque to
// beekeeper.
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
	if id := o.configured(recipients, abs); id != nil {
		return id, recipients, nil
	}
	if o.Vault != "" {
		return &AgeIdentity{item: true}, recipients, nil
	}
	return nil, nil, fmt.Errorf("%s: %w for its recipients %s: checked %s and secret.ageIdentities, and no shared vault (secret.vault) holds an item per recipient; "+
		"an entry there (recipient or pathRegex, and ref: op://<vault>/<item>/<field>, file://<identity file> or store://[<entry>] holding the AGE-SECRET-KEY-1… identity) gives beekeeper one",
		file, ErrNoAgeIdentity, strings.Join(recipients, ", "), strings.Join(checked, ", "))
}

// configured is the first entry of secret.ageIdentities that names one of
// recipients or matches the absolute path abs, nil when none does.
func (o *Ops) configured(recipients []string, abs string) *AgeIdentity {
	for i, id := range o.Ages {
		if slices.Contains(recipients, id.Recipient) || abs != "" && id.Path != nil && id.Path.MatchString(abs) {
			return &o.Ages[i]
		}
	}
	return nil
}

// itemKeys read the shared vault's items of recipients, [AgeItemTitle]
// each, those the vault's item listing names (metadata, no value): the
// identity of a recipient no secret.ageIdentities entry names. With none,
// the call fails in one line naming the installation, the recipient and the
// item the vault lacks: never a person's own sops run.
func (o *Ops) itemKeys(ctx context.Context, file string, recipients []string) ([]ageKey, error) {
	titles, err := o.itemTitles(ctx)
	if err != nil {
		return nil, fmt.Errorf("%s: the age identity in the vault: %w", file, err)
	}
	var out []ageKey
	var missing []string
	for _, r := range recipients {
		if !slices.Contains(titles, AgeItemTitle(r)) {
			missing = append(missing, fmt.Sprintf("%q", AgeItemTitle(r)))
			continue
		}
		ref := AgeItemRef(o.Vault, r)
		v, err := o.value(ctx, Ref{Op: ref})
		if err != nil {
			return nil, fmt.Errorf("%s: the age identity in the vault: %w", file, err)
		}
		out = append(out, ageKey{ref, v})
	}
	if len(out) == 0 {
		whose := "its"
		if inst := o.installation(file); inst != "" {
			whose = inst + "'s"
		}
		return nil, fmt.Errorf("%s: %w for %s recipient %s: the vault %s holds no item %s, whose password field is the AGE-SECRET-KEY-1… identity",
			file, ErrNoAgeIdentity, whose, strings.Join(recipients, ", "), o.Vault, strings.Join(missing, " or "))
	}
	return out, nil
}

// itemTitles are the titles of the shared vault's items: the listing names
// no value.
func (o *Ops) itemTitles(ctx context.Context) ([]string, error) {
	items, err := o.vaultItems(ctx, o.Vault)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrVault, err)
	}
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, it.Title)
	}
	return out, nil
}

// installation is the one of Installations that file's path names, as a
// directory under the .sops.yaml above it (management-clusters/<name>/…),
// "" when none.
func (o *Ops) installation(file string) string {
	_, rel, err := sopsTarget(file)
	if err != nil {
		return ""
	}
	for _, seg := range strings.Split(filepath.ToSlash(rel), "/") {
		if slices.Contains(o.Installations, seg) {
			return seg
		}
	}
	return ""
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
