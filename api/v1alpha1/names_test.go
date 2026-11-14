package v1alpha1

import (
	"strings"
	"testing"
)

func TestObjectNames(t *testing.T) {
	t.Parallel()
	tests := []struct {
		ref, item, change string
	}{
		{"giantswarm/roadmap#101", "giantswarm-roadmap-101", "giantswarm-roadmap-pr-101"},
		{"GiantSwarm/Roadmap#101", "giantswarm-roadmap-101", "giantswarm-roadmap-pr-101"},
		{"giantswarm/cluster-api#7", "giantswarm-cluster-api-7", "giantswarm-cluster-api-pr-7"},
	}
	for _, tt := range tests {
		if got := ItemName(tt.ref); got != tt.item {
			t.Errorf("ItemName(%s) = %s, want %s", tt.ref, got, tt.item)
		}
		if got := ChangeName(tt.ref); got != tt.change {
			t.Errorf("ChangeName(%s) = %s, want %s", tt.ref, got, tt.change)
		}
	}
	// Refs the readable form can't tell apart get distinct names.
	refs := []string{"giantswarm/foo-bar#1", "giantswarm/foo.bar#1", "giantswarm/foo_bar#1", "giant-swarm/bar#1",
		"giant/swarm-bar#1", "giantswarm/.github#1", "giantswarm/github#1"}
	seen := map[string]string{}
	for _, ref := range refs {
		name := ItemName(ref)
		if other, ok := seen[name]; ok {
			t.Errorf("%s and %s both name %s", ref, other, name)
		}
		seen[name] = ref
		if len(name) > 253 || strings.Trim(name, "abcdefghijklmnopqrstuvwxyz0123456789-") != "" {
			t.Errorf("%s: %q isn't a valid object name", ref, name)
		}
	}
}
