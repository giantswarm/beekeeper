package secret

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// Recipient is one age recipient of a .sops.yaml creation rule or of a
// SOPS file, with where its identity is.
type Recipient struct {
	// Rule is the creation rule's path_regex, "(every file)" for one
	// without, or the file whose recipients are answered.
	Rule      string `json:"rule"`
	Recipient string `json:"recipient"`
	// Identity is the source of the recipient's identity: sops' own
	// sources, the entry of secret.ageIdentities, the shared vault's item,
	// or [None] with what is missing.
	Identity string `json:"identity"`
}

// None opens the identity of a recipient beekeeper cannot decrypt for.
const None = "none"

// Missing reports whether no identity decrypts for r.
func (r Recipient) Missing() bool { return strings.HasPrefix(r.Identity, None) }

// Recipients answers, for a directory, every age recipient of the creation
// rules of the .sops.yaml nearest above it and, for a SOPS file, the file's
// recipients (its metadata when encrypted, else its creation rule's), each
// with where its identity is: sops' own sources, an entry of
// secret.ageIdentities, the shared vault's item per recipient (checked by
// the vault's item listing, metadata only), or none. No value is read.
func (o *Ops) Recipients(ctx context.Context, path string) ([]Recipient, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	// an absent path is a file still to be written
	fi, err := os.Stat(abs)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	type named struct {
		rule       string
		recipients []string
		file       string
	}
	var rules []named
	if err == nil && fi.IsDir() {
		cfg, err := sopsConfig(abs)
		if err != nil {
			return nil, err
		}
		rs, err := rulesOf(cfg)
		if err != nil {
			return nil, err
		}
		for _, r := range rs {
			if recipients := r.recipients(); len(recipients) > 0 {
				name := r.PathRegex
				if name == "" {
					name = "(every file)"
				}
				rules = append(rules, named{rule: name, recipients: recipients})
			}
		}
		if len(rules) == 0 {
			return nil, fmt.Errorf("%s: no creation rule names an age recipient", cfg)
		}
	} else {
		recipients := ageRecipients(abs)
		if len(recipients) == 0 {
			cfg, rel, err := sopsTarget(abs)
			if err != nil {
				return nil, err
			}
			rule, err := ruleFor(cfg, rel)
			if err != nil {
				return nil, err
			}
			if rule == nil {
				return nil, fmt.Errorf("%s: no creation rule of %s applies to it", path, cfg)
			}
			if recipients = rule.recipients(); len(recipients) == 0 {
				return nil, fmt.Errorf("%s: the creation rule %s names no age recipient", path, rule)
			}
		}
		rules = append(rules, named{rule: path, recipients: recipients, file: abs})
	}
	var titles []string
	listed := false
	var out []Recipient
	for _, n := range rules {
		for _, r := range n.recipients {
			rec := Recipient{Rule: n.rule, Recipient: r}
			switch id := o.configured([]string{r}, n.file); {
			case localAgeIdentityFor(r):
				rec.Identity = "sops' own sources"
			case id != nil:
				rec.Identity = "secret.ageIdentities (" + id.Ref + ")"
			case o.Vault == "":
				rec.Identity = None + ": no entry of secret.ageIdentities names it, and no shared vault (secret.vault) holds an item per recipient"
			default:
				if !listed {
					if titles, err = o.itemTitles(ctx); err != nil {
						return nil, err
					}
					listed = true
				}
				if slices.Contains(titles, AgeItemTitle(r)) {
					rec.Identity = fmt.Sprintf("the vault item %q", AgeItemTitle(r))
				} else {
					rec.Identity = fmt.Sprintf("%s: the vault %s holds no item %q, whose password field is the AGE-SECRET-KEY-1… identity", None, o.Vault, AgeItemTitle(r))
				}
			}
			out = append(out, rec)
		}
	}
	return out, nil
}

// localAgeIdentityFor reports whether one of sops' own sources holds an
// identity of recipient, or is opaque to beekeeper.
func localAgeIdentityFor(recipient string) bool {
	_, found := localAgeIdentities([]string{recipient})
	return found
}
