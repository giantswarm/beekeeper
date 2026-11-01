package secret

import (
	"context"
	"fmt"
	"slices"
	"strings"
)

const (
	storeFieldTemplate = "{field}"
	storeAttributeFlag = "--attributes"
)

// parseStoreRef reads one entry and optional attribute of the person's store.
func parseStoreRef(s string) (Ref, error) {
	entry, field, hasField := strings.Cut(strings.TrimPrefix(s, StoreRef), "#")
	if entry == "" || strings.HasPrefix(entry, "-") || strings.ContainsAny(entry+field, "\x00\r\n") || hasField && (field == "" || strings.Contains(field, "#")) {
		return Ref{}, fmt.Errorf("%q: a store source is store://<entry>[#<field>]", s)
	}
	return Ref{Store: entry, Path: field}, nil
}

func storeFieldSuffix(field string) string {
	if field == "" {
		return ""
	}
	return "#" + field
}

// NeedsStore reports a store source before the consumer separator, including
// the sources of a multi-value copy. Callers must preserve the separator or
// pass only arguments before it.
func NeedsStore(args []string) bool {
	for _, arg := range args {
		if arg == "--" {
			break
		}
		if strings.HasPrefix(arg, StoreRef) {
			return true
		}
	}
	return false
}

// readStore never returns the store command's diagnostics: even an error can
// carry an attribute's value. A field template supports arbitrary adapters;
// without one an explicit field uses KeePassXC's --attributes option.
func (o *Ops) readStore(ctx context.Context, r Ref) (string, error) {
	if len(o.Store.Read) == 0 {
		return "", fmt.Errorf("%s: configure secret.store.read to read the person's credential store", r)
	}
	argv := slices.Clone(o.Store.Read)
	field := r.Path
	if field == "" {
		field = "password"
	}
	template := false
	for i := 1; i < len(argv); i++ {
		if strings.Contains(argv[i], storeFieldTemplate) {
			argv[i] = strings.ReplaceAll(argv[i], storeFieldTemplate, field)
			template = true
		}
	}
	if r.Path != "" && !template {
		argv = storeAttributes(argv, field)
	}
	raw, err := o.storeRun(ctx, argv, r.Store)
	if err != nil {
		return "", fmt.Errorf("%s: credential store read failed (entry or field missing, store locked, command failed or timed out)", r)
	}
	return strings.TrimSuffix(strings.TrimSuffix(string(raw), "\n"), "\r"), nil
}

// storeAttributes replaces KeePassXC selectors, including attached short
// values and combined flags, so a copy never reads the default field as well.
func storeAttributes(argv []string, field string) []string {
	out := []string{argv[0]}
	for i := 1; i < len(argv); i++ {
		arg := argv[i]
		if arg == "--" {
			out = append(out, storeAttributeFlag, field)
			return append(out, argv[i:]...)
		}
		if arg == storeAttributeFlag {
			i++
			continue
		}
		if strings.HasPrefix(arg, "--attributes=") {
			continue
		}
		if strings.HasPrefix(arg, "-") && !strings.HasPrefix(arg, "--") {
			for j := 1; j < len(arg); j++ {
				switch arg[j] {
				case 'a':
					if j+1 == len(arg) {
						i++
					}
					// The remaining characters, if any, are the attribute's value.
					arg = arg[:j]
					if arg == "-" {
						arg = ""
					}
				case 'k', 'y':
					// Key-file and YubiKey options take a value, not more short flags.
					if j+1 == len(arg) && i+1 < len(argv) {
						out = append(out, arg, argv[i+1])
						i++
						arg = ""
					}
				default:
					continue
				}
				break
			}
		}
		if arg != "" {
			out = append(out, arg)
		}
	}
	return append(out, storeAttributeFlag, field)
}
