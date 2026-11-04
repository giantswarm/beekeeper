package board

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
)

const (
	tidySetSecond     = "set I2"
	tidyArchiveSecond = "archive I2"
)

// A new Client on the second pass shares only the fake remote board's state.
// Successful remote writes survive the first client's error.
func TestTidyReportsPartialWritesAndResumesFromPersistedState(t *testing.T) {
	for _, failArchive := range []bool{false, true} {
		t.Run(fmt.Sprintf("archiveFailure=%t", failArchive), func(t *testing.T) {
			type entry struct {
				id, status string
				archived   bool
			}
			items := []entry{{id: "I1", status: inProgress}, {id: "I2", status: inProgress}, {id: "I3", status: inProgress}}
			failure := errors.New("synthetic remote failure")
			failed := false
			var attempts []string
			gh := func(_ context.Context, args ...string) ([]byte, error) {
				checkSelectionSchema(t, args)
				q := args[3]
				vs := map[string]string{}
				for i := 4; i+1 < len(args); i += 2 {
					k, v, _ := strings.Cut(args[i+1], "=")
					vs[k] = v
				}
				switch {
				case strings.Contains(q, "fields(first:50)"):
					return []byte(tidyMetaJSON), nil
				case strings.Contains(q, boardItemsQuery):
					var nodes []string
					for i, it := range items {
						nodes = append(nodes, closedNode(it.id, i+1, it.status, days(21), it.archived))
					}
					return []byte(tidyPage(false, nodes...)), nil
				}
				index := slices.IndexFunc(items, func(it entry) bool { return it.id == vs["i"] })
				if index < 0 {
					return nil, fmt.Errorf("unexpected mutation: %s", q)
				}
				archiving := strings.Contains(q, "archiveProjectV2Item")
				action := "set " + vs["i"]
				if archiving {
					action = "archive " + vs["i"]
				}
				attempts = append(attempts, action)
				if index == 1 && !failed && archiving == failArchive {
					failed = true
					return nil, failure
				}
				if archiving {
					items[index].archived = true
					return []byte(`{"data":{"archiveProjectV2Item":{"item":{"id":"x"}}}}`), nil
				}
				items[index].status = doneValue
				return []byte(`{"data":{"updateProjectV2ItemFieldValue":{"projectV2Item":{"id":"x"}}}}`), nil
			}
			first, err := (&Client{GH: gh, Board: tidyBoard()}).Tidy(t.Context(), now, false)
			wantItems := 1
			wantAttempts := []string{"set I1", "archive I1", tidySetSecond}
			if failArchive {
				wantItems++
				wantAttempts = append(wantAttempts, tidyArchiveSecond)
			}
			if !errors.Is(err, failure) || first == nil || len(first.Items) != wantItems || !slices.Equal(attempts, wantAttempts) {
				t.Fatalf("partial result=%+v, error=%v, attempts=%v", first, err, attempts)
			}
			if it := first.Items[0]; it.Ref != ref1 || !it.Done || !it.Archived {
				t.Fatalf("lost confirmed progress: %+v", it)
			}
			if failArchive {
				if it := first.Items[1]; it.Ref != ref2 || !it.Done || it.Archived || items[1].status != doneValue {
					t.Fatalf("failed archive reported as complete or lost Done: %+v", it)
				}
			} else if items[1].status != inProgress {
				t.Fatalf("failed status write persisted: %+v", items[1])
			}
			if items[1].archived || items[2].status != inProgress || items[2].archived {
				t.Fatalf("failed or later item changed: %+v", items)
			}

			attempts = nil
			second, err := (&Client{GH: gh, Board: tidyBoard()}).Tidy(t.Context(), now, false)
			wantAttempts = []string{tidyArchiveSecond, "set I3", "archive I3"}
			if !failArchive {
				wantAttempts = append([]string{tidySetSecond}, wantAttempts...)
			}
			if err != nil || len(second.Items) != 2 || !slices.Equal(attempts, wantAttempts) {
				t.Fatalf("resume=%+v, error=%v, attempts=%v", second, err, attempts)
			}
			if second.Items[0].Done == failArchive {
				t.Fatalf("resume repeated or omitted the Status write: %+v", second.Items[0])
			}
			for _, it := range items {
				if it.status != doneValue || !it.archived {
					t.Errorf("incomplete remote state: %+v", it)
				}
			}
		})
	}
}
