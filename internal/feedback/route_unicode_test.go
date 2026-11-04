package feedback_test

import (
	"testing"

	"github.com/giantswarm/beekeeper/internal/feedback"
)

const (
	routeBoardColonName   = "Board: CI"
	routeUnicodeColonName = "Maße: CI"
	routeUnicodeName      = "Maße"
	routeUnicodeReply     = "Maße: check the CI"
	routeMessage          = "check the CI"
	routeBoardName        = "Board"
)

func TestRouteUnicodeCaseFold(t *testing.T) {
	for _, c := range []struct{ name, text string }{
		{routeUnicodeName, "MAẞE: check the CI"},
		{"MAẞE", routeUnicodeReply},
		{"Keeper", "Keeper: check the CI"},
		{"Keeper", "keeper: check the CI"},
	} {
		t.Run(c.name+"/"+c.text, func(t *testing.T) {
			to, msg := feedback.Route(c.text, []string{c.name})
			if to != c.name || msg != routeMessage {
				t.Fatalf("Route(%q, %q) = (%q, %q); want (%q, %q)", c.text, c.name, to, msg, c.name, routeMessage)
			}
		})
	}
}

func TestRouteColonNamesAndMessages(t *testing.T) {
	for _, c := range []struct {
		name, text, to, msg string
		names               []string
	}{
		{"longest colon name", "board: ci: check: the CI", routeBoardColonName, "check: the CI", []string{routeBoardName, routeBoardColonName}},
		{"longest Unicode name", "MAẞE: CI: check the CI", routeUnicodeColonName, routeMessage, []string{routeUnicodeName, routeUnicodeColonName}},
		{"longest text prefix", "KKK: X: check the CI", "kkk: x", routeMessage, []string{"KKK", "kkk: x"}},
		{"longest name first", "MAẞE: CI: check the CI", routeUnicodeColonName, routeMessage, []string{routeUnicodeColonName, routeUnicodeName}},
		{"Unicode message whitespace", "MAẞE: \u2003check the CI\u00a0", routeUnicodeName, routeMessage, []string{routeUnicodeName}},
		{"empty longest message", "Board: CI:", routeBoardName, "CI:", []string{routeBoardName, routeBoardColonName}},
		{"blank message", "MAẞE: \t\n", "", "MAẞE: \t\n", []string{routeUnicodeName}},
		{"longer unmatched prefix", "MAẞE extra: check the CI", "", "MAẞE extra: check the CI", []string{routeUnicodeName}},
		{"empty name", ": check the CI", "", ": check the CI", []string{""}},
		{"empty input", "", "", "", []string{routeUnicodeName}},
		{"no names", routeUnicodeReply, "", routeUnicodeReply, nil},
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
