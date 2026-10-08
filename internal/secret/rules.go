package secret

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"filippo.io/age"
	"gopkg.in/yaml.v3"
)

// creationRule is what a .sops.yaml creation rule says about the file it
// applies to, the recipients it encrypts for and the keys sops encrypts in it.
type creationRule struct {
	PathRegex               string `yaml:"path_regex"`
	Age                     string `yaml:"age"`
	UnencryptedSuffix       string `yaml:"unencrypted_suffix"`
	EncryptedSuffix         string `yaml:"encrypted_suffix"`
	UnencryptedRegex        string `yaml:"unencrypted_regex"`
	EncryptedRegex          string `yaml:"encrypted_regex"`
	UnencryptedCommentRegex string `yaml:"unencrypted_comment_regex"`
	EncryptedCommentRegex   string `yaml:"encrypted_comment_regex"`
}

// sopsTarget is the .sops.yaml whose creation rules encrypt file and file's
// path relative to it, the name the rules' path_regex match.
func sopsTarget(file string) (cfg, rel string, err error) {
	abs, err := filepath.Abs(file)
	if err != nil {
		return "", "", err
	}
	if cfg, err = sopsConfig(filepath.Dir(abs)); err != nil {
		return "", "", err
	}
	rel, err = filepath.Rel(filepath.Dir(cfg), abs)
	return cfg, rel, err
}

// rulesOf are the creation rules of the .sops.yaml cfg, in their order.
func rulesOf(cfg string) ([]creationRule, error) {
	raw, err := os.ReadFile(cfg) //nolint:gosec // the .sops.yaml above the file
	if err != nil {
		return nil, err
	}
	var c struct {
		CreationRules []creationRule `yaml:"creation_rules"`
	}
	if err := yaml.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("%s: %w", cfg, err)
	}
	return c.CreationRules, nil
}

// ruleFor is the creation rule of cfg that sops applies to rel: the first
// without a path_regex or whose path_regex matches. nil when none does,
// which sops refuses itself.
func ruleFor(cfg, rel string) (*creationRule, error) {
	rules, err := rulesOf(cfg)
	if err != nil {
		return nil, err
	}
	for i := range rules {
		r := &rules[i]
		if r.PathRegex == "" {
			return r, nil
		}
		re, err := regexp.Compile(r.PathRegex)
		if err != nil {
			return nil, fmt.Errorf("%s: path_regex %q: %w", cfg, r.PathRegex, err)
		}
		if re.MatchString(rel) {
			return r, nil
		}
	}
	return nil, nil
}

// recipients are the age recipients r encrypts for, as the rule's age
// setting lists them (comma-separated); nil when it names none or one that
// is not X25519, which beekeeper does not check.
func (r creationRule) recipients() []string {
	var out []string
	for _, s := range strings.Split(r.Age, ",") {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if _, err := age.ParseX25519Recipient(s); err != nil {
			return nil
		}
		out = append(out, s)
	}
	return out
}

// check refuses a dotted path whose value sops would leave in plaintext
// under r, decided as sops decides it from the keys on the path. A rule
// that decides by comments is refused as a whole: a written value carries
// none.
func (r creationRule) check(path string) error {
	if r.UnencryptedCommentRegex != "" || r.EncryptedCommentRegex != "" {
		return fmt.Errorf("%s: the creation rule %s decides by comments, and a written value carries none", path, r)
	}
	keys := strings.Split(path, ".")
	anyKey := func(f func(string) bool) bool { return slices.ContainsFunc(keys, f) }
	matching := func(field, expr string) (func(string) bool, error) {
		re, err := regexp.Compile(expr)
		if err != nil {
			return nil, fmt.Errorf("the creation rule's %s %q: %w", field, expr, err)
		}
		return re.MatchString, nil
	}
	encrypted := true
	if s := r.UnencryptedSuffix; s != "" && anyKey(func(k string) bool { return strings.HasSuffix(k, s) }) {
		encrypted = false
	}
	if s := r.EncryptedSuffix; s != "" {
		encrypted = anyKey(func(k string) bool { return strings.HasSuffix(k, s) })
	}
	if r.UnencryptedRegex != "" {
		m, err := matching("unencrypted_regex", r.UnencryptedRegex)
		if err != nil {
			return err
		}
		if anyKey(m) {
			encrypted = false
		}
	}
	if r.EncryptedRegex != "" {
		m, err := matching("encrypted_regex", r.EncryptedRegex)
		if err != nil {
			return err
		}
		encrypted = anyKey(m)
	}
	if !encrypted {
		return fmt.Errorf("%s would stay plaintext: the creation rule %s encrypts no key on its path", path, r)
	}
	return nil
}

// String names the rule by its path_regex and the settings that decide
// which keys it encrypts.
func (r creationRule) String() string {
	var parts []string
	for _, kv := range [][2]string{
		{"path_regex", r.PathRegex},
		{"unencrypted_suffix", r.UnencryptedSuffix},
		{"encrypted_suffix", r.EncryptedSuffix},
		{"unencrypted_regex", r.UnencryptedRegex},
		{"encrypted_regex", r.EncryptedRegex},
		{"unencrypted_comment_regex", r.UnencryptedCommentRegex},
		{"encrypted_comment_regex", r.EncryptedCommentRegex},
	} {
		if kv[1] != "" {
			parts = append(parts, fmt.Sprintf("%s %q", kv[0], kv[1]))
		}
	}
	if len(parts) == 0 {
		return "(every key)"
	}
	return "(" + strings.Join(parts, ", ") + ")"
}
