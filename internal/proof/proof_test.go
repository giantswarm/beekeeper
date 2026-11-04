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
	"image/gif"
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

// The screenshot formats Normalize accepts, as image.DecodeConfig names them.
const (
	formatPNG  = "png"
	formatJPEG = "jpeg"
	formatGIF  = "gif"
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

// A highly compressed valid image must still obey the decoded pixel budget.
func TestNormalizeCompressedOversized(t *testing.T) {
	var b bytes.Buffer
	if err := png.Encode(&b, image.NewGray(image.Rect(0, 0, 8192, 4097))); err != nil {
		t.Fatal(err)
	}
	if b.Len() >= MaxBytes {
		t.Fatalf("fixture is not compressed below the byte limit: %d", b.Len())
	}
	out, err := Normalize(b.Bytes())
	if err == nil || !strings.Contains(err.Error(), "pixels") || out != nil {
		t.Fatalf("Normalize(compressed oversized PNG) = %d bytes, %v", len(out), err)
	}
}

// A PNG signature and IHDR suffice to inspect dimensions, without any pixel
// data. Rejection must happen before decoding even for enormous dimensions.
func TestNormalizePixelHeaders(t *testing.T) {
	for _, tc := range []struct {
		name          string
		width, height uint32
		oversized     bool
	}{
		{"at limit", MaxPixels, 1, false},
		{"over limit", MaxPixels + 1, 1, true},
		{"over limit tall", 1, MaxPixels + 1, true},
		{"over limit square", 8192, 4097, true},
		{"huge dimensions", 1 << 29, 1 << 29, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			header := bytes.Clone(testPNG(t)[:33])
			binary.BigEndian.PutUint32(header[16:20], tc.width)
			binary.BigEndian.PutUint32(header[20:24], tc.height)
			binary.BigEndian.PutUint32(header[29:33], crc32.ChecksumIEEE(header[12:29]))
			config, _, err := image.DecodeConfig(bytes.NewReader(header))
			if err != nil || config.Width != int(tc.width) || config.Height != int(tc.height) {
				t.Fatalf("invalid header fixture: %+v, %v", config, err)
			}
			out, err := Normalize(header)
			if err == nil || out != nil {
				t.Fatalf("Normalize(header) = %d bytes, %v", len(out), err)
			}
			if got := strings.Contains(err.Error(), "pixels"); got != tc.oversized {
				t.Errorf("pixel limit rejection = %v, want %v: %v", got, tc.oversized, err)
			}
		})
	}
}

// The same budget applies to JPEG and GIF headers before pixel data is read.
func TestNormalizeOversizedFormatHeaders(t *testing.T) {
	var j bytes.Buffer
	if err := jpeg.Encode(&j, image.NewGray(image.Rect(0, 0, 8, 6)), nil); err != nil {
		t.Fatal(err)
	}
	jpegHeader := j.Bytes()
	sof := bytes.Index(jpegHeader, []byte{0xff, 0xc0})
	if sof < 0 {
		t.Fatal("JPEG fixture has no baseline frame header")
	}
	binary.BigEndian.PutUint16(jpegHeader[sof+5:sof+7], 65535)
	binary.BigEndian.PutUint16(jpegHeader[sof+7:sof+9], 65535)
	sos := bytes.Index(jpegHeader, []byte{0xff, 0xda})
	if sos < 0 {
		t.Fatal("JPEG fixture has no scan header")
	}
	jpegHeader = jpegHeader[:sos+2+int(binary.BigEndian.Uint16(jpegHeader[sos+2:sos+4]))]

	gifHeader := append([]byte("GIF89a"), 0xff, 0xff, 0xff, 0xff, 0, 0, 0)
	for name, header := range map[string][]byte{formatJPEG: jpegHeader, formatGIF: gifHeader} {
		t.Run(name, func(t *testing.T) {
			config, _, err := image.DecodeConfig(bytes.NewReader(header))
			if err != nil || config.Width != 65535 || config.Height != 65535 {
				t.Fatalf("invalid header fixture: %+v, %v", config, err)
			}
			out, err := Normalize(header)
			if err == nil || !strings.Contains(err.Error(), "pixels") || out != nil {
				t.Fatalf("Normalize(oversized header) = %d bytes, %v", len(out), err)
			}
		})
	}
}

func TestNormalizeScreenshotFormats(t *testing.T) {
	for _, format := range []string{formatPNG, formatJPEG, formatGIF} {
		t.Run(format, func(t *testing.T) {
			img := image.NewGray(image.Rect(0, 0, 8, 6))
			var b bytes.Buffer
			var err error
			switch format {
			case formatPNG:
				err = png.Encode(&b, img)
			case formatJPEG:
				err = jpeg.Encode(&b, img, nil)
			case formatGIF:
				err = gif.Encode(&b, img, nil)
			}
			if err != nil {
				t.Fatal(err)
			}
			out, err := Normalize(b.Bytes())
			if err != nil {
				t.Fatal(err)
			}
			decoded, err := png.Decode(bytes.NewReader(out))
			if err != nil {
				t.Fatal(err)
			}
			if decoded.Bounds() != img.Bounds() {
				t.Errorf("bounds = %v, want %v", decoded.Bounds(), img.Bounds())
			}
		})
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

// Alt text cannot escape the image delimiter or introduce line endings.
func TestUploadAltText(t *testing.T) {
	const paragraphs = "shot  next"
	for _, tt := range []struct{ name, alt, want string }{
		{"empty", "", ""},
		{"odd trailing backslashes", `shot\`, `shot\\`},
		{"even trailing backslashes", `shot\\`, `shot\\\\`},
		{"three trailing backslashes", `shot\\\`, `shot\\\\\\`},
		{"backslashes beside brackets", `\[shot\]\`, `\\shot\\\\`},
		{"brackets between backslashes", `shot\[]\`, `shot\\\\`},
		{"CR paragraphs", "shot\r\rnext", paragraphs},
		{"LF paragraphs", "shot\n\nnext", paragraphs},
		{"CRLF paragraphs", "shot\r\n\r\nnext", paragraphs},
		{"mixed line endings", "a\rb\nc\r\nd", "a b c d"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := &fakeGH{visibility: public}
			res, err := (Upload{GH: f.run, Repo: repo, Alt: tt.alt, Now: at}).Run(context.Background(), []byte("img"))
			if err != nil {
				t.Fatal(err)
			}
			url := "https://raw.githubusercontent.com/o/r/" + res.Commit + "/" + res.Path
			if want := "![" + tt.want + "](" + url + ")"; res.Markdown != want {
				t.Errorf("Markdown = %q, want %q", res.Markdown, want)
			}
		})
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

// An unknown visibility, or a name that is not owner/repo, gets nothing written.
func TestUploadRefused(t *testing.T) {
	for _, c := range []struct{ repo, vis, want string }{
		{repo, "", "unknown visibility"},
		{repo, "unexpected", "unknown visibility"},
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

// Private and internal images stay in the target repository. The returned
// link uses the authenticated repository route, never a raw access token.
func TestUploadRestrictedRepository(t *testing.T) {
	for _, visibility := range []string{"private", "internal"} {
		for _, head := range []string{"", "existing"} {
			t.Run(visibility+"/"+head, func(t *testing.T) {
				f := &fakeGH{visibility: visibility, head: head}
				res, err := (Upload{GH: f.run, Repo: repo, Alt: "proof", Now: at}).Run(context.Background(), testPNG(t))
				if err != nil {
					t.Fatal(err)
				}
				want := "https://github.com/o/r/blob/" + res.Commit + "/" + res.Path + "?raw=true"
				if res.URL != want || res.Markdown != "![proof](../../../o/r/blob/"+res.Commit+"/"+res.Path+"?raw=true)" {
					t.Fatalf("result = %+v, want %s", res, want)
				}
				for _, call := range f.calls {
					if !strings.Contains(call, "repos/o/r") {
						t.Errorf("call escaped repository: %s", call)
					}
				}
			})
		}
	}
}
