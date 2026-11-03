package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/internal/github"
)

const dependabotTestRepo = "giantswarm/example"

func TestSecurityAlertOutput(t *testing.T) {
	for _, jsonOutput := range []bool{false, true} {
		a := &app{json: jsonOutput}
		run := func(context.Context, ...string) ([]byte, error) {
			return []byte(`{"number":8,"state":"fixed","html_url":"https://github.com/giantswarm/example/security/dependabot/8"}`), nil
		}
		c := a.securityAlertCmd(run)
		var out bytes.Buffer
		c.SetOut(&out)
		c.SetArgs([]string{dependabotTestRepo, "8"})
		if err := c.Execute(); err != nil {
			t.Fatal(err)
		}
		if jsonOutput {
			var alert github.DependabotAlert
			if err := json.Unmarshal(out.Bytes(), &alert); err != nil || alert.State != "fixed" || alert.Number != 8 {
				t.Fatalf("output %q: %v", out.String(), err)
			}
		} else if out.String() != "fixed https://github.com/giantswarm/example/security/dependabot/8\n" {
			t.Fatalf("output %q", out.String())
		}
	}
}

func TestSecurityAlertRefusesInvalidAndUnreadable(t *testing.T) {
	for _, args := range [][]string{{}, {dependabotTestRepo}, {dependabotTestRepo, "no"}, {dependabotTestRepo, "0"}, {dependabotTestRepo, "-1"}, {dependabotTestRepo, "9999999999999999999999999"}, {dependabotTestRepo, "8", "surplus-alert-argument"}, {"bad/repo/path", "8"}} {
		c := (&app{}).securityAlertCmd(func(context.Context, ...string) ([]byte, error) {
			t.Fatal("invalid input reached GitHub")
			return nil, nil
		})
		c.SetOut(new(bytes.Buffer))
		c.SetErr(new(bytes.Buffer))
		c.SetArgs(args)
		if err := c.Execute(); err == nil {
			t.Errorf("accepted %q", args)
		}
	}
	c := (&app{}).securityAlertCmd(func(context.Context, ...string) ([]byte, error) { return nil, errors.New("HTTP 403") })
	var out bytes.Buffer
	c.SilenceUsage, c.SilenceErrors = true, true
	c.SetOut(&out)
	c.SetArgs([]string{dependabotTestRepo, "8"})
	if err := c.Execute(); err == nil || !strings.Contains(err.Error(), "HTTP 403") || out.Len() != 0 {
		t.Fatalf("unreadable: output %q, error %v", out.String(), err)
	}
}
