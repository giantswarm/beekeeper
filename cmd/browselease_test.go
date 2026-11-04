package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/lease"
	"github.com/giantswarm/beekeeper/internal/state"
)

func browseApp(t *testing.T) (*app, *bytes.Buffer) {
	t.Helper()
	a, out := stubApp(t)
	holdLease(t, a, browserResource, state.Party{Name: a.as})
	return a, out
}

func TestBrowseLeaseRefusesBeforeSteps(t *testing.T) {
	for _, holder := range []string{"", "Another session"} {
		t.Run(holder, func(t *testing.T) {
			a, _ := stubApp(t)
			bin := t.TempDir()
			ran := filepath.Join(bin, "ran")
			writeFile(t, filepath.Join(bin, "claude"), "#!/bin/sh\n: > "+ran+"\n")
			if err := os.Chmod(filepath.Join(bin, "claude"), 0o700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			if holder != "" {
				holdLease(t, a, browserResource, state.Party{Name: holder})
			}
			c := a.browseCmd()
			c.SilenceUsage, c.SilenceErrors = true, true
			c.SetArgs([]string{"Open the page"})
			err := c.ExecuteContext(t.Context())
			want := holder
			if want == "" {
				want = "nobody"
			}
			if Code(err) != ExitRefused || !strings.Contains(err.Error(), want) {
				t.Fatalf("browse = %v, want refusal naming %q", err, want)
			}
			st, err := a.store.Read()
			if err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(ran); !os.IsNotExist(err) {
				t.Fatalf("refused browse ran claude: %v", err)
			}
			if len(st.Starts) != 0 {
				t.Fatal("refused browse started a turn")
			}
		})
	}
}

func TestBrowseLeaseGrant(t *testing.T) {
	for _, tc := range []struct {
		name    string
		holder  state.Party
		age     time.Duration
		allowed bool
	}{
		{name: "valid", age: time.Minute, allowed: true},
		{name: "expired", age: time.Hour},
		{name: "other holder", holder: claimOther, age: time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := claimApp(t)
			if tc.holder.Name != "" {
				holdLease(t, a, browserResource, tc.holder)
			}
			if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
				st.Grants = []state.Grant{{Resource: browserResource, To: claimMe, At: a.now.Add(-tc.age)}}
				return nil, nil
			}); err != nil {
				t.Fatal(err)
			}
			err := a.browseLease()
			if tc.allowed {
				if err != nil {
					t.Fatal(err)
				}
				h, err := lease.Dir(a.cfg.LeaseDir).Get(browserResource)
				if err != nil || h == nil || !h.Party().Is(claimMe) {
					t.Fatalf("holder = %v, %v", h, err)
				}
				st, err := a.store.Read()
				if err != nil || len(st.Grants) != 0 {
					t.Fatalf("grant was not consumed: %v", err)
				}
				if err := a.browseLease(); err != nil {
					t.Fatalf("holder retry: %v", err)
				}
			} else if Code(err) != ExitRefused {
				t.Fatalf("browseLease = %v, want refusal", err)
			}
		})
	}
}
