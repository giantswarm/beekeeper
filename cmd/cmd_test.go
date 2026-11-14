package cmd

import (
	"bytes"
	"strings"
	"testing"
)

func TestExecute(t *testing.T) {
	t.Parallel()
	tests := []struct {
		args   []string
		code   int
		stdout string
		stderr string
	}{
		{args: []string{"version"}, code: 0, stdout: "beekeeper "},
		{args: nil, code: 2, stderr: "Commands:"},
		{args: []string{"help"}, code: 0, stderr: "replay"},
		{args: []string{"nope"}, code: 2, stderr: `unknown command "nope"`},
		{args: []string{"engine", "--no-such-flag"}, code: 1, stderr: "flag provided but not defined"},
		{args: []string{"engine", "-h"}, code: 0, stderr: "-github-private-key-file"},
		{args: []string{"receiver", "-h"}, code: 0, stderr: "-project-node-ids"},
	}
	for _, tt := range tests {
		var out, errOut bytes.Buffer
		code := execute(tt.args, &out, &errOut)
		if code != tt.code || !strings.Contains(out.String(), tt.stdout) || !strings.Contains(errOut.String(), tt.stderr) {
			t.Errorf("execute(%v) = %d\nstdout: %s\nstderr: %s", tt.args, code, out.String(), errOut.String())
		}
	}
}
