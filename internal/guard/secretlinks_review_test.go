package guard

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// These reads were refused by origin/main and allowed by the reviewed change.
// Commands are only inspected, using synthetic files outside the working tree.
func TestSecretLinkReviewedReads(t *testing.T) {
	home, work := t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))
	for _, p := range []string{filepath.Join(home, ".tool"), filepath.Join(home, ".codex"), filepath.Join(work, "sub")} {
		if err := os.MkdirAll(p, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"auth.json", "other.json"} {
		if err := os.WriteFile(filepath.Join(home, ".tool", name), []byte(`{"token":"synthetic"}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, p := range []string{filepath.Join(home, "existing-link.json"), filepath.Join(work, "existing-link.json")} {
		if err := os.Symlink(filepath.Join(home, ".tool", "auth.json"), p); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(work, "installer.sh"), []byte("ln -s \"$HOME/.tool/auth.json\" new.json\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	h := Hook{SecretFiles: []string{"~/.tool/auth.json", "~/.tool/other.json"}}
	for _, tc := range []struct {
		name, command string
		adapters      []string
	}{
		{"param_read", "link() { ln -sfn \"${1:?}\" \"${2:?}\"; }; link \"$HOME/.tool/auth.json\" new.json; cat new.json", []string{"bash", "codex"}},
		{"default_read", "link() { ln -sfn \"$1\" \"${2:-new.json}\"; }; link \"$HOME/.tool/auth.json\" new.json; cat new.json", []string{"bash", "codex"}},
		{"star_read", "link() { ln -sfn $*; }; link \"$HOME/.tool/auth.json\" new.json; cat new.json", []string{"bash", "codex"}},
		{"local_substitution", "link() { local dst=\"$(dirname \"$2\")/$(basename \"$2\")\"; ln -sfn \"$1\" \"$dst\"; }; link \"$HOME/.tool/auth.json\" new.json; cat new.json", []string{"bash", "codex"}},
		{"loop", "link() { for f in \"$@\"; do ln -s \"$f\" .; done; }; link \"$HOME/.tool/auth.json\"; cat auth.json", []string{"bash", "codex"}},
		{"printf_source", "link() { printf -v v \"%s\" \"$1\"; ln -s \"$v\" \"$2\"; }; link \"$HOME/.tool/auth.json\" new.json; cat new.json", []string{"bash", "codex"}},
		{"cd_relative", "cd \"$HOME/.tool\" && ln -s auth.json \"$WORK/new.json\" && cat \"$WORK/new.json\"", []string{"codex"}},
		{"trailing_slash", "ln -s \"$HOME/.tool/auth.json\" newdir/; cat newdir/auth.json", []string{"bash", "codex"}},
		{"glob_source", "ln -s \"$HOME/.tool/\"*auth* multi; jq 'del(.. | .foo?)' multi/auth.json", []string{"bash", "codex"}},
		{"read_glob", "ln -s \"$HOME/.tool/auth.json\" new1.json; cat new*", []string{"bash", "codex"}},
		{"recursive_grep", "ln -s \"$HOME/.tool/auth.json\" new1.json; rg token .", []string{"bash", "codex"}},
		{"nested_shell", "bash -c 'ln -s \"$HOME/.tool/auth.json\" new.json'; cat new.json", []string{"bash", "codex"}},
		{"nested_eval", "eval 'ln -s \"$HOME/.tool/auth.json\" new.json'; cat new.json", []string{"bash", "codex"}},
		{"nested_script", "bash installer.sh && cat new.json", []string{"bash", "codex"}},
		{"nested_heredoc", "bash <<'END'\nln -s \"$HOME/.tool/auth.json\" new.json\nEND\ncat new.json", []string{"bash", "codex"}},
		{"nested_python", "ln -s \"$HOME/.tool/auth.json\" new.json; python3 -c 'open(\"new.json\").read()'", []string{"bash", "codex"}},
		{"link_input_substitution", "ln -s \"$HOME/.tool/auth.json\" new.json; echo \"$(<new.json)\"", []string{"bash", "codex"}},
		{"link_backtick", "ln -s \"$HOME/.tool/auth.json\" new.json; echo \"` <new.json `\"", []string{"bash", "codex"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, mode := range []string{"direct", "script", "check"} {
				cmd := strings.ReplaceAll(tc.command, "$WORK", work)
				if mode != "direct" {
					p := filepath.Join(work, "review.sh")
					if err := os.WriteFile(p, []byte(cmd+"\n"), 0o600); err != nil {
						t.Fatal(err)
					}
					cmd = "bash " + ShellQuote(p)
					if mode == "check" {
						cmd += " --check"
					}
				}
				t.Run(mode, func(t *testing.T) {
					for _, adapter := range tc.adapters {
						t.Run(adapter, func(t *testing.T) {
							deny := false
							if adapter == "codex" {
								deny = decideCodexCmd(t, h, work, cmd).PermissionDecision == decisionDeny
							} else if d := decide(t, h, work, cmd, nil); d != nil {
								deny = d.PermissionDecision == decisionDeny
							}
							if !deny {
								t.Error("refused = false, want true")
							}
						})
					}
				})
			}
		})
	}
}
