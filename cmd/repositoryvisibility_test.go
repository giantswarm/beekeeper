//go:build unix

package cmd

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/github"
)

func TestTaskAdmissionRepositoryVisibility(t *testing.T) {
	old := repositoryVisibilityGH
	t.Cleanup(func() { repositoryVisibilityGH = old })
	a, _ := executionTestApp(t)
	a.cfg.GitHub.PublicOnlyOwners = []string{"team"}
	for _, tc := range []struct {
		name, body    string
		fail, allowed bool
	}{
		{"public", `{"visibility":"public"}`, false, true},
		{"private", `{"visibility":"private"}`, false, false},
		{"missing field", `{}`, false, false},
		{"unreadable", ``, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repositoryVisibilityGH = func(ctx context.Context, args ...string) ([]byte, error) {
				if github.Repo(ctx) != "team/repo" {
					t.Fatalf("wrong repository: %s", github.Repo(ctx))
				}
				if tc.fail {
					return nil, errors.New("unreadable")
				}
				return []byte(tc.body), nil
			}
			for _, brief := range []string{"Work https://github.com/team/repo/issues/1", "Work team/repo#1"} {
				if err := a.admitTaskVisibility(context.Background(), brief, ""); (err == nil) != tc.allowed {
					t.Errorf("admission error = %v", err)
				}
			}
			if err := a.admitTaskVisibility(context.Background(), "Work https://github.com/other/repo/issues/1", ""); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestExecutionWriteVisibilityRechecked(t *testing.T) {
	old := repositoryVisibilityGH
	t.Cleanup(func() { repositoryVisibilityGH = old })
	a, r := workerGitHubTestApp(t)
	owner, _, _ := strings.Cut(r.Repository, "/")
	a.cfg.GitHub.PublicOnlyOwners = []string{owner}
	visibility := "public"
	repositoryVisibilityGH = func(context.Context, ...string) ([]byte, error) {
		return []byte(`{"visibility":"` + visibility + `"}`), nil
	}
	request := workerGitHubRequest{Repository: r.Repository, Write: true}
	if err := a.workerGitHubLive(r.Ref, request); err != nil {
		t.Fatal(err)
	}
	visibility = "private"
	if err := a.workerGitHubLive(r.Ref, request); err == nil {
		t.Fatal("write authority survived visibility change")
	}
	request.Write = false
	if err := a.workerGitHubLive(r.Ref, request); err != nil {
		t.Fatalf("read refused: %v", err)
	}
}

func TestRemoteAndGateAdmissionRefusePrivateRepository(t *testing.T) {
	old := repositoryVisibilityGH
	t.Cleanup(func() { repositoryVisibilityGH = old })
	a, _ := executionTestApp(t)
	a.cfg.GitHub.PublicOnlyOwners = []string{"team"}
	repositoryVisibilityGH = func(context.Context, ...string) ([]byte, error) { return []byte(`{"visibility":"private"}`), nil }
	if err := a.startSSHExecution(context.Background(), "worker", "", executionStartOptions{Repository: "team/repo"}); err == nil || !strings.Contains(err.Error(), "publicOnlyOwners") {
		t.Fatalf("SSH admission did not refuse visibility: %v", err)
	}
	if err := a.gate(context.Background(), []string{"devctl", "pr", "merge", "team/repo", "1"}, time.Minute, time.Minute, false); err == nil || !strings.Contains(err.Error(), "publicOnlyOwners") {
		t.Fatalf("gate admission did not refuse visibility: %v", err)
	}
}
