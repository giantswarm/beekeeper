package secret

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/giantswarm/beekeeper/internal/guard"
)

// Item is what items answers of one vault item: its metadata and, when
// asked, its fields' names, types and lengths, never a value.
type Item struct {
	Title    string      `json:"title"`
	ID       string      `json:"id"`
	Category string      `json:"category,omitempty"`
	Tags     []string    `json:"tags,omitempty"`
	Updated  string      `json:"updated,omitempty"`
	Fields   []FieldInfo `json:"fields,omitempty"`
	EnvKeys  []EnvKey    `json:"envKeys,omitempty"`
}

// FieldInfo is one field of an item: its label, the section it sits in,
// its type and purpose, and the length of its value in bytes.
type FieldInfo struct {
	Label   string `json:"label"`
	Section string `json:"section,omitempty"`
	Type    string `json:"type,omitempty"`
	Purpose string `json:"purpose,omitempty"`
	Length  int    `json:"length"`
}

// ItemsOptions narrow a listing.
type ItemsOptions struct {
	// Match keeps the items whose title, category or a tag contains it,
	// case-insensitively; empty keeps every item.
	Match string
	// Fields reads each kept item for its fields' names and lengths.
	Fields bool
	// EnvKeys reads each kept item for the keys of its env-style fields'
	// KEY=VALUE lines and their values' lengths.
	EnvKeys bool
}

// ListVaults are the vaults items lists: secret.vault, secret.vaults and
// secret.ageVaults, those beekeeper reads.
func (o *Ops) ListVaults() []string {
	return slices.Compact(append(o.ReadVaults(), o.AgeVaults...))
}

// Items lists the items of vault, one of [Ops.ListVaults]: titles and
// metadata, with opt.Fields each field's label, type and length, with
// opt.EnvKeys each KEY of a field's KEY=VALUE lines and its value's length.
// A value is read in this process for its length alone and answered
// nowhere.
func (o *Ops) Items(ctx context.Context, vault string, opt ItemsOptions) ([]Item, error) {
	ref := VaultRef(vault)
	if vault == "" || strings.Contains(vault, "/") {
		return nil, fmt.Errorf("%q: name a vault, op://<vault>", ref)
	}
	if err := o.checkVaultIn(Ref{Op: ref + "/"}, o.ListVaults()...); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()
	listed, err := o.vaultItems(ctx, vault)
	if err != nil {
		return nil, opErr(ctx, ref, err)
	}
	var out []Item
	for _, it := range listed {
		item := Item{Title: it.Title, ID: it.ID, Category: it.Category, Tags: it.Tags, Updated: it.UpdatedAt}
		if !item.matches(opt.Match) {
			continue
		}
		if opt.Fields || opt.EnvKeys {
			fields, keys, err := o.itemFields(ctx, vault, it.ID)
			if err != nil {
				return nil, opErr(ctx, ref+"/"+it.Title, err)
			}
			if opt.Fields {
				item.Fields = fields
			}
			if opt.EnvKeys {
				item.EnvKeys = keys
			}
		}
		out = append(out, item)
	}
	slices.SortStableFunc(out, func(a, b Item) int { return strings.Compare(strings.ToLower(a.Title), strings.ToLower(b.Title)) })
	return out, nil
}

// VaultRef is the reference that names a whole vault, op://<vault>.
func VaultRef(vault string) string { return guard.OpRef + vault }

// opErr is a failed op call on ref: a vault error, op's silence named.
func opErr(ctx context.Context, ref string, err error) error {
	if ctx.Err() != nil {
		return fmt.Errorf("%w: %s: op answered nothing in %s", ErrVault, ref, opTimeout)
	}
	return fmt.Errorf("%w: %s: %w", ErrVault, ref, err)
}

// matches reports whether the item's title, category or a tag contains
// match, case-insensitively.
func (it Item) matches(match string) bool {
	m := strings.ToLower(match)
	for _, s := range append([]string{it.Title, it.Category}, it.Tags...) {
		if strings.Contains(strings.ToLower(s), m) {
			return true
		}
	}
	return false
}

// itemFields reads the item id of vault and keeps of each field its
// metadata and its value's length, and the keys of its KEY=VALUE lines
// with their values' lengths; the values go out of scope here.
func (o *Ops) itemFields(ctx context.Context, vault, id string) ([]FieldInfo, []EnvKey, error) {
	raw, err := o.op(ctx, nil, "item", "get", id, "--vault", vault, "--format", "json")
	if err != nil {
		return nil, nil, err
	}
	var item struct {
		Fields []struct {
			ID      string `json:"id"`
			Label   string `json:"label"`
			Type    string `json:"type"`
			Purpose string `json:"purpose"`
			Value   string `json:"value"`
			Section struct {
				Label string `json:"label"`
			} `json:"section"`
		} `json:"fields"`
	}
	if err := json.Unmarshal(raw, &item); err != nil {
		return nil, nil, errors.New("op item get answered no item")
	}
	out := make([]FieldInfo, 0, len(item.Fields))
	var keys []EnvKey
	for _, f := range item.Fields {
		label := f.Label
		if label == "" {
			label = f.ID
		}
		out = append(out, FieldInfo{Label: label, Section: f.Section.Label, Type: f.Type, Purpose: f.Purpose, Length: len(f.Value)})
		// the field as op read names it: a built-in one by its id, one in
		// a section after the section
		name := label
		if _, builtin := builtins[f.ID]; builtin {
			name = f.ID
		} else if f.Section.Label != "" {
			name = f.Section.Label + "/" + label
		}
		keys = append(keys, envKeys(name, f.Value)...)
	}
	return out, keys, nil
}
