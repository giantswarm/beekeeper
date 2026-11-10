//go:build unix

package github

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

func TestBudgetCredentialsIgnoreAmbientRepositoryAndTokens(t *testing.T) {
	bin := t.TempDir()
	gh := "#!/bin/sh\n[ -z \"$GH_TOKEN$GITHUB_TOKEN$GH_REPO$DEVCTL_AUTH_EXEC_CHAIN\" ] || exit 7\nprintf 'gh-fixture\\n'\n"
	devctl := "#!/bin/sh\nif [ \"$1 $2\" = 'auth status' ]; then printf '%s\\n' '{\"github\":{\"present\":true}}'; else [ \"$GH_REPO\" = 'giantswarm/beekeeper' ] || exit 7; printf 'app-fixture\\n'; fi\n"
	for name, script := range map[string]string{"gh": gh, "devctl": devctl} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(script), 0o700); err != nil { //nolint:gosec // executable test fixture
			t.Fatal(err)
		}
	}
	link := t.TempDir()
	if err := os.Symlink(filepath.Join(bin, "devctl"), filepath.Join(link, "gh")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", link+string(os.PathListSeparator)+bin)
	t.Setenv("GH_TOKEN", "ambient-fixture")
	t.Setenv("GITHUB_TOKEN", "ambient-fixture")
	t.Setenv("GH_REPO", "personal/checkout")
	t.Setenv("DEVCTL_AUTH_EXEC_CHAIN", "ambient-fixture")
	for range 2 {
		credentials := BudgetCredentials(context.Background())
		if len(credentials) != 2 {
			t.Fatalf("got %d identities, want two", len(credentials))
		}
		for _, c := range credentials {
			if c.Err != nil {
				t.Fatalf("identity %s: %v", c.Identity, c.Err)
			}
			if c.Identity == IdentityGH && c.Value != "gh-fixture" || c.Identity == IdentityApp && c.Value != "app-fixture" {
				t.Fatal("credential routed through wrong identity")
			}
		}
		t.Chdir(t.TempDir())
	}
}

func TestBudgetDevctlDiscoversGHLink(t *testing.T) {
	for _, name := range []string{"devctl", "devctl.exe"} {
		t.Run(name, func(t *testing.T) {
			target := filepath.Join(t.TempDir(), name)
			if err := os.WriteFile(target, []byte("offline resolution fixture"), 0o700); err != nil { //nolint:gosec // executable test fixture
				t.Fatal(err)
			}
			bin := t.TempDir()
			if err := os.Symlink(target, filepath.Join(bin, "gh")); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", bin)
			got, err := budgetDevctl()
			if err != nil || got != target {
				t.Fatalf("budgetDevctl() = %q, %v; want %q", got, err, target)
			}
			if got, err := budgetGH(); err == nil {
				t.Fatalf("budgetGH() accepted devctl link: %q", got)
			}
		})
	}
}

func TestLowestBudgetKeepsLowCounterAndHigherIdentityRefusal(t *testing.T) {
	now := time.Now()
	b := LowestBudget(map[string]Budget{
		IdentityGH:  {Identity: IdentityGH, Remaining: 4800, GraphQL: &GraphQL{Identity: IdentityGH, Remaining: 1941, Reset: now.Add(20 * time.Minute)}},
		IdentityApp: {Identity: IdentityApp, Remaining: 3000, GraphQL: &GraphQL{Identity: IdentityApp, Remaining: 4910, Reset: now.Add(54 * time.Minute), Refused: "secondary limit"}},
	}, now)
	if b.GraphQL.Identity != IdentityGH || b.GraphQL.Remaining != 1941 || !b.GraphQL.Blocks(now) || len(b.GraphQL.BlockedBy) != 1 || b.GraphQL.BlockedBy[0] != IdentityApp {
		t.Fatalf("higher identity hid the low counter or its refusal: %+v", b.GraphQL)
	}
	if len(b.Identities[IdentityGH].GraphQL.BlockedBy) != 0 {
		t.Fatal("aggregate mutated an identity's cached reading")
	}
}

func TestDevctlEnvFollowsGuardsToggle(t *testing.T) {
	t.Cleanup(func() { SetDevctlSkipVersionCheck(false) })
	const optOut = DevctlSkipVersionCheckEnv + "=true"
	base := []string{"PATH=/usr/bin", optOut}
	if got := DevctlEnv(base, false); slices.Contains(got, optOut) {
		t.Fatalf("DevctlEnv(skip=false) kept %s: %v", optOut, got)
	}
	if got := DevctlEnv([]string{"PATH=/usr/bin"}, true); !slices.Contains(got, optOut) {
		t.Fatalf("DevctlEnv(skip=true) lacked %s: %v", optOut, got)
	}
	SetDevctlSkipVersionCheck(true)
	if got := budgetEnv([]string{"PATH=/usr/bin"}); !slices.Contains(got, optOut) {
		t.Fatalf("budgetEnv with skip=true lacked %s: %v", optOut, got)
	}
	SetDevctlSkipVersionCheck(false)
	if got := budgetEnv(base); slices.Contains(got, optOut) {
		t.Fatalf("budgetEnv with skip=false kept %s: %v", optOut, got)
	}
}
