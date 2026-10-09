package cmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/proof"
)

// proofGH is the gh CLI proof upload writes through; a seam for the tests.
var proofGH proof.GH = proof.RunGH

// proofTimeout bounds one upload: a handful of API calls.
const proofTimeout = 2 * time.Minute

func (a *app) proofCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "proof",
		Short: "Put a proof image where a public GitHub thread shows it",
	}
	var repo, alt string
	upload := &cobra.Command{
		Use:   "upload <image> --repo <owner/repo>",
		Short: "Host a screenshot in a public repository and print its Markdown image line",
		Long: `upload hosts <image> (PNG, JPEG or GIF; a browse screenshot or one taken
from a transcript) in the public repository --repo, the repository of the
thread the proof is for, and prints a Markdown image line to paste into an
issue comment or a pull request body there:

  beekeeper proof upload ~/.local/state/beekeeper/browse/<id>/shot-1.jpg --repo giantswarm/beekeeper
  ![shot-1](https://raw.githubusercontent.com/giantswarm/beekeeper/<commit>/<date>/<time>-<digest>.png)

The image is re-encoded as PNG first, which drops its metadata, and
committed to the repository's images-only branch ` + proof.Branch + ` through
the GitHub API with the gh on PATH (an agent's gh carries the App's token;
beekeeper never reads it). The link is pinned to that commit. A repository
that is not public is refused. What the pixels show is the caller's to
check: a public thread carries nothing internal, in an image as in text.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if repo == "" {
				return usageErr("proof upload needs --repo <owner/repo>, the repository of the thread")
			}
			return a.proofUpload(cmd.Context(), args[0], repo, alt)
		},
	}
	upload.Flags().StringVar(&repo, "repo", "", "the public repository of the thread the proof is for (owner/repo)")
	upload.Flags().StringVar(&alt, "alt", "", "the image's alt text (default: the file's name)")
	c.AddCommand(upload)
	return c
}

func (a *app) proofUpload(ctx context.Context, file, repo, alt string) error {
	f, err := os.Open(file) //nolint:gosec // the caller's own image
	if err != nil {
		return err
	}
	raw, err := io.ReadAll(io.LimitReader(f, proof.MaxBytes+1))
	_ = f.Close()
	if err != nil {
		return err
	}
	img, err := proof.Normalize(raw)
	if err != nil {
		return fmt.Errorf("%s: %w", file, err)
	}
	if alt == "" {
		alt = strings.TrimSuffix(filepath.Base(file), filepath.Ext(file))
	}
	ctx, cancel := context.WithTimeout(ctx, proofTimeout)
	defer cancel()
	res, err := proof.Upload{GH: proofGH, Repo: repo, Alt: alt, Now: a.now}.Run(ctx, img)
	if err != nil {
		return err
	}
	if a.json {
		return a.printJSON(res)
	}
	_, err = fmt.Fprintln(a.out, res.Markdown)
	return err
}
