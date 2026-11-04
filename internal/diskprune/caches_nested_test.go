package diskprune

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCacheFilesNestedPolicyDoesNotBlockOuterCleanup(t *testing.T) {
	for _, scenario := range []string{"nested checkout", "initial reference", "refreshed reference", whyUnreadable} {
		t.Run(scenario, func(t *testing.T) {
			root := t.TempDir()
			outer := cacheFile(t, root, "outer", 1000, 10*day)
			inner := filepath.Join(root, "nested")
			data := cacheFile(t, inner, "data", 1000, 10*day)
			p := Policy{Root: root, Files: true, OlderThan: 7 * day, Nested: []string{inner}}
			var refs []string
			switch scenario {
			case "nested checkout":
				cacheFile(t, inner, ".git/HEAD", 10, 10*day)
			case "initial reference":
				refs = []string{data}
			case "refreshed reference":
				p.Recheck = func() ([]string, error) { return []string{data}, nil }
			case whyUnreadable:
				if err := os.Chmod(inner, 0); err != nil {
					t.Fatal(err)
				}
				defer func() { _ = os.Chmod(inner, 0700) }() //nolint:gosec // test-owned directory
				if _, err := os.ReadDir(inner); !os.IsPermission(err) {
					t.Skip("filesystem does not enforce directory permissions")
				}
			}
			r := Cache(p, time.Now(), false, refs, nil, false)
			if scenario == whyUnreadable {
				if err := os.Chmod(inner, 0700); err != nil { //nolint:gosec // test-owned directory
					t.Fatal(err)
				}
			}
			if r.Held != 1000 || r.Freed != 1000 || r.Removed != 1 || len(r.Errors) != 0 || exists(outer) || !exists(data) {
				t.Fatalf("nested policy blocked outer cleanup: %+v, outer=%v inner=%v", r, exists(outer), exists(data))
			}
		})
	}
}

func TestCacheFilesNestedPolicyStillProtectsOuterReferences(t *testing.T) {
	for _, refreshed := range []bool{false, true} {
		name := "initial"
		if refreshed {
			name = "refreshed"
		}
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			outer := cacheFile(t, root, "outer", 1000, 10*day)
			inner := filepath.Join(root, "nested")
			data := cacheFile(t, inner, "data", 1000, 10*day)
			refs := []string{outer, data}
			p := Policy{Root: root, Files: true, OlderThan: 7 * day, Nested: []string{inner}}
			if refreshed {
				p.Recheck = func() ([]string, error) { return []string{outer, data}, nil }
				refs = nil
			}
			r := Cache(p, time.Now(), false, refs, nil, false)
			if r.Removed != 0 || r.Kept["in use"] != 1 || !exists(outer) || !exists(data) {
				t.Fatalf("outer reference lost protection: %+v", r)
			}
		})
	}
}

// Excluding a checkout's metadata must not hide the checkout that contains
// it: source files outside the excluded metadata still belong to that checkout.
func TestCacheFilesNestedMetadataDoesNotHideOuterCheckout(t *testing.T) {
	root := t.TempDir()
	source := cacheFile(t, root, "repo/source", 1000, 10*day)
	cacheFile(t, root, "repo/.git/HEAD", 10, 10*day)
	p := Policy{Root: root, Files: true, OlderThan: 7 * day, Nested: []string{filepath.Join(root, "repo", checkoutMetadata)}}
	r := Cache(p, time.Now(), false, nil, nil, false)
	if r.Removed != 0 || r.Kept[whyCheckout] != 1 || !exists(source) {
		t.Fatalf("excluding metadata hid the outer checkout: %+v", r)
	}
}
