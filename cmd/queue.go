package cmd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gofrs/flock"
	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/superloop"
)

// queueLocks is the directory under the state directory with one lock per
// shared issue body; queueLockWait bounds the wait for another add.
const (
	queueLocks    = "queue"
	queueLockWait = 2 * time.Minute
)

func (a *app) queueCmd() *cobra.Command {
	c := &cobra.Command{Use: "queue", Short: "Shared queues kept in an issue body", Args: cobra.NoArgs}
	c.AddCommand(&cobra.Command{
		Use:   "add <issue> <row>",
		Short: "Add a numbered row to a shared issue body without losing another writer's",
		Long: `Adds <row> after the issue body's last numbered row, a list's "n. " or a table's
"| n |", one above the highest number, and prints that number. <issue> is its
URL or owner/repo#n. A body without numbered rows gets a list.

GitHub has no conditional write of an issue body. Calls on this machine take
turns under a lock; each reads the body again right before its write, starts
over when it changed, and reads it after the write to prove the row landed. A
row already there keeps its number, so a repeated call adds nothing. An edit of
the body by other means can still fall between that read and the write.`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			r, ok := parseRef(args[0])
			if !ok {
				return usageErr("want an issue as its URL or owner/repo#n, got %q", args[0])
			}
			if (superloop.Desk{Orgs: a.cfg.GitHub.AllowedOrgs}).Outside(r.Repo) {
				return refused("%s is outside github.allowedOrgs: the desk writes nothing there", r.Repo)
			}
			n, err := a.addQueueRow(cmd.Context(), r, args[1])
			if err != nil {
				return err
			}
			_, err = fmt.Fprintf(a.out, "row %d of https://github.com/%s/issues/%d\n", n, r.Repo, r.N)
			return err
		},
	})
	return c
}

// addQueueRow adds row to r's body under r's lock.
func (a *app) addQueueRow(ctx context.Context, r github.PR, row string) (int, error) {
	dir := filepath.Join(a.store.Dir(), queueLocks)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return 0, err
	}
	base := filepath.Join(dir, fmt.Sprintf("%s-%d", strings.ReplaceAll(r.Repo, "/", "_"), r.N))
	lock := flock.New(base + ".lock")
	wait, cancel := context.WithTimeout(ctx, queueLockWait)
	defer cancel()
	if locked, err := lock.TryLockContext(wait, 100*time.Millisecond); err != nil || !locked {
		return 0, fmt.Errorf("another queue add holds %s: %w", refName(r), err)
	}
	defer func() { _ = lock.Unlock() }()
	number := strconv.Itoa(r.N)
	return github.AddQueueRow(ctx, github.IssueBody{
		Read: func(ctx context.Context) (string, error) {
			out, err := github.RunGH(ctx, "issue", "view", number, "--repo", r.Repo, "--json", "body", "--jq", ".body")
			return strings.TrimSuffix(string(out), "\n"), err
		},
		Write: func(ctx context.Context, body string) error {
			file := base + ".body"
			if err := os.WriteFile(file, []byte(body), 0o600); err != nil {
				return err
			}
			defer func() { _ = os.Remove(file) }()
			_, err := github.RunGH(ctx, "issue", "edit", number, "--repo", r.Repo, "--body-file", file)
			return err
		},
		Sleep: time.Sleep,
	}, row)
}
