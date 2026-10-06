package sandbox

import (
	"os"
	"slices"
	"testing"
)

func TestUnheld(t *testing.T) {
	for _, tc := range []struct {
		env  map[string]string
		want bool
	}{
		{map[string]string{}, false},
		{map[string]string{Env: "1"}, true},
		{map[string]string{Env: "1", Runtime: "1"}, false},
		{map[string]string{Env: "1", Brokered: "1"}, false},
	} {
		if got := Unheld(func(k string) string { return tc.env[k] }); got != tc.want {
			t.Errorf("Unheld(%v) = %v, want %v", tc.env, got, tc.want)
		}
	}
}

// An unheld environment drops every egress variable and Env, git's
// credential helper override with them, so the host's own configuration
// applies again.
func TestUnconfine(t *testing.T) {
	const dir = "/run/user/1/beekeeper/egress"
	for k, v := range egressVars(dir) {
		t.Setenv(k, v)
		if !slices.Contains(Unset(dir), k) {
			t.Errorf("%s not unset", k)
		}
	}
	t.Setenv(Env, "1")
	Unconfine(dir)
	for _, k := range []string{Env, ghConfigDir, sslCertFile, gitConfigCount, "GIT_CONFIG_VALUE_2"} {
		if v, ok := os.LookupEnv(k); ok {
			t.Errorf("%s=%q still set", k, v)
		}
	}
}
