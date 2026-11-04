package github

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestRepositoriesPerOwner(t *testing.T) {
	var acting []string
	run := func(ctx context.Context, args ...string) ([]byte, error) {
		repo := Repo(ctx)
		acting = append(acting, repo)
		if len(args) != 4 || args[0] != "api" || args[1] != "graphql" || args[2] != "-f" || !strings.HasPrefix(args[3], "query=query{") || strings.Contains(args[3], "mutation") {
			t.Fatalf("unexpected read: %v", args)
		}
		if repo == "example/tools" {
			if strings.Contains(args[3], "giantswarm") || !strings.Contains(args[3], `r1:repository(owner:"example",name:"plans")`) {
				t.Fatalf("wrong owner grouping: %s", args[3])
			}
			return []byte(`{"data":{"r0":{"nameWithOwner":"example/tools","owner":{"__typename":"User"}},"r1":{"nameWithOwner":"example/plans","owner":{"__typename":"User"}}}}`), nil
		}
		if strings.Contains(args[3], `owner:"example"`) {
			t.Fatal("mixed owners")
		}
		return []byte(`{"data":{"r0":{"nameWithOwner":"giantswarm/beekeeper","owner":{"__typename":"Organization"}}}}`), nil
	}
	got, err := Repositories(t.Context(), run, []string{"example/tools", "giantswarm/beekeeper", "example/plans"})
	if err != nil || len(got) != 3 {
		t.Fatalf("Repositories = %v, %v", got, err)
	}
	if !reflect.DeepEqual(acting, []string{"example/tools", "giantswarm/beekeeper"}) || got[0].Owner.Type != "User" || got[2].Owner.Type != "Organization" {
		t.Fatalf("routing %v, results %v", acting, got)
	}
}

func TestRepositoriesFailures(t *testing.T) {
	for _, body := range []string{`{`, `{}`, `{"data":{"r0":null}}`, `{"data":{"r0":{"nameWithOwner":"example/tools"}}}`, `{"data":{"r0":{"nameWithOwner":"example/tools","owner":{"__typename":"User"}}},"errors":[{"message":"refused"}]}`} {
		t.Run(body, func(t *testing.T) {
			_, err := Repositories(t.Context(), func(context.Context, ...string) ([]byte, error) { return []byte(body), nil }, []string{"example/tools"})
			if err == nil {
				t.Fatal("unresolved probe passed")
			}
		})
	}
	want := errors.New("offline")
	if _, err := Repositories(t.Context(), func(context.Context, ...string) ([]byte, error) { return nil, want }, []string{"example/tools"}); !errors.Is(err, want) {
		t.Fatalf("error = %v", err)
	}
	for _, repo := range []string{"example", "/tools", "example/", "example/tools/extra", "example/tools#1", "example/a b"} {
		if _, err := Repositories(t.Context(), func(context.Context, ...string) ([]byte, error) {
			t.Fatal("invalid input reached GitHub")
			return nil, nil
		}, []string{"giantswarm/beekeeper", repo}); err == nil {
			t.Errorf("accepted %q", repo)
		}
	}
}
