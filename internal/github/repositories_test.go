package github

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

const (
	probeToolsRepo = "example/tools"
	probeUserType  = "User"
)

func TestRepositoriesPerOwner(t *testing.T) {
	var acting []string
	run := func(ctx context.Context, args ...string) ([]byte, error) {
		repo := Repo(ctx)
		acting = append(acting, repo)
		if len(args) != 4 || args[0] != "api" || args[1] != "graphql" || args[2] != "-f" || !strings.HasPrefix(args[3], "query=query{") || strings.Contains(args[3], "mutation") {
			t.Fatalf("unexpected read: %v", args)
		}
		if repo == probeToolsRepo {
			if strings.Contains(args[3], "giantswarm") || !strings.Contains(args[3], `r1:repository(owner:"example",name:"plans")`) {
				t.Fatalf("wrong owner grouping: %s", args[3])
			}
			return []byte(`{"data":{"r0":{"nameWithOwner":probeToolsRepo,"owner":{"__typename":probeUserType}},"r1":{"nameWithOwner":examplePlansRepo,"owner":{"__typename":probeUserType}}}}`), nil
		}
		if strings.Contains(args[3], `owner:"example"`) {
			t.Fatal("mixed owners")
		}
		return []byte(`{"data":{"r0":{"nameWithOwner":"giantswarm/beekeeper","owner":{"__typename":"Organization"}}}}`), nil
	}
	got, err := Repositories(t.Context(), run, []string{probeToolsRepo, "giantswarm/beekeeper", examplePlansRepo})
	if err != nil || len(got) != 3 {
		t.Fatalf("Repositories = %v, %v", got, err)
	}
	if !reflect.DeepEqual(acting, []string{probeToolsRepo, "giantswarm/beekeeper"}) || got[0].Owner.Type != probeUserType || got[2].Owner.Type != "Organization" {
		t.Fatalf("routing %v, results %v", acting, got)
	}
}

func TestRepositoriesFailures(t *testing.T) {
	for _, body := range []string{`{`, `{}`, `{"data":{"r0":null}}`, `{"data":{"r0":{"nameWithOwner":probeToolsRepo}}}`, `{"data":{"r0":{"nameWithOwner":probeToolsRepo,"owner":{"__typename":probeUserType}}},"errors":[{"message":"refused"}]}`} {
		t.Run(body, func(t *testing.T) {
			_, err := Repositories(t.Context(), func(context.Context, ...string) ([]byte, error) { return []byte(body), nil }, []string{probeToolsRepo})
			if err == nil {
				t.Fatal("unresolved probe passed")
			}
		})
	}
	want := errors.New("offline")
	if _, err := Repositories(t.Context(), func(context.Context, ...string) ([]byte, error) { return nil, want }, []string{probeToolsRepo}); !errors.Is(err, want) {
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
