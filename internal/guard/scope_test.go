package guard

import "testing"

func TestUnder(t *testing.T) {
	dirs := []string{"/home/u/desk", "/home/u/work/"}
	for _, c := range []struct {
		paths []string
		want  bool
	}{
		{[]string{"/home/u/desk"}, true},
		{[]string{"/home/u/desk/repo/sub"}, true},
		{[]string{"/home/u/work/x"}, true},
		{[]string{"/home/u/desk/../own"}, false},
		{[]string{"/home/u/desktop"}, false},
		{[]string{"", "/home/u/own", "/home/u/desk/a"}, true},
		{[]string{"", ""}, false},
		{nil, false},
	} {
		if got := Under(dirs, c.paths...); got != c.want {
			t.Errorf("Under(%q) = %v, want %v", c.paths, got, c.want)
		}
	}
}
