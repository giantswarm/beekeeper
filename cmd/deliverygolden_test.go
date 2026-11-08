package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/state"
)

// Exercise the command, not just its view helper: JSON keys, empty arrays,
// durations, event statistics and text formatting are public output.
func TestDeliveryOutputGoldens(t *testing.T) {
	oldGH := github.RunGH
	t.Cleanup(func() { github.RunGH = oldGH })
	github.RunGH = func(context.Context, ...string) ([]byte, error) {
		t.Fatal("delivery must read local state and events only")
		return nil, errors.New("unexpected GitHub call")
	}
	t.Setenv("PATH", t.TempDir())
	now := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	start := now.Add(-24 * time.Hour)
	for _, empty := range []bool{false, true} {
		for _, stats := range []bool{false, true} {
			for _, asJSON := range []bool{false, true} {
				name := "populated"
				if empty {
					name = "empty"
				}
				if stats {
					name += "-stats"
				} else {
					name += "-list"
				}
				ext := ".txt"
				if asJSON {
					ext = ".json"
				}
				t.Run(name+ext, func(t *testing.T) {
					a, out := supervisorApp(t)
					a.now, a.json = now, asJSON
					// Zero/off and a configured limit both have fixed output.
					if !empty {
						a.cfg.Delivery.WIPLimit = 4
						active := state.Delivery{Ref: "example/project#1", Pulls: []string{"example/project#11"}, Step: state.DeliveryCI, Since: start.Add(22 * time.Hour), Started: start.Add(20 * time.Hour), Agent: "worker", Waits: "checks"}
						done := state.Delivery{Ref: "example/project#2", Pulls: []string{"example/project#12"}, Step: state.DeliveryDone, Since: start.Add(3 * time.Hour), Started: start, Verdict: state.DeliveryNoRelease}
						seed(t, a, func(st *state.State) {
							// Deliberately reversed: output must sort by reference.
							st.Deliveries = []state.Delivery{done, active}
							st.DeliveriesAt = now
						})
						implementing := func(d state.Delivery) state.Delivery {
							d.Step, d.Since = state.DeliveryImplementing, d.Started
							return d
						}
						for _, d := range []state.Delivery{implementing(done), done, implementing(active), active} {
							raw, err := json.Marshal(d)
							if err != nil {
								t.Fatal(err)
							}
							if err := a.store.Record(state.Event{Verb: state.DeliveryStepVerb, At: d.Since, Detail: string(raw)}); err != nil {
								t.Fatal(err)
							}
						}
					}
					c := a.deliveryCmd()
					if stats {
						c.SetArgs([]string{"--stats", "--since", "24h"})
					}
					if err := c.Execute(); err != nil {
						t.Fatal(err)
					}
					want, err := os.ReadFile(filepath.Join("testdata", "delivery", name+ext))
					if err != nil {
						t.Fatal(err)
					}
					if out.String() != string(want) {
						t.Fatalf("got:\n%s\nwant:\n%s", out.String(), want)
					}
				})
			}
		}
	}
}
