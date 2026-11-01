package secret

import (
	"context"
	"fmt"
	"slices"
	"strings"
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
// the sources of a multi-value copy. Store reads always belong to the broker.
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
// without one an explicit field uses KeePassXC's --attribute option.
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
		if strings.Contains(argv[i], "{field}") {
			argv[i] = strings.ReplaceAll(argv[i], "{field}", field)
			template = true
		}
	}
	if r.Path != "" && !template {
		argv = append(argv, "--attribute", field)
	}
	raw, err := o.storeRun(ctx, argv, r.Store)
	if err != nil {
		return "", fmt.Errorf("%s: credential store read failed (entry or field missing, store locked, command failed or timed out)", r)
	}
	return strings.TrimSuffix(strings.TrimSuffix(string(raw), "\n"), "\r"), nil
}
