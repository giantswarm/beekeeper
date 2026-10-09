// Package proof hosts a proof image in a public GitHub repository so a
// public issue or pull request thread can show it: one commit on the
// repository's images-only branch through the Git data API, made with the gh
// CLI's own login, and a Markdown image line pinned to that commit.
package proof

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	_ "image/gif"  // a GIF proof decodes
	_ "image/jpeg" // a Chrome screenshot is a JPEG
	"image/png"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// Branch holds the proof images of a repository: an orphan branch with
// nothing but images, so it runs no workflow and never meets the default
// branch.
const Branch = "beekeeper-proofs"

// MaxBytes bounds the image read: a screenshot is far smaller.
const MaxBytes = 10 << 20

// attempts bounds the retries of an upload whose branch moved under it.
const attempts = 3

// GH runs the gh CLI with input on its stdin and returns its stdout; err
// carries its stderr.
type GH func(ctx context.Context, input []byte, args ...string) ([]byte, error)

// ErrConflict is a ref update GitHub refused because the branch moved.
var ErrConflict = errors.New("the proofs branch moved")

// shaKey names an object's id in the Git data API.
const shaKey = "sha"

var repoName = regexp.MustCompile(`^[\w.-]+/[\w.-]+$`)

// Upload is one image hosted in Repo.
type Upload struct {
	GH   GH
	Repo string
	Alt  string
	Now  time.Time
}

// Result is a hosted image: where it lives and the Markdown that shows it.
type Result struct {
	Commit   string `json:"commit"`
	Path     string `json:"path"`
	URL      string `json:"url"`
	Markdown string `json:"markdown"`
}

// Normalize decodes a PNG, JPEG or GIF image and encodes it as PNG, which
// keeps the pixels and drops every metadata chunk (EXIF, text, comments).
func Normalize(raw []byte) ([]byte, error) {
	if len(raw) > MaxBytes {
		return nil, fmt.Errorf("the image is %d bytes, more than %d", len(raw), MaxBytes)
	}
	img, format, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("not a PNG, JPEG or GIF image: %w", err)
	}
	var out bytes.Buffer
	if err := png.Encode(&out, img); err != nil {
		return nil, fmt.Errorf("re-encode the %s image as PNG: %w", format, err)
	}
	return out.Bytes(), nil
}

// Path is where an image goes on the branch: the UTC date and time and the
// image's digest; it names no thread, repository or person.
func Path(img []byte, now time.Time) string {
	sum := sha256.Sum256(img)
	now = now.UTC()
	return fmt.Sprintf("%s/%s-%s.png", now.Format("2006-01-02"), now.Format("150405"), hex.EncodeToString(sum[:])[:12])
}

// Run hosts img (already normalized) on the branch and returns its link. A
// repository that is not public is refused: its image would not render for
// a public thread's readers.
func (u Upload) Run(ctx context.Context, img []byte) (Result, error) {
	if !repoName.MatchString(u.Repo) {
		return Result{}, fmt.Errorf("--repo %q is not owner/repo", u.Repo)
	}
	vis, err := u.api(ctx, nil, "repos/"+u.Repo, "--jq", ".visibility")
	if err != nil {
		return Result{}, err
	}
	if v := strings.TrimSpace(string(vis)); v != "public" {
		return Result{}, fmt.Errorf("%s is %s, not public: proof upload hosts images for public threads only", u.Repo, v)
	}
	path := Path(img, u.Now)
	blob, err := u.send(ctx, "POST", "git/blobs", map[string]any{"content": base64.StdEncoding.EncodeToString(img), "encoding": "base64"})
	if err != nil {
		return Result{}, err
	}
	for i := 0; ; i++ {
		commit, err := u.commit(ctx, path, blob)
		if errors.Is(err, ErrConflict) && i+1 < attempts {
			continue
		}
		if err != nil {
			return Result{}, err
		}
		url := fmt.Sprintf("https://raw.githubusercontent.com/%s/%s/%s", u.Repo, commit, path)
		alt := strings.NewReplacer("[", "", "]", "", "\n", " ").Replace(u.Alt)
		return Result{Commit: commit, Path: path, URL: url, Markdown: fmt.Sprintf("![%s](%s)", alt, url)}, nil
	}
}

// commit adds the blob at path on top of the branch's head (or as the first
// commit of a new branch) and moves the branch to it; ErrConflict when the
// branch moved meanwhile.
func (u Upload) commit(ctx context.Context, path, blob string) (string, error) {
	head, err := u.head(ctx)
	if err != nil {
		return "", err
	}
	tree := map[string]any{"tree": []map[string]string{{"path": path, "mode": "100644", "type": "blob", shaKey: blob}}}
	parents := []string{}
	if head != "" {
		tree["base_tree"] = head
		parents = append(parents, head)
	}
	t, err := u.send(ctx, "POST", "git/trees", tree)
	if err != nil {
		return "", err
	}
	c, err := u.send(ctx, "POST", "git/commits", map[string]any{"message": "proof: " + path, "tree": t, "parents": parents})
	if err != nil {
		return "", err
	}
	if head == "" {
		_, err = u.send(ctx, "POST", "git/refs", map[string]any{"ref": "refs/heads/" + Branch, shaKey: c})
	} else {
		_, err = u.send(ctx, "PATCH", "git/refs/heads/"+Branch, map[string]any{shaKey: c, "force": false})
	}
	if err != nil && (strings.Contains(err.Error(), "HTTP 422") || strings.Contains(err.Error(), "HTTP 409")) {
		return "", fmt.Errorf("%w: %v", ErrConflict, err)
	}
	return c, err
}

// head is the branch's head commit, "" while the branch does not exist.
func (u Upload) head(ctx context.Context) (string, error) {
	out, err := u.api(ctx, nil, "repos/"+u.Repo+"/git/ref/heads/"+Branch, "--jq", ".object.sha")
	if err != nil {
		if strings.Contains(err.Error(), "HTTP 404") {
			return "", nil
		}
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// send sends body to the repository's endpoint with method and returns the
// sha of the object it answers.
func (u Upload) send(ctx context.Context, method, endpoint string, body any) (string, error) {
	in, err := json.Marshal(body)
	if err != nil {
		return "", err
	}
	out, err := u.api(ctx, in, "-X", method, "repos/"+u.Repo+"/"+endpoint, "--input", "-")
	if err != nil {
		return "", err
	}
	var obj struct {
		SHA    string `json:"sha"`
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	if err := json.Unmarshal(out, &obj); err != nil {
		return "", fmt.Errorf("gh api %s: %w", endpoint, err)
	}
	if sha := cmp.Or(obj.SHA, obj.Object.SHA); sha != "" {
		return sha, nil
	}
	return "", fmt.Errorf("gh api %s: no sha in the answer", endpoint)
}

func (u Upload) api(ctx context.Context, input []byte, args ...string) ([]byte, error) {
	return u.GH(ctx, input, append([]string{"api"}, args...)...)
}

// RunGH is GH through the gh binary the caller's PATH resolves: an agent's
// gh carries the App's token, which beekeeper never reads.
func RunGH(ctx context.Context, input []byte, args ...string) ([]byte, error) {
	var stderr bytes.Buffer
	c := exec.CommandContext(ctx, "gh", args...) //nolint:gosec // gh api with a validated owner/repo
	c.Stdin = bytes.NewReader(input)
	c.Stderr = &stderr
	out, err := c.Output()
	if err != nil {
		return out, fmt.Errorf("gh %s: %v: %s", strings.Join(args[:min(len(args), 3)], " "), err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}
