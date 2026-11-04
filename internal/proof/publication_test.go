package proof

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"strings"
	"testing"
)

// publicationGH retains immutable objects separately from the published ref.
// Hooks run before an operation, allowing deterministic interleavings without
// a repository, credentials, goroutines, or timing assumptions.
type publicationGH struct {
	head    string
	trees   map[string]map[string]string
	commits map[string]string
	blobs   map[string]bool
	calls   []string
	bodies  []map[string]any
	hook    func(string) ([]byte, error, bool)
	next    int
}

func newPublicationGH() *publicationGH {
	return &publicationGH{trees: map[string]map[string]string{}, commits: map[string]string{}, blobs: map[string]bool{}}
}
func (f *publicationGH) id() string                  { f.next++; return fmt.Sprintf("%040x", f.next) }
func (f *publicationGH) contents() map[string]string { return f.trees[f.commits[f.head]] }
func (f *publicationGH) run(ctx context.Context, input []byte, args ...string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	method, endpoint := "GET", args[1]
	if endpoint == "-X" {
		method, endpoint = args[2], args[3]
	}
	endpoint = strings.TrimPrefix(endpoint, "repos/o/r")
	call := method + " " + endpoint
	body := map[string]any{}
	if len(input) > 0 {
		if err := json.Unmarshal(input, &body); err != nil {
			return nil, err
		}
	}
	f.calls = append(f.calls, call)
	f.bodies = append(f.bodies, body)
	if f.hook != nil {
		if out, err, handled := f.hook(call); handled {
			return out, err
		}
	}
	answer := func(id string) ([]byte, error) { return json.Marshal(map[string]string{"sha": id}) }
	switch call {
	case "GET ":
		return []byte("public"), nil
	case "GET /git/ref/heads/" + Branch:
		if f.head == "" {
			return nil, errors.New("HTTP 404")
		}
		return []byte(f.head), nil
	case "POST /git/blobs":
		id := f.id()
		f.blobs[id] = true
		return answer(id)
	case "POST /git/trees":
		entries := map[string]string{}
		if base, ok := body["base_tree"].(string); ok {
			tree, ok := f.commits[base]
			if !ok {
				return nil, errors.New("unknown base commit")
			}
			entries = maps.Clone(f.trees[tree])
		}
		for _, raw := range body["tree"].([]any) {
			entry := raw.(map[string]any)
			sha := entry["sha"].(string)
			if !f.blobs[sha] {
				return nil, errors.New("unknown blob")
			}
			entries[entry["path"].(string)] = sha
		}
		id := f.id()
		f.trees[id] = entries
		return answer(id)
	case "POST /git/commits":
		tree := body["tree"].(string)
		if _, ok := f.trees[tree]; !ok {
			return nil, errors.New("unknown tree")
		}
		for _, parent := range body["parents"].([]any) {
			if _, ok := f.commits[parent.(string)]; !ok {
				return nil, errors.New("unknown parent")
			}
		}
		id := f.id()
		f.commits[id] = tree
		return answer(id)
	case "POST /git/refs", "PATCH /git/refs/heads/" + Branch:
		id := body["sha"].(string)
		if _, ok := f.commits[id]; !ok {
			return nil, errors.New("unknown commit")
		}
		f.head = id
		return json.Marshal(map[string]any{"object": map[string]string{"sha": id}})
	}
	return nil, fmt.Errorf("unexpected call %s", call)
}
func (f *publicationGH) count(call string) int {
	n := 0
	for _, c := range f.calls {
		if c == call {
			n++
		}
	}
	return n
}
func (f *publicationGH) upload(ctx context.Context, image string) (Result, error) {
	return Upload{GH: f.run, Repo: repo, Now: at}.Run(ctx, []byte(image))
}
func assertPublished(t *testing.T, f *publicationGH, r Result, image string) {
	t.Helper()
	if r.Commit != f.head || r.Path != Path([]byte(image), at) || r.Markdown == "" || f.contents()[r.Path] == "" {
		t.Fatalf("unpublished result: %+v, contents %v", r, f.contents())
	}
}
func TestPublicationCreateRace(t *testing.T) {
	f := newPublicationGH()
	var winner Result
	f.hook = func(call string) ([]byte, error, bool) {
		if call != "POST /git/refs" {
			return nil, nil, false
		}
		f.hook = nil
		var err error
		winner, err = f.upload(context.Background(), "winner")
		if err != nil {
			t.Fatal(err)
		}
		return nil, errors.New("HTTP 422: reference already exists"), true
	}
	result, err := f.upload(context.Background(), "loser")
	if err != nil {
		t.Fatal(err)
	}
	assertPublished(t, f, result, "loser")
	if len(f.contents()) != 2 || f.contents()[winner.Path] == "" {
		t.Fatalf("lost winner: %v", f.contents())
	}
	if f.count("POST /git/blobs") != 2 || f.count("POST /git/refs") != 2 || f.count("PATCH /git/refs/heads/"+Branch) != 1 {
		t.Fatalf("unexpected retries: %v", f.calls)
	}
}
func TestPublicationConflictBudget(t *testing.T) {
	f := newPublicationGH()
	original, err := f.upload(context.Background(), "original")
	if err != nil {
		t.Fatal(err)
	}
	before := maps.Clone(f.contents())
	f.calls = nil
	f.hook = func(call string) ([]byte, error, bool) {
		if strings.HasPrefix(call, "PATCH ") {
			return nil, errors.New("HTTP 409"), true
		}
		return nil, nil, false
	}
	result, err := f.upload(context.Background(), "new")
	if !errors.Is(err, ErrConflict) || result != (Result{}) || f.head != original.Commit || !reflect.DeepEqual(before, f.contents()) {
		t.Fatalf("result %+v, error %v, contents %v", result, err, f.contents())
	}
	if f.count("PATCH /git/refs/heads/"+Branch) != attempts || f.count("POST /git/commits") != attempts || f.count("POST /git/blobs") != 1 {
		t.Fatalf("unbounded retries: %v", f.calls)
	}
}

func TestPublicationFailures(t *testing.T) {
	stages := []string{"GET ", "POST /git/blobs", "GET /git/ref/heads/" + Branch, "POST /git/trees", "POST /git/commits", "POST /git/refs", "PATCH /git/refs/heads/" + Branch}
	for _, existing := range []bool{false, true} {
		for _, stage := range stages {
			if existing && stage == "POST /git/refs" || !existing && strings.HasPrefix(stage, "PATCH") {
				continue
			}
			for _, response := range []string{"error", "{", "{}", `{"sha":"invalid"}`} {
				if response != "error" && stage == "GET " {
					continue
				}
				t.Run(fmt.Sprintf("existing=%v/%s/%s", existing, stage, response), func(t *testing.T) {
					f := newPublicationGH()
					if existing {
						if _, err := f.upload(context.Background(), "original"); err != nil {
							t.Fatal(err)
						}
					}
					head := f.head
					before := maps.Clone(f.contents())
					f.calls = nil
					failure := errors.New("injected failure")
					f.hook = func(call string) ([]byte, error, bool) {
						if call != stage {
							return nil, nil, false
						}
						if response == "error" {
							return nil, failure, true
						}
						return []byte(response), nil, true
					}
					result, err := f.upload(context.Background(), "new")
					if err == nil || result != (Result{}) || head != f.head || !reflect.DeepEqual(before, f.contents()) {
						t.Fatalf("result %+v, error %v, head %s", result, err, f.head)
					}
					if response == "error" && !errors.Is(err, failure) {
						t.Fatalf("lost failure: %v", err)
					}
					if f.calls[len(f.calls)-1] != stage {
						t.Fatalf("continued after failure: %v", f.calls)
					}
				})
			}
		}
	}
}
func TestPublicationCancellationAfterObjects(t *testing.T) {
	for _, stage := range []string{"POST /git/blobs", "POST /git/trees", "POST /git/commits"} {
		t.Run(stage, func(t *testing.T) {
			for _, existing := range []bool{false, true} {
				f := newPublicationGH()
				if existing {
					if _, err := f.upload(context.Background(), "original"); err != nil {
						t.Fatal(err)
					}
				}
				head := f.head
				before := maps.Clone(f.contents())
				f.calls = nil
				ctx, cancel := context.WithCancel(context.Background())
				gh := func(ctx context.Context, input []byte, args ...string) ([]byte, error) {
					out, err := f.run(ctx, input, args...)
					if f.calls[len(f.calls)-1] == stage {
						cancel()
					}
					return out, err
				}
				result, err := (Upload{GH: gh, Repo: repo, Now: at}).Run(ctx, []byte("new"))
				cancel()
				if !errors.Is(err, context.Canceled) || result != (Result{}) || f.head != head || !reflect.DeepEqual(before, f.contents()) {
					t.Fatalf("result %+v, error %v, head %s", result, err, f.head)
				}
				if f.calls[len(f.calls)-1] != stage {
					t.Fatalf("continued after cancellation: %v", f.calls)
				}
			}
		})
	}
}
