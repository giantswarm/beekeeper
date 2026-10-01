package post

import "testing"

func TestLinked(t *testing.T) {
	owners := Owners("example/tools", "example/plans")
	for _, c := range []struct{ in, here, want string }{
		{"tools#12 next", "", "[tools#12](https://github.com/example/tools/issues/12) next"},
		{"after #3 merged", "example/plans", "after [plans#3](https://github.com/example/plans/issues/3) merged"},
		{"other/repo#4", "", "[repo#4](https://github.com/other/repo/issues/4)"},
		{"unknown#5 and #6", "", "unknown 5 and 6"},
		{"12/#34 drift", "", "12/34 drift"},
		{"see note #84", "example/plans", "see note 84"},
		{"[tools#1](https://github.com/example/tools/pull/1) and `x#2`", "", "[tools#1](https://github.com/example/tools/pull/1) and `x#2`"},
	} {
		got := Linked(c.in, owners, c.here)
		if got != c.want {
			t.Errorf("Linked(%q) = %q, want %q", c.in, got, c.want)
		}
		if p := Check(got); len(p) > 0 {
			t.Errorf("Linked(%q) fails the check: %v", c.in, p)
		}
	}
}
