package guard

import (
	"os"
	"path/filepath"
	"testing"
)

// Commands are inspected against synthetic files, never executed.
func TestSecretLinkRoundTwoProbes(t *testing.T) {
	home, work := t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", filepath.Join(home, ".tool"))
	for _, p := range []string{filepath.Join(home, ".tool"), filepath.Join(work, "sub")} {
		if err := os.MkdirAll(p, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{authFixtureName, "other.json"} {
		if err := os.WriteFile(filepath.Join(home, ".tool", name), []byte(`{"token":"synthetic"}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	h := Hook{SecretFiles: []string{authFixturePath, otherFixturePath}}
	for _, tc := range []struct {
		name, command string
		deny          bool
		adapters      []string
	}{
		{"helper_glob_read", "link() { ln -sfn \"$1\" \"$2\"; }; link \"$HOME/.tool/\"*auth* new.json; cat new.json", true, []string{bashCmd, secretLinkCodex}},
		{"helper_glob_strip", "link() { ln -sfn \"$1\" \"$2\"; }; link \"$HOME/.tool/\"*auth* new.json; jq 'del(.. | .foo?)' new.json", true, []string{bashCmd, secretLinkCodex}},
		{"helper_glob_write", "link() { ln -sfn \"$1\" \"$2\"; }; link \"$HOME/.tool/auth.json\" \"$HOME/.tool/hooks.json\".*", true, []string{secretLinkCodex}},
		{"spread_multiple_read", "link() { ln -sfn \"$@\"; }; link \"$HOME/.tool/auth.json\" \"$HOME/.tool/other.json\" newdir; cat newdir/auth.json", true, []string{bashCmd, secretLinkCodex}},
		{"spread_target_read", "link() { ln -sfn -t newdir \"$@\"; }; link \"$HOME/.tool/auth.json\" \"$HOME/.tool/other.json\"; cat newdir/auth.json", true, []string{bashCmd, secretLinkCodex}},
		{"spread_multiple_write", "link() { ln -sfn \"$@\"; }; link \"$HOME/.tool/auth.json\" \"$HOME/.tool/other.json\" \"$HOME/.tool\"", true, []string{secretLinkCodex}},
		{"helper_link_directory", "ln -s \"$HOME/.tool/auth.json\" sub/auth.json; link() { ln -sfn \"$1\" \"$2\"; }; link sub linkdir; cat linkdir/auth.json", true, []string{bashCmd, secretLinkCodex}},
		{"helper_mkdir", "link() { mkdir -p \"$2\"; ln -sfn \"$1\" new.json; }; link \"$HOME/.tool/auth.json\" \"$HOME/.tool/sub\"", true, []string{secretLinkCodex}},
		{"empty_assignment_read", "link() { local pfx=safe/; pfx=; ln -sfn \"$1\" \"${pfx}$2\"; }; link \"$HOME/.tool/auth.json\" new.json; cat new.json", true, []string{bashCmd, secretLinkCodex}},
		{"empty_assignment_write", "link() { local pfx=safe/; pfx=; ln -sfn \"$1\" \"${pfx}$2\"; }; link \"$HOME/.tool/auth.json\" \"$HOME/.tool/hooks.json\"", true, []string{secretLinkCodex}},
		{"branch_assignment_read", "link() { local dst=\"$2\"; if false; then dst=\"$3\"; fi; ln -sfn \"$1\" \"$dst\"; }; link \"$HOME/.tool/auth.json\" new.json safe.json; cat new.json", true, []string{bashCmd, secretLinkCodex}},
		{"branch_assignment_write", "link() { local dst=\"$2\"; if false; then dst=\"$3\"; fi; ln -sfn \"$1\" \"$dst\"; }; link \"$HOME/.tool/auth.json\" \"$HOME/.tool/hooks.json\" safe.json", true, []string{secretLinkCodex}},
		{"overwritten_destination_read", "dst=new.json; ln -s \"$HOME/.tool/auth.json\" \"$dst\"; dst=; cat new.json", true, []string{bashCmd, secretLinkCodex}},
		{"overwritten_destination_write", "dst=\"$HOME/.tool/hooks.json\"; ln -s \"$HOME/.tool/auth.json\" \"$dst\"; dst=", true, []string{secretLinkCodex}},
		{"loop_variable_read", "for dst in a b; do :; done; dst=new.json; link() { ln -sfn \"$1\" \"$dst\"; }; link \"$HOME/.tool/auth.json\"; cat new.json", true, []string{bashCmd, secretLinkCodex}},
		{"assigned_destination", "dst=new.json; ln -s \"$HOME/.tool/auth.json\" \"$dst\"", false, []string{bashCmd, secretLinkCodex}},
		{"assigned_source_destination", "src=\"$HOME/.tool/auth.json\"; dst=new.json; ln -s \"$src\" \"$dst\"", false, []string{bashCmd, secretLinkCodex}},
		{"nested_chain", "bash -c 'ln -s \"$HOME/.tool/auth.json\" link1.json'; ln -s link1.json link2.json; cat link2.json", true, []string{bashCmd, secretLinkCodex}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, adapter := range tc.adapters {
				t.Run(adapter, func(t *testing.T) {
					denied := false
					if adapter == secretLinkCodex {
						denied = decideCodexCmd(t, h, work, tc.command).PermissionDecision == decisionDeny
					} else if d := decide(t, h, work, tc.command, nil); d != nil {
						denied = d.PermissionDecision == decisionDeny
					}
					t.Logf("refused = %v", denied)
					if denied != tc.deny {
						t.Errorf("refused = %v, want %v", denied, tc.deny)
					}
				})
			}
		})
	}
}
