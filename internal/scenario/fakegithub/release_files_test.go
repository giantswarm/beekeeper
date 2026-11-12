//go:build scenario

package fakegithub

import (
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// The updater finds releases by their tags (git/matching-refs) and
// downloads a binary no scenario could hold in its YAML: an asset's
// content is then a file, read at each download.
func TestMatchingRefsAndFileAssets(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "tool-linux-amd64")
	if err := os.WriteFile(binary, []byte("\x7fELF the binary"), 0o600); err != nil {
		t.Fatal(err)
	}
	s, _ := build(t, `
repositories:
  - name: example/tool
    releases:
      - {tag: v1.0.0, assets: [{name: notes.txt, content: "text\n"}]}
      - tag: v1.1.0-rc.1
        prerelease: true
        assets:
          - {name: tool-linux-amd64, file: `+binary+`}
`)
	resp := request(s, "test", http.MethodGet, "/repos/example/tool/git/matching-refs/tags/v", nil, "")
	if resp.Status != http.StatusOK {
		t.Fatalf("matching-refs: status %d: %s", resp.Status, resp.Body)
	}
	refs := decodeList(t, resp.Body)
	if len(refs) != 2 || at(t, refs, 0, "ref") != "refs/tags/v1.0.0" || at(t, refs, 1, "ref") != "refs/tags/v1.1.0-rc.1" {
		t.Fatalf("matching-refs tags/v: %s", resp.Body)
	}
	if sha, _ := at(t, refs, 0, "object", "sha").(string); len(sha) != 40 {
		t.Errorf("a ref's object.sha = %q, want a commit id", sha)
	}
	// A prefix nothing starts with is an empty list, not a 404; an unknown
	// repository is one.
	if resp := request(s, "test", http.MethodGet, "/repos/example/tool/git/matching-refs/tags/w", nil, ""); resp.Status != http.StatusOK || len(decodeList(t, resp.Body)) != 0 {
		t.Errorf("matching-refs tags/w: status %d: %s", resp.Status, resp.Body)
	}
	if resp := request(s, "test", http.MethodGet, "/repos/example/none/git/matching-refs/tags/v", nil, ""); resp.Status != http.StatusNotFound {
		t.Errorf("matching-refs of an unknown repository: status %d", resp.Status)
	}

	resp = request(s, "test", http.MethodGet, "/repos/example/tool/releases/tags/v1.1.0-rc.1", nil, "")
	if resp.Status != http.StatusOK {
		t.Fatalf("release by tag: status %d: %s", resp.Status, resp.Body)
	}
	rel := decode(t, resp.Body)
	if got := at(t, rel, "assets", 0, "size"); got != float64(len("\x7fELF the binary")) {
		t.Errorf("a file asset's size = %v", got)
	}
	id, _ := at(t, rel, "assets", 0, "id").(float64)
	download := "/repos/example/tool/releases/assets/" + strconv.Itoa(int(id))
	resp = request(s, "test", http.MethodGet, download, map[string]string{"Accept": "application/octet-stream"}, "")
	if resp.Status != http.StatusOK || string(resp.Body) != "\x7fELF the binary" {
		t.Fatalf("download: status %d, body %q", resp.Status, resp.Body)
	}
	// Read at each download: the file is the asset.
	if err := os.WriteFile(binary, []byte("changed"), 0o600); err != nil {
		t.Fatal(err)
	}
	if resp := request(s, "test", http.MethodGet, download, map[string]string{"Accept": "application/octet-stream"}, ""); string(resp.Body) != "changed" {
		t.Errorf("a second download = %q, want the file's content now", resp.Body)
	}
}

func TestAssetWithContentAndFileIsRefused(t *testing.T) {
	_, err := New(parseGiven(t, `
repositories:
  - name: example/tool
    releases:
      - {tag: v1.0.0, assets: [{name: a, content: x, file: /nowhere}]}
`), Options{})
	if err == nil {
		t.Fatal("an asset with content and file was accepted")
	}
}
