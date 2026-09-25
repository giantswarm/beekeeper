package guard

import (
	"strings"
	"testing"
)

func TestCommandHead(t *testing.T) {
	for _, c := range []struct {
		argv []string
		want string
	}{
		{[]string{"zsh", "-c", "cd /x &&\n  go test ./...  "}, "cd /x && go test ./..."},
		{[]string{"/usr/bin/bash", "-c", "make test", "arg0"}, "make test"},
		{[]string{"python3", "-c", "bytearray(200<<20)"}, "python3 -c bytearray(200<<20)"},
		{[]string{"go", "test", strings.Repeat("x", 200)}, "go test " + strings.Repeat("x", 91) + "…"},
	} {
		if got := CommandHead(c.argv); got != c.want {
			t.Errorf("CommandHead(%q) = %q, want %q", c.argv, got, c.want)
		}
	}
}

func TestRunEventDetail(t *testing.T) {
	d := "memcap-2516344-492870.scope slot 1 max 12G: go test ./... -run 'A: B'"
	if RunScope(d) != "memcap-2516344-492870.scope" || RunCommand(d) != "go test ./... -run 'A: B'" {
		t.Errorf("scope %q, command %q", RunScope(d), RunCommand(d))
	}
}
