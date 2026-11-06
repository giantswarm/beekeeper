package cmd

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/internal/github"
)

const (
	probeRepositoryCommand = "repository"
	probeToolsRepo         = "example/tools"
	probeUserType          = "User"
	probeOrgType           = "Organization"
)

func TestDoctorRepository(t *testing.T) {
	old := github.RunGH
	t.Cleanup(func() { github.RunGH = old })
	var owners []string
	github.RunGH = func(ctx context.Context, _ ...string) ([]byte, error) {
		repo := github.Repo(ctx)
		owners = append(owners, repo)
		kind := probeUserType
		if strings.HasPrefix(repo, "giantswarm/") {
			kind = probeOrgType
		}
		return []byte(`{"data":{"r0":{"nameWithOwner":"` + repo + `","owner":{"__typename":"` + kind + `"}}}}`), nil
	}
	for _, jsonOutput := range []bool{false, true} {
		var out bytes.Buffer
		a := &app{out: &out, json: jsonOutput}
		c := a.doctorCmd()
		c.SetArgs([]string{probeRepositoryCommand, probeToolsRepo, sshTestRepository})
		if err := c.Execute(); err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{probeToolsRepo, sshTestRepository, probeUserType, probeOrgType} {
			if !strings.Contains(out.String(), want) {
				t.Fatalf("output missing %s: %s", want, out.String())
			}
		}
	}
	if len(owners) != 4 {
		t.Fatalf("reads = %v", owners)
	}
	c := (&app{}).doctorRepositoryCmd()
	c.SetArgs(nil)
	if err := c.Execute(); err == nil {
		t.Fatal("missing repository accepted")
	}
}
