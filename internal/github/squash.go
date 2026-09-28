package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// The plain squash merge's exit codes and verdicts, devctl pr merge's own
// (docs/pr-merge.md in giantswarm/devctl), so the gate reads either the same.
const (
	SquashMerged        = 0
	SquashRed           = 1
	SquashTimeout       = 2
	SquashNotApplicable = 3
	SquashRefused       = 5
	SquashTooling       = 7
)

var squashVerdicts = map[int]string{
	SquashMerged: "merged", SquashRed: "red", SquashTimeout: "timeout",
	SquashNotApplicable: "not_applicable", SquashRefused: "refused", SquashTooling: "usage",
}

// SquashDoc is the plain squash merge's JSON document, shaped as devctl pr
// merge's: the gate parses mergeCommitSha and release alike.
type SquashDoc struct {
	Command        string  `json:"command"`
	Route          string  `json:"route"`
	SchemaVersion  int     `json:"schemaVersion"`
	ExitCode       int     `json:"exitCode"`
	Verdict        string  `json:"verdict"`
	Reason         string  `json:"reason"`
	Repo           string  `json:"repository"`
	Number         int     `json:"number"`
	HeadSha        string  `json:"headSha,omitempty"`
	MergeCommitSha string  `json:"mergeCommitSha,omitempty"`
	Release        *string `json:"release"`
}

// SquashRoute names the route in the document and the gate's lines.
const SquashRoute = "plain squash merge"

// GH runs the gh CLI and returns its stdout; err carries its stderr.
type GH func(ctx context.Context, args ...string) ([]byte, error)

// RunGH is GH through the gh binary.
func RunGH(ctx context.Context, args ...string) ([]byte, error) {
	var stderr bytes.Buffer
	c := exec.CommandContext(ctx, "gh", args...) //nolint:gosec // gh with the merge's validated owner/repo and number
	c.Stderr = &stderr
	out, err := c.Output()
	if err != nil {
		return out, fmt.Errorf("gh %s: %v: %s", strings.Join(args[:min(len(args), 3)], " "), err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

// Squash is one plain squash merge: the route of a repository devctl does
// not serve (its GitHub App login reaches the giantswarm organisation and
// public repositories only), made with the gh CLI's login as the person.
type Squash struct {
	GH       GH
	Repo     string
	Number   int
	Timeout  time.Duration
	Poll     time.Duration
	Progress io.Writer
	Now      func() time.Time
	Sleep    func(time.Duration)
}

type squashPull struct {
	State             string `json:"state"`
	IsDraft           bool   `json:"isDraft"`
	Mergeable         string `json:"mergeable"`
	HeadRefOid        string `json:"headRefOid"`
	HeadRefName       string `json:"headRefName"`
	Title             string `json:"title"`
	IsCrossRepository bool   `json:"isCrossRepository"`
	Author            struct {
		Login string `json:"login"`
		IsBot bool   `json:"is_bot"`
	} `json:"author"`
}

type squashCheck struct {
	Name   string `json:"name"`
	Bucket string `json:"bucket"`
}

// Run waits until the head's checks finished, all green, and squash-merges
// it through the merge API with that head as the expected one, the subject
// "<title> (#<n>)", then deletes the head branch. It refuses what devctl
// refuses before its wait: a draft, a closed, merged or conflicting pull
// request (3) and another human's (5). Its document says what happened;
// nothing waits for a release.
func (s Squash) Run(ctx context.Context) SquashDoc {
	doc := SquashDoc{Command: "pr merge", Route: SquashRoute, SchemaVersion: 1, Repo: s.Repo, Number: s.Number}
	end := func(rc int, format string, args ...any) SquashDoc {
		doc.ExitCode, doc.Verdict, doc.Reason = rc, squashVerdicts[rc], fmt.Sprintf(format, args...)
		return doc
	}
	me, err := s.GH(ctx, "api", "user", "--jq", ".login")
	if err != nil {
		return end(SquashTooling, "the gh login does not answer: %v", err)
	}
	login := strings.TrimSpace(string(me))
	deadline := s.Now().Add(s.Timeout)
	var p squashPull
	for {
		if p, err = s.pull(ctx); err != nil {
			return end(SquashTooling, "%v", err)
		}
		doc.HeadSha = p.HeadRefOid
		switch {
		case p.State != Open:
			return end(SquashNotApplicable, "%s#%d is %s", s.Repo, s.Number, strings.ToLower(p.State))
		case p.IsDraft:
			return end(SquashNotApplicable, "%s#%d is a draft", s.Repo, s.Number)
		case p.Mergeable == "CONFLICTING":
			return end(SquashNotApplicable, "%s#%d conflicts with its base", s.Repo, s.Number)
		case !p.Author.IsBot && !strings.HasSuffix(p.Author.Login, "[bot]") && !strings.EqualFold(p.Author.Login, login):
			return end(SquashRefused, "%s#%d was opened by %s, not by %s: another person's pull request is theirs to merge", s.Repo, s.Number, p.Author.Login, login)
		}
		checks, err := s.checks(ctx)
		if err != nil {
			return end(SquashTooling, "%v", err)
		}
		red, pending := bucketed(checks)
		if len(red) > 0 {
			return end(SquashRed, "%s#%d at %s is red: %s", s.Repo, s.Number, short(p.HeadRefOid), strings.Join(red, ", "))
		}
		if len(pending) == 0 {
			break
		}
		if !s.Now().Before(deadline) {
			return end(SquashTimeout, "%s#%d at %s still runs %s after %s", s.Repo, s.Number, short(p.HeadRefOid), strings.Join(pending, ", "), s.Timeout)
		}
		s.say("waiting for %s at %s: %s", fmt.Sprintf("%s#%d", s.Repo, s.Number), short(p.HeadRefOid), strings.Join(pending, ", "))
		s.Sleep(s.Poll)
	}
	s.say("green at %s: squash-merging %s#%d", short(p.HeadRefOid), s.Repo, s.Number)
	out, err := s.GH(ctx, "api", "-X", "PUT", fmt.Sprintf("repos/%s/pulls/%d/merge", s.Repo, s.Number),
		"-f", "merge_method=squash", "-f", "sha="+p.HeadRefOid, "-f", fmt.Sprintf("commit_title=%s (#%d)", p.Title, s.Number))
	if err != nil {
		// GitHub declines as the pull request stands (405 a rule, 409 the head moved).
		if strings.Contains(err.Error(), "HTTP 405") || strings.Contains(err.Error(), "HTTP 409") {
			return end(SquashNotApplicable, "GitHub declines the merge: %v", err)
		}
		return end(SquashTooling, "the merge call fails: %v", err)
	}
	var merged struct {
		Sha string `json:"sha"`
	}
	if json.Unmarshal(out, &merged) != nil || merged.Sha == "" {
		return end(SquashTooling, "the merge call answers without a merge commit: %s", strings.TrimSpace(string(out)))
	}
	doc.MergeCommitSha = merged.Sha
	reason := fmt.Sprintf("squash-merged as %s", short(merged.Sha))
	if p.IsCrossRepository {
		reason += "; the head lives in a fork and is left alone"
	} else if _, err := s.GH(ctx, "api", "-X", "DELETE", fmt.Sprintf("repos/%s/git/refs/heads/%s", s.Repo, p.HeadRefName)); err != nil &&
		!strings.Contains(err.Error(), "HTTP 422") {
		reason += fmt.Sprintf("; the branch %s is not deleted: %v", p.HeadRefName, err)
	}
	return end(SquashMerged, "%s; no release wait on this route", reason)
}

func (s Squash) pull(ctx context.Context) (squashPull, error) {
	var p squashPull
	out, err := s.GH(ctx, "pr", "view", strconv.Itoa(s.Number), "--repo", s.Repo, "--json",
		"state,isDraft,mergeable,headRefOid,headRefName,title,isCrossRepository,author")
	if err != nil {
		return p, err
	}
	if err := json.Unmarshal(out, &p); err != nil {
		return p, fmt.Errorf("gh pr view %d --repo %s: %w", s.Number, s.Repo, err)
	}
	return p, nil
}

// checks reads the head's checks; gh's exit code says pending or red, which
// the buckets say too, so only an unparsable answer is an error. A head
// without checks has none to wait for.
func (s Squash) checks(ctx context.Context) ([]squashCheck, error) {
	out, err := s.GH(ctx, "pr", "checks", strconv.Itoa(s.Number), "--repo", s.Repo, "--json", "name,bucket")
	var cs []squashCheck
	if jerr := json.Unmarshal(out, &cs); jerr == nil {
		return cs, nil
	}
	if err != nil && strings.Contains(err.Error(), "no checks reported") {
		return nil, nil
	}
	return nil, errors.Join(fmt.Errorf("gh pr checks %d --repo %s answers no checks", s.Number, s.Repo), err)
}

// bucketed names the red and the pending checks.
func bucketed(cs []squashCheck) (red, pending []string) {
	for _, c := range cs {
		switch c.Bucket {
		case "fail", "cancel":
			red = append(red, c.Name)
		case "pending":
			pending = append(pending, c.Name)
		}
	}
	return red, pending
}

func (s Squash) say(format string, args ...any) {
	if s.Progress != nil {
		_, _ = fmt.Fprintf(s.Progress, format+"\n", args...)
	}
}

func short(sha string) string { return sha[:min(len(sha), 7)] }
