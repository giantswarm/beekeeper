package guard

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReview1872FindAlias(t *testing.T) {
	home, work, h := secretGapFixture(t)
	if err := os.Symlink(filepath.Join(home, ".tool", "auth.json"), filepath.Join(home, "renamed.json")); err != nil {
		t.Fatal(err)
	}
	for name, cmd := range map[string]string{
		"exec":           `find -L "$HOME" -name renamed.json -exec cat {} +`,
		"pipe":           `find -L "$HOME" -name renamed.json -print0 | xargs -0 cat`,
		"without_follow": `find "$HOME" -name renamed.json -exec cat {} +`,
		"explicit_alias": `find "$HOME/renamed.json" -name renamed.json -exec cat {} +`,
		"case_fold":      `find -L "$HOME" -iname RENAMED.JSON -exec cat {} +`,
	} {
		t.Run(name, func(t *testing.T) { secretGapDecisions(t, h, work, cmd, []string{bashCmd, secretLinkCodex}, true) })
	}
	for name, cmd := range map[string]string{
		"metadata":       `find -L "$HOME" -name renamed.json -print`,
		"count":          `find -L "$HOME" -name renamed.json -print0 | wc -c`,
		"unrelated_name": `find "$HOME" -name absent.json -exec cat {} +`,
		"depth_zero":     `find "$HOME" -maxdepth 0 -name renamed.json -exec cat {} +`,
	} {
		t.Run(name, func(t *testing.T) {
			secretGapDecisions(t, h, work, cmd, []string{bashCmd, secretLinkCodex}, false)
		})
	}
}
