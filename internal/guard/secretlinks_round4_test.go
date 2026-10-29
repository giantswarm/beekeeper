package guard

import (
	"os"
	"path/filepath"
	"testing"
)

// These review probes only inspect commands and synthetic files.
func TestSecretLinkRoundThreeProbes(t *testing.T) {
	home, work := t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", filepath.Join(home, ".tool"))
	if err := os.MkdirAll(filepath.Join(home, ".tool"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".tool", "auth.json"), []byte(`{"token":"synthetic"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	h := Hook{SecretFiles: []string{authFixturePath}}
	for _, tc := range []struct{ name, command, script string }{
		{"parameter_default", `src="$HOME/.tool/auth.json"; cat "${src:-}"`, ""},
		{"parameter_replacement", `src="$HOME/.tool/auth.json"; cat "${src/a/a}"`, ""},
		{"concatenated_variables", `src="$HOME/.tool/auth.json"; pfx=""; cat "$pfx$src"`, ""},
		{"indirect_variable", `src="$HOME/.tool/auth.json"; ref=src; cat "${!ref}"`, ""},
		{"find_script", `find . .. -maxdepth 0 -exec cat new.json \; -o -exec bash installer.sh \;`, `link() { ln -sfn "$1" "$2"; }; link "$HOME/.tool/auth.json" new.json`},
		{"sed_script", `sed -e 'e' -e '$e cat new.json' installer.sh`, `link() { ln -sfn "$1" "$2"; }; link "$HOME/.tool/auth.json" new.json`},
		{"script_home_write", installerCommand, `ln -sfn "$HOME/.tool/auth.json" "$HOME/.tool/hooks.json"`},
		{"helper_script_home_write", installerCommand, `link() { ln -sfn "$1" "$2"; }; link "$HOME/.tool/auth.json" "$HOME/.tool/hooks.json"`},
		{"script_home_write_check", `bash installer.sh --check`, `ln -sfn "$HOME/.tool/auth.json" "$HOME/.tool/hooks.json"`},
		{"helper_script_home_write_check", `bash installer.sh --check`, `link() { ln -sfn "$1" "$2"; }; link "$HOME/.tool/auth.json" "$HOME/.tool/hooks.json"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.WriteFile(filepath.Join(work, "installer.sh"), []byte(tc.script+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			adapters := []string{bashCmd, secretLinkCodex}
			if tc.name == "script_home_write" || tc.name == "helper_script_home_write" || tc.name == "script_home_write_check" || tc.name == "helper_script_home_write_check" {
				adapters = []string{secretLinkCodex}
			}
			for _, adapter := range adapters {
				t.Run(adapter, func(t *testing.T) {
					denied := false
					if adapter == secretLinkCodex {
						denied = decideCodexCmd(t, h, work, tc.command).PermissionDecision == decisionDeny
					} else if d := decide(t, h, work, tc.command, nil); d != nil {
						denied = d.PermissionDecision == decisionDeny
					}
					if !denied {
						t.Error("refused = false, want true")
					}
				})
			}
		})
	}
}
