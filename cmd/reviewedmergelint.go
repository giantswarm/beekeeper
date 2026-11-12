package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/giantswarm/beekeeper/internal/loop"
)

const mergeLintCommand = "golangci-lint"

type mergeLintIssue struct {
	FromLinter  string
	Text        string
	SourceLines []string
	Pos         struct {
		Filename string
		Line     int
		Column   int
	}
}

// Only a failed lint needs the extra scans. Compare whole-module findings with
// the exact fetched base, not the PR's merge base or a subsequently moved ref.
// Both scans use the same runner/toolchain. A baseline that cannot be read is a
// checker failure, never evidence that the PR needs repair.
func reviewedLintRegressions(ctx context.Context, dir, baseSHA string, args []string, run mergeCheckCommand, local bool) (string, error) {
	merged, err := readMergeLintIssues(ctx, dir, args, run)
	if err != nil {
		return "", err
	}
	baseline, err := os.MkdirTemp(filepath.Dir(dir), "lint-base-*")
	if err != nil {
		return "", err
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_, _ = runGit(cleanup, dir, "worktree", loop.LoopRemove, "--force", baseline)
		_ = os.RemoveAll(baseline) //nolint:gosec // owned temporary baseline worktree
	}()
	if _, err := runGit(ctx, dir, "worktree", loop.LoopAdd, "--detach", baseline, baseSHA); err != nil {
		return "", fmt.Errorf("creating lint baseline: %w", err)
	}
	// Each tree supplies its own repository policy; config changes in the PR
	// must still expose newly enabled findings.
	baseArgs := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		if args[i] == workerGitHubConfigOption {
			i++
			continue
		}
		if args[i] != "--no-config" {
			baseArgs = append(baseArgs, args[i])
		}
	}
	if config := golangCILintConfig(baseline); config != "" {
		if local {
			config = filepath.Join(baseline, config)
		}
		baseArgs = append(baseArgs, workerGitHubConfigOption, config)
	} else {
		baseArgs = append(baseArgs, "--no-config")
	}
	base, err := readMergeLintIssues(ctx, baseline, baseArgs, run)
	if err != nil {
		return "", err
	}
	return newMergeLintIssues(merged, base), nil
}

func readMergeLintIssues(ctx context.Context, dir string, args []string, run mergeCheckCommand) ([]mergeLintIssue, error) {
	args = append(append([]string{}, args...), "--output.json.path=stdout", "--output.text.path=", "--show-stats=false", "--path-mode=", "--path-prefix=", "--issues-exit-code=0")
	out, err := run(ctx, dir, mergeLintCommand, args...)
	if err != nil {
		return nil, fmt.Errorf("reading lint findings: %w: %s", err, summarizeMergeCheckOutput(out))
	}
	// Remote setup and linter warnings share the command's output stream. Find
	// the structured report without treating those diagnostics as findings.
	for offset := 0; offset < len(out); offset++ {
		if out[offset] != '{' {
			continue
		}
		var report struct {
			Issues json.RawMessage
		}
		if err := json.NewDecoder(strings.NewReader(out[offset:])).Decode(&report); err != nil || report.Issues == nil {
			continue
		}
		var issues []mergeLintIssue
		if err := json.Unmarshal(report.Issues, &issues); err != nil {
			return nil, fmt.Errorf("decoding lint findings: %w", err)
		}
		for _, issue := range issues {
			if issue.FromLinter == "" || issue.Text == "" || issue.Pos.Filename == "" {
				return nil, fmt.Errorf("incomplete lint finding")
			}
		}
		return issues, nil
	}
	return nil, fmt.Errorf("lint did not return a JSON findings report: %s", summarizeMergeCheckOutput(out))
}

func newMergeLintIssues(merged, base []mergeLintIssue) string {
	// Line numbers move when unrelated code is inserted. Match by file, linter,
	// diagnostic and source instead, counting occurrences so an added duplicate
	// is still a regression. Never filter to changed lines: dependencies can
	// introduce findings in unchanged files and tests.
	key := func(issue mergeLintIssue) string {
		return strings.Join([]string{filepath.Clean(issue.Pos.Filename), issue.FromLinter, issue.Text, strings.Join(issue.SourceLines, "\n")}, "\x00")
	}
	counts := make(map[string]int, len(base))
	for _, issue := range base {
		counts[key(issue)]++
	}
	var findings []string
	for _, issue := range merged {
		k := key(issue)
		if counts[k] > 0 {
			counts[k]--
			continue
		}
		findings = append(findings, fmt.Sprintf("%s:%d:%d: %s (%s)", issue.Pos.Filename, issue.Pos.Line, issue.Pos.Column, issue.Text, issue.FromLinter))
	}
	return strings.Join(findings, "\n")
}
