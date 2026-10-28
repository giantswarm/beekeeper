//go:build linux && !nosystemd

package platform

import (
	"slices"
	"strings"
	"testing"
)

func TestExecutionScopeStopsWithExistingOwnerWithoutStartingIt(t *testing.T) {
	owner := "beekeeper-execution-fixture.service"
	args := scopeArgs("fixture-build.scope", Cap{Max: "1G", Swap: "0", StopWith: owner}, []string{"true"})
	for _, property := range []string{"Requisite=" + owner, "PartOf=" + owner, "After=" + owner} {
		if !slices.Contains(args, property) {
			t.Errorf("missing owned lifecycle property %s", property)
		}
	}
	for _, arg := range args {
		if strings.HasPrefix(arg, "Requires=") || strings.HasPrefix(arg, "BindsTo=") {
			t.Fatal("child scope may start an inactive owner")
		}
	}
	local := scopeArgs("fixture-build.scope", Cap{Max: "1G", Swap: "0"}, []string{"true"})
	for _, arg := range local {
		if strings.Contains(arg, owner) {
			t.Fatal("local scope inherited remote ownership")
		}
	}
}
