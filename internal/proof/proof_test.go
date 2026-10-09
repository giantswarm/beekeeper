package proof

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"slices"
	"strings"
	"testing"
	"time"
)

// fakeGH answers the API calls of an upload from a branch head it keeps,
// recording each call and its body.
type fakeGH struct {
	visibility string
	head       string
	conflicts  int
	calls      []string
	bodies     map[string]map[string]any
	n          int
}

func (f *fakeGH) run(_ context.Context, input []byte, args ...string) ([]byte, error) {
	method, path := "GET", ""
	for i := 1; i < len(args); i++ {
		switch {
		case args[i] == "-X":
			method = args[i+1]
			i++
		case args[i] == "--jq" || args[i] == "--input":
			i++
		case path == "":
			path = args[i]
		}
	}
	call := method + " " + path
	f.calls = append(f.calls, call)
	if len(input) > 0 {
		var body map[string]any
		if err := json.Unmarshal(input, &body); err != nil {
			return nil, err
		}
		if f.bodies == nil {
			f.bodies = map[string]map[string]any{}
		}
		f.bodies[call] = body
	}
	f.n++
	sha := fmt.Sprintf(`{"sha":"sha%d"}`, f.n)
	switch {
	case strings.HasSuffix(path, "/o/r") || path == "repos/o/r":
		return []byte(f.visibility + "\n"), nil
	case method == "GET" && strings.Contains(path, "/git/ref/heads/"):
		if f.head == "" {
			return []byte(`{"message":"Not Found"}`), errors.New("gh api: exit status 1: gh: Not Found (HTTP 404)")
		}
		return []byte(f.head + "\n"), nil
	case method == "PATCH" && f.conflicts > 0:
		f.conflicts--
		f.head = "moved"
		return nil, errors.New("gh api: exit status 1: gh: Update is not a fast forward (HTTP 422)")
	case method == "POST" && strings.HasSuffix(path, "/git/refs"), method == "PATCH":
		f.head = f.bodies[call]["sha"].(string)
		return []byte(fmt.Sprintf(`{"object":{"sha":%q}}`, f.head)), nil
	}
	return []byte(sha), nil
}

// public and repo are a public repository's visibility and name.
const (
	public = "public"
	repo   = "o/r"
)

var at = time.Date(2026, 10, 9, 7, 30, 5, 0, time.UTC)

func testPNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 4, 3))
	img.Set(1, 1, color.RGBA{R: 200, A: 255})
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// withText inserts a tEXt chunk after a PNG's IHDR: metadata a re-encode
// drops.
func withText(p []byte, text string) []byte {
	body := []byte("tEXt" + text)
	chunk := binary.BigEndian.AppendUint32(nil, uint32(len(text))) //nolint:gosec // a short fixture
	chunk = binary.BigEndian.AppendUint32(append(chunk, body...), crc32.ChecksumIEEE(body))
	at := 8 + 25 // signature, IHDR
	return slices.Concat(p[:at], chunk, p[at:])
}

// Normalize keeps the pixels and drops the metadata; it refuses what is
// not an image and what is too large.
func TestNormalize(t *testing.T) {
	src := withText(testPNG(t), "Comment\x00secret-looking")
	if !bytes.Contains(src, []byte("secret-looking")) {
		t.Fatal("the fixture carries no text chunk")
	}
	out, err := Normalize(src)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(out, []byte("tEXt")) || bytes.Contains(out, []byte("secret-looking")) {
		t.Error("Normalize kept the text chunk")
	}
	img, err := png.Decode(bytes.NewReader(out))
	if err != nil {
		t.Fatal(err)
	}
	if r, _, _, _ := img.At(1, 1).RGBA(); r>>8 != 200 || img.Bounds().Dx() != 4 {
		t.Errorf("pixels changed: %v %v", img.At(1, 1), img.Bounds())
	}

	var j bytes.Buffer
	if err := jpeg.Encode(&j, image.NewGray(image.Rect(0, 0, 8, 8)), nil); err != nil {
		t.Fatal(err)
	}
	if out, err := Normalize(j.Bytes()); err != nil || !bytes.HasPrefix(out, []byte("\x89PNG")) {
		t.Errorf("Normalize(jpeg) = %v, PNG %v", err, bytes.HasPrefix(out, []byte("\x89PNG")))
	}
	if _, err := Normalize([]byte("not an image")); err == nil {
		t.Error("Normalize accepted text")
	}
	if _, err := Normalize(make([]byte, MaxBytes+1)); err == nil || !strings.Contains(err.Error(), "more than") {
		t.Errorf("Normalize(too large) = %v", err)
	}
}

// The path names the time and the digest, nothing else.
func TestPath(t *testing.T) {
	p := Path([]byte("x"), at.In(time.FixedZone("CEST", 2*3600)))
	if !strings.HasPrefix(p, "2026-10-09/073005-") || !strings.HasSuffix(p, ".png") || len(p) != len("2026-10-09/073005-")+12+4 {
		t.Errorf("Path = %q", p)
	}
}

// The first upload creates the branch with a parentless commit; the link is
// pinned to that commit.
func TestUploadNewBranch(t *testing.T) {
	f := &fakeGH{visibility: public}
	res, err := Upload{GH: f.run, Repo: repo, Alt: "shot [1]", Now: at}.Run(context.Background(), []byte("img"))
	if err != nil {
		t.Fatal(err)
	}
	commit := f.bodies["POST repos/o/r/git/refs"]
	if commit["ref"] != "refs/heads/"+Branch {
		t.Errorf("ref created = %v", commit)
	}
	if p := f.bodies["POST repos/o/r/git/commits"]["parents"]; len(p.([]any)) != 0 {
		t.Errorf("first commit parents = %v", p)
	}
	if _, ok := f.bodies["POST repos/o/r/git/trees"]["base_tree"]; ok {
		t.Error("first tree has a base_tree")
	}
	want := "https://raw.githubusercontent.com/o/r/" + res.Commit + "/" + res.Path
	if res.URL != want || res.Markdown != "![shot 1]("+want+")" || res.Commit != commit["sha"] {
		t.Errorf("result = %+v, want URL %s", res, want)
	}
}

// An upload onto the branch builds on its head and moves it without force;
// a branch that moved meanwhile is retried on its new head.
func TestUploadExistingBranchRetries(t *testing.T) {
	f := &fakeGH{visibility: public, head: "h0", conflicts: 1}
	res, err := Upload{GH: f.run, Repo: repo, Now: at}.Run(context.Background(), []byte("img"))
	if err != nil {
		t.Fatal(err)
	}
	patch := f.bodies["PATCH repos/o/r/git/refs/heads/"+Branch]
	if patch["force"] != false || patch["sha"] != res.Commit {
		t.Errorf("ref update = %v", patch)
	}
	if p := f.bodies["POST repos/o/r/git/commits"]["parents"].([]any); len(p) != 1 || p[0] != "moved" {
		t.Errorf("retried commit parents = %v, want the moved head", p)
	}
	if n := slices.Index(f.calls, "POST repos/o/r/git/blobs"); n < 0 || slices.Contains(f.calls[n+1:], "POST repos/o/r/git/blobs") {
		t.Errorf("blob uploaded other than once: %q", f.calls)
	}
}

// A repository that is not public, or a name that is not owner/repo, gets
// nothing written.
func TestUploadRefused(t *testing.T) {
	for _, c := range []struct{ repo, vis, want string }{
		{repo, "private", "not public"},
		{repo, "internal", "not public"},
		{"o/r/x", public, "not owner/repo"},
		{"o", public, "not owner/repo"},
	} {
		f := &fakeGH{visibility: c.vis}
		_, err := Upload{GH: f.run, Repo: c.repo, Now: at}.Run(context.Background(), []byte("img"))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s %s: err = %v, want %q", c.repo, c.vis, err, c.want)
		}
		for _, call := range f.calls {
			if !strings.HasPrefix(call, "GET") {
				t.Errorf("%s %s: wrote %s", c.repo, c.vis, call)
			}
		}
	}
}
