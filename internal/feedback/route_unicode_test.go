package feedback_test

import (
	"testing"

	"github.com/giantswarm/beekeeper/internal/feedback"
)

func TestRouteUnicodeCaseFold(t *testing.T) {
	for _, c := range []struct{ name, text string }{
		{"Maße", "MAẞE: check the CI"},
		{"MAẞE", "Maße: check the CI"},
		{"Keeper", "Keeper: check the CI"},
		{"Keeper", "keeper: check the CI"},
	} {
		t.Run(c.name+"/"+c.text, func(t *testing.T) {
			to, msg := feedback.Route(c.text, []string{c.name})
			if to != c.name || msg != "check the CI" {
				t.Fatalf("Route(%q, %q) = (%q, %q); want (%q, %q)", c.text, c.name, to, msg, c.name, "check the CI")
			}
		})
	}
}

func TestRouteColonNamesAndMessages(t *testing.T) {
	for _, c := range []struct {
		name, text, to, msg string
		names               []string
	}{
		{"longest colon name", "board: ci: check: the CI", "Board: CI", "check: the CI", []string{"Board", "Board: CI"}},
		{"longest Unicode name", "MAẞE: CI: check the CI", "Maße: CI", "check the CI", []string{"Maße", "Maße: CI"}},
		{"longest text prefix", "KKK: X: check the CI", "kkk: x", "check the CI", []string{"KKK", "kkk: x"}},
		{"empty longest message", "Board: CI:", "Board", "CI:", []string{"Board", "Board: CI"}},
		{"blank message", "MAẞE: \t\n", "", "MAẞE: \t\n", []string{"Maße"}},
		{"longer unmatched prefix", "MAẞE extra: check the CI", "", "MAẞE extra: check the CI", []string{"Maße"}},
		{"empty name", ": check the CI", "", ": check the CI", []string{""}},
		{"empty input", "", "", "", []string{"Maße"}},
		{"no names", "Maße: check the CI", "", "Maße: check the CI", nil},
		{"simple fold only", "STRASSE: check the CI", "", "STRASSE: check the CI", []string{"Straße"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			to, msg := feedback.Route(c.text, c.names)
			if to != c.to || msg != c.msg {
				t.Fatalf("Route(%q, %q) = (%q, %q); want (%q, %q)", c.text, c.names, to, msg, c.to, c.msg)
			}
		})
	}
}
