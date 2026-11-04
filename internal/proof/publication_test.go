package proof

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"testing"
)

const (
	proofRepoAPI = "repos/" + repo
	proofGitAPI  = proofRepoAPI + "/git/"
	otherPath    = "earlier.png"
	otherBlob    = "other-blob"
	otherTree    = "other-tree"
	otherHead    = "other-head"
	getPrefix    = http.MethodGet + " "
	postPrefix   = http.MethodPost + " "
	patchPrefix  = http.MethodPatch + " "
	proofCommits = proofGitAPI + "commits"
	proofRefs    = proofGitAPI + "refs"
	proofRef     = proofRefs + "/heads/" + Branch
)

type proofCommit struct {
	Tree    string
	Parents []string
}

// publicationGH keeps immutable Git objects separately from the mutable
// branch ref. Tests can interleave a competing writer at that boundary.
type publicationGH struct {
	head    string
	blobs   map[string][]byte
	trees   map[string]map[string]string
	commits map[string]proofCommit
	calls   []string
	next    int
}

func newPublicationGH() *publicationGH {
	return &publicationGH{
		blobs:   map[string][]byte{},
		trees:   map[string]map[string]string{},
		commits: map[string]proofCommit{},
	}
}

func (g *publicationGH) seed() {
	g.blobs[otherBlob] = []byte("earlier proof")
	g.trees[otherTree] = map[string]string{otherPath: otherBlob}
	g.commits[otherHead] = proofCommit{Tree: otherTree}
	g.head = otherHead
}

func (g *publicationGH) files() map[string]string {
	return g.trees[g.commits[g.head].Tree]
}

func publicationRequest(args []string) (method, path string) {
	method = http.MethodGet
	for i := 1; i < len(args); i++ {
		switch args[i] {
		case "-X":
			i++
			method = args[i]
		case "--jq", "--input":
			i++
		default:
			path = args[i]
		}
	}
	return method, path
}

func (g *publicationGH) run(ctx context.Context, input []byte, args ...string) ([]byte, error) {
	method, path := publicationRequest(args)
	call := method + " " + path
	g.calls = append(g.calls, call)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if call == getPrefix+proofRepoAPI {
		return []byte(public), nil
	}
	if call == getPrefix+proofGitAPI+"ref/heads/"+Branch {
		if g.head == "" {
			return nil, errors.New("Not Found (HTTP 404)")
		}
		return []byte(g.head), nil
	}
	g.next++
	id := fmt.Sprintf("object-%d", g.next)
	switch call {
	case postPrefix + proofGitAPI + "blobs":
		var body struct{ Content, Encoding string }
		if err := json.Unmarshal(input, &body); err != nil {
			return nil, err
		}
		if body.Encoding != "base64" {
			return nil, errors.New("blob encoding must be base64")
		}
		data, err := base64.StdEncoding.DecodeString(body.Content)
		if err != nil {
			return nil, err
		}
		g.blobs[id] = data
	case postPrefix + proofGitAPI + "trees":
		var body struct {
			Base string `json:"base_tree"`
			Tree []struct{ Path, Mode, Type, SHA string }
		}
		if err := json.Unmarshal(input, &body); err != nil {
			return nil, err
		}
		files := map[string]string{}
		if body.Base != "" {
			base := body.Base
			if c, ok := g.commits[base]; ok {
				base = c.Tree
			}
			if g.trees[base] == nil {
				return nil, errors.New("unknown base tree")
			}
			files = maps.Clone(g.trees[base])
		}
		for _, entry := range body.Tree {
			if entry.Mode != "100644" || entry.Type != "blob" || g.blobs[entry.SHA] == nil {
				return nil, errors.New("invalid image entry")
			}
			files[entry.Path] = entry.SHA
		}
		g.trees[id] = files
	case postPrefix + proofCommits:
		var body proofCommit
		if err := json.Unmarshal(input, &body); err != nil {
			return nil, err
		}
		if g.trees[body.Tree] == nil {
			return nil, errors.New("unknown commit tree")
		}
		for _, parent := range body.Parents {
			if _, ok := g.commits[parent]; !ok {
				return nil, errors.New("unknown commit parent")
			}
		}
		g.commits[id] = body
	case postPrefix + proofRefs, patchPrefix + proofRef:
		var body struct {
			Ref, SHA string
			Force    bool
		}
		if err := json.Unmarshal(input, &body); err != nil {
			return nil, err
		}
		commit, ok := g.commits[body.SHA]
		if !ok {
			return nil, errors.New("unknown ref commit")
		}
		if method == http.MethodPost {
			if body.Ref != "refs/heads/"+Branch {
				return nil, errors.New("wrong branch")
			}
			if g.head != "" {
				return nil, errors.New("Reference already exists (HTTP 422)")
			}
		} else if !body.Force && !slices.Contains(commit.Parents, g.head) {
			return nil, errors.New("Update is not a fast forward (HTTP 422)")
		}
		g.head = body.SHA
		return json.Marshal(map[string]any{"object": map[string]string{shaKey: g.head}})
	default:
		return nil, fmt.Errorf("unexpected API call: %s", call)
	}
	return json.Marshal(map[string]string{shaKey: id})
}

func TestUploadCreationRace(t *testing.T) {
	g := newPublicationGH()
	img := testPNG(t)
	raced := false
	gh := func(ctx context.Context, input []byte, args ...string) ([]byte, error) {
		method, path := publicationRequest(args)
		if method == http.MethodPost && path == proofRefs && !raced {
			// Another upload publishes after our missing-head read.
			g.seed()
			raced = true
		}
		return g.run(ctx, input, args...)
	}
	res, err := (Upload{GH: gh, Repo: repo, Now: at}).Run(context.Background(), img)
	if err != nil {
		t.Fatal(err)
	}
	if !raced || res.Commit != g.head {
		t.Fatalf("creation race was not recovered: raced=%v, result=%+v, head=%s", raced, res, g.head)
	}
	files := g.files()
	if len(files) != 2 || files[otherPath] != otherBlob || !bytes.Equal(g.blobs[files[res.Path]], img) {
		t.Fatalf("published tree lost an image: %v", files)
	}
	if parents := g.commits[res.Commit].Parents; len(parents) != 1 || parents[0] != otherHead {
		t.Fatalf("published parents = %v, want the competing upload's head", parents)
	}
	if len(g.blobs) != 2 { // the seeded image and exactly one uploaded blob
		t.Fatalf("creation retry uploaded extra blobs: %d", len(g.blobs))
	}
}

func TestUploadConflictBudget(t *testing.T) {
	g := newPublicationGH()
	g.seed()
	moves := 0
	gh := func(ctx context.Context, input []byte, args ...string) ([]byte, error) {
		method, path := publicationRequest(args)
		if method == http.MethodPatch && path == proofRef {
			// A competing writer wins every attempt; keep its tree and
			// advance its ancestry so our candidate is never a fast-forward.
			moves++
			if moves > 3 {
				return nil, errors.New("retry budget exceeded")
			}
			old := g.head
			g.head = fmt.Sprintf("moving-head-%d", moves)
			g.commits[g.head] = proofCommit{Tree: g.commits[old].Tree, Parents: []string{old}}
			out, err := g.run(ctx, input, args...)
			if err != nil {
				return nil, errors.New("Conflict (HTTP 409)")
			}
			return out, nil // a forced update would incorrectly report success
		}
		return g.run(ctx, input, args...)
	}
	res, err := (Upload{GH: gh, Repo: repo, Now: at}).Run(context.Background(), testPNG(t))
	if !errors.Is(err, ErrConflict) || res != (Result{}) {
		t.Fatalf("exhausted upload = %+v, %v; want empty result and ErrConflict", res, err)
	}
	if moves != 3 {
		t.Fatalf("conflicting publication attempts = %d, want 3", moves)
	}
	if files := g.files(); len(files) != 1 || files[otherPath] != otherBlob {
		t.Fatalf("failed upload changed published images: %v", files)
	}
	if len(g.blobs) != 2 {
		t.Fatalf("conflict retries uploaded extra blobs: %d", len(g.blobs))
	}
}

func TestUploadPublicationFailures(t *testing.T) {
	for _, tc := range []struct {
		name, call string
		newBranch  bool
	}{
		{"visibility", getPrefix + proofRepoAPI, false},
		{"blob", postPrefix + proofGitAPI + "blobs", false},
		{"head", getPrefix + proofGitAPI + "ref/heads/" + Branch, false},
		{"tree", postPrefix + proofGitAPI + "trees", false},
		{"commit", postPrefix + proofCommits, false},
		{"update ref", patchPrefix + proofRef, false},
		{"create ref", postPrefix + proofRefs, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := newPublicationGH()
			if !tc.newBranch {
				g.seed()
			}
			head := g.head
			failure := errors.New("offline API failure")
			failed, continued := false, false
			gh := func(ctx context.Context, input []byte, args ...string) ([]byte, error) {
				if failed {
					continued = true
				}
				method, path := publicationRequest(args)
				if method+" "+path == tc.call {
					failed = true
					return nil, failure
				}
				return g.run(ctx, input, args...)
			}
			res, err := (Upload{GH: gh, Repo: repo, Now: at}).Run(context.Background(), testPNG(t))
			if !failed || continued || !errors.Is(err, failure) || res != (Result{}) {
				t.Fatalf("upload after %s = %+v, %v; failed=%v, continued=%v", tc.call, res, err, failed, continued)
			}
			if g.head != head {
				t.Fatalf("failed upload moved branch from %s to %s", head, g.head)
			}
		})
	}
}

func TestUploadInvalidObjectResponses(t *testing.T) {
	for _, response := range []string{`{}`, `{"sha":""}`, `not JSON`} {
		t.Run(response, func(t *testing.T) {
			g := newPublicationGH()
			g.seed()
			gh := func(ctx context.Context, input []byte, args ...string) ([]byte, error) {
				out, err := g.run(ctx, input, args...)
				method, path := publicationRequest(args)
				if err == nil && method == http.MethodPost && path == proofCommits {
					return []byte(response), nil // the commit exists, but its ID is unavailable
				}
				return out, err
			}
			res, err := (Upload{GH: gh, Repo: repo, Now: at}).Run(context.Background(), testPNG(t))
			if err == nil || res != (Result{}) || g.head != otherHead {
				t.Fatalf("invalid commit response published an image: %+v, %v, head=%s", res, err, g.head)
			}
			if len(g.commits) != 2 {
				t.Fatalf("fixture never reached the partial-write boundary: %v", g.commits)
			}
			for _, call := range g.calls {
				if call == patchPrefix+proofRef {
					t.Fatalf("publication attempted after invalid response: %v", g.calls)
				}
			}
		})
	}
}

func TestUploadCancellationBeforePublication(t *testing.T) {
	g := newPublicationGH()
	g.seed()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	gh := func(ctx context.Context, input []byte, args ...string) ([]byte, error) {
		out, err := g.run(ctx, input, args...)
		method, path := publicationRequest(args)
		if method == http.MethodPost && path == proofCommits {
			cancel() // objects are written; the ref update must honor cancellation
		}
		return out, err
	}
	res, err := (Upload{GH: gh, Repo: repo, Now: at}).Run(ctx, testPNG(t))
	if !errors.Is(err, context.Canceled) || res != (Result{}) || g.head != otherHead {
		t.Fatalf("canceled upload = %+v, %v, head=%s", res, err, g.head)
	}
	if len(g.commits) != 2 {
		t.Fatalf("fixture never reached cancellation after object creation: %v", g.commits)
	}
}
