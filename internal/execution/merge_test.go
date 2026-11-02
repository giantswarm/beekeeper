package execution

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testMergeGrant(t *testing.T) (MergeAuthorization, MergeUse, string) {
	t.Helper()
	f := testFixture(t)
	now := time.Now()
	g := MergeAuthorization{ID: "grant-one", Ref: f.r.Ref, Repository: f.r.Repository, PR: 7, Head: strings.Repeat("a", 40), Issuer: "supervisor", IssuedAt: now, ExpiresAt: now.Add(time.Minute)}
	use := MergeUse{ID: g.ID, Ref: g.Ref, Repository: g.Repository, PR: g.PR, Head: g.Head}
	root := filepath.Join(f.s.Root, "grants")
	if err := WriteMergeAuthorization(root, g, "issued", now); err != nil {
		t.Fatal(err)
	}
	return g, use, root
}

func TestMergeAuthorizationDeniesEveryTupleMismatchAndRevocation(t *testing.T) {
	g, use, _ := testMergeGrant(t)
	for _, mutate := range []func(*MergeAuthorization){
		func(g *MergeAuthorization) { g.ID = "other-grant" },
		func(g *MergeAuthorization) { g.Ref.Host = "other-host" },
		func(g *MergeAuthorization) { g.Ref.Execution = "00000000-0000-0000-0000-000000000001" },
		func(g *MergeAuthorization) { g.Ref.Generation++ },
		func(g *MergeAuthorization) { g.Repository = "other/repo" },
		func(g *MergeAuthorization) { g.PR++ },
		func(g *MergeAuthorization) { g.Head = strings.Repeat("b", 40) },
		func(g *MergeAuthorization) { g.Revoked = true },
		func(g *MergeAuthorization) { g.Consumed = true },
		func(g *MergeAuthorization) { g.ExpiresAt = g.IssuedAt.Add(-time.Second) },
	} {
		bad := g
		mutate(&bad)
		if bad.Admits(use, time.Now()) == nil {
			t.Fatal("invalid authorization admitted")
		}
	}
}

func TestMergeConsumptionSurvivesLostOutcomeAndNeverRetries(t *testing.T) {
	_, use, root := testMergeGrant(t)
	calls := 0
	live := func(context.Context) error { return nil }
	merge := func(context.Context) error {
		calls++
		g, err := ReadMergeAuthorization(root, use.ID)
		if err != nil || !g.Consumed || g.Outcome != "uncertain" {
			t.Fatal("merge before durable consumption", err)
		}
		return errors.New("lost merge response")
	}
	if _, err := UseMerge(context.Background(), root, use, time.Now(), live, merge); err == nil {
		t.Fatal("uncertain result hidden")
	}
	if _, err := UseMerge(context.Background(), root, use, time.Now(), live, merge); err == nil || calls != 1 {
		t.Fatal("consumed authorization repeated merge")
	}
	grant, err := ReadMergeAuthorization(root, use.ID)
	if err != nil || grant.Outcome != "uncertain" || len(grant.Audit) < 3 || grant.Audit[len(grant.Audit)-1].Action != "denied" {
		t.Fatal("missing durable audit", err)
	}
}

func TestMergeLiveFailureDeniesWithoutConsumption(t *testing.T) {
	for _, reason := range []string{"coordinator unreachable", "head moved", "global stop", "global hold", "review revoked"} {
		t.Run(reason, func(t *testing.T) {
			_, use, root := testMergeGrant(t)
			if _, err := UseMerge(context.Background(), root, use, time.Now(), func(context.Context) error { return errors.New(reason) }, func(context.Context) error { t.Fatal("failed live guard reached merge"); return nil }); err == nil {
				t.Fatal("failed live check admitted")
			}
			grant, err := ReadMergeAuthorization(root, use.ID)
			if err != nil || grant.Consumed || grant.Audit[len(grant.Audit)-1].Action != "denied-live-check" {
				t.Fatal("denial not durable", err)
			}
		})
	}
}
