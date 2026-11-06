package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/issuemonitor"
)

// issuesQueueName is the subcommand listing queued issue pointers.
const issuesQueueName = "queue"

// issueTriageEndpoint is the one place triage messages go. Scheme and host are
// fixed here so that a webhook read from the environment selects only a path.
const issueTriageEndpoint = "https://hooks.slack.com/"

var (
	issueMonitorGH = github.RunGH

	// issueTriageTransport carries triage messages; tests replace it.
	issueTriageTransport http.RoundTripper = http.DefaultTransport

	issueTriageWebhookPath = regexp.MustCompile(`^/services/[A-Za-z0-9]+/[A-Za-z0-9]+/[A-Za-z0-9]+$`)
)

func (a *app) issueMonitorDir() string { return filepath.Join(a.cfg.StateDir, "issue-monitor") }
func (a *app) issuesCmd() *cobra.Command {
	c := &cobra.Command{Use: "issues", Short: "Monitor owned issues without reading bodies or comments"}
	var record string
	var channels string
	poll := &cobra.Command{Use: "poll", Short: "Reconcile open issues once; run periodically from a trusted service", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		// Configuration names teams and secret environment variables, never webhooks.
		var envs map[string]string
		if err := json.Unmarshal([]byte(channels), &envs); err != nil {
			return errors.New("channels must map team names to webhook environment variable names")
		}
		repo, dir, ok := splitTeamRecord(record)
		if !ok {
			return errors.New("ownership must name owner/repo/directory")
		}
		owners, err := github.Teams(cmd.Context(), issueMonitorGH, repo, dir)
		if err != nil {
			return errors.New("read ownership record failed")
		}
		for r, t := range owners {
			if !slices.Contains([]string{"team-bumblebee", "team-planeteers"}, t) {
				delete(owners, r)
				continue
			}
			if os.Getenv(envs[t]) == "" {
				return fmt.Errorf("missing channel configuration for %s", t)
			}
		}
		return issuemonitor.WithState(cmd.Context(), a.issueMonitorDir(), func(s *issuemonitor.State, save func(*issuemonitor.State) error) error {
			m := issuemonitor.Monitor{GH: issueMonitorGH, Save: save, Now: a.timeNow(), Notify: func(ctx context.Context, team, message string) error {
				return postIssueTriage(ctx, os.Getenv(envs[team]), message)
			}}
			return m.Poll(cmd.Context(), s, owners)
		})
	}}
	poll.Flags().StringVar(&record, "ownership", "giantswarm/github/repositories", "Organisation ownership record")
	poll.Flags().StringVar(&channels, "channels", `{"team-bumblebee":"BEEKEEPER_BUMBLEBEE_WEBHOOK","team-planeteers":"BEEKEEPER_PLANETEERS_WEBHOOK"}`, "JSON map of team to Slack webhook environment variable")
	c.AddCommand(poll)
	for _, kind := range []string{issuesQueueName, "contributors", "cleanup"} {
		c.AddCommand(&cobra.Command{Use: kind, Short: "Show " + kind + " using pointers and classification only", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
			return issuemonitor.WithState(cmd.Context(), a.issueMonitorDir(), func(s *issuemonitor.State, _ func(*issuemonitor.State) error) error {
				switch kind {
				case issuesQueueName:
					return a.printJSON(s.Queue)
				case "contributors":
					return a.printJSON(s.Contributors)
				default:
					report := map[string]map[string][]string{}
					for ref, r := range s.Issues {
						if r.Cleanup == "" {
							continue
						}
						repo, _, _ := strings.Cut(ref, "#")
						if report[repo] == nil {
							report[repo] = map[string][]string{}
						}
						report[repo][r.Cleanup] = append(report[repo][r.Cleanup], ref)
					}
					for _, groups := range report {
						for _, refs := range groups {
							slices.Sort(refs)
						}
					}
					return a.printJSON(report)
				}
			})
		}})
	}
	return c
}
func splitTeamRecord(record string) (string, string, bool) {
	parts := strings.SplitN(record, "/", 3)
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return "", "", false
	}
	return strings.Join(parts[:2], "/"), parts[2], true
}

// issueTriageWebhookTarget returns the path of a Slack incoming webhook. Any
// other destination is refused, and the refusal never repeats the webhook.
func issueTriageWebhookTarget(webhook string) (string, error) {
	u, err := url.Parse(webhook)
	if err != nil {
		return "", errors.New("invalid triage webhook")
	}
	endpoint, err := url.Parse(issueTriageEndpoint)
	if err != nil {
		return "", errors.New("invalid triage endpoint")
	}
	if u.Scheme != endpoint.Scheme || u.Host != endpoint.Host || u.User != nil || u.RawQuery != "" || u.Fragment != "" || !issueTriageWebhookPath.MatchString(u.Path) {
		return "", errors.New("triage webhook is not a Slack incoming webhook")
	}
	return u.Path, nil
}

func postIssueTriage(ctx context.Context, webhook, message string) error {
	path, err := issueTriageWebhookTarget(webhook)
	if err != nil {
		return err
	}
	raw, _ := json.Marshal(map[string]any{paramText: message, "mrkdwn": false, "unfurl_links": false, "unfurl_media": false})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, issueTriageEndpoint, bytes.NewReader(raw))
	if err != nil {
		return errors.New("invalid triage endpoint")
	}
	req.URL.Path = path
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 30 * time.Second, Transport: issueTriageTransport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		return errors.New("triage delivery failed")
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode != http.StatusOK {
		return errors.New("triage delivery rejected")
	}
	return nil
}

// nextMonitoredIssue rechecks both open state and live membership at dispatch.
// It returns only a pointer, so no external issue text enters the worker brief.
func (a *app) nextMonitoredIssue(ctx context.Context) (*loopBoardItem, error) {
	// Avoid creating monitor state for installations that have not enabled it.
	if _, err := os.Stat(a.issueMonitorDir()); errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	var next *loopBoardItem
	err := issuemonitor.WithState(ctx, a.issueMonitorDir(), func(s *issuemonitor.State, save func(*issuemonitor.State) error) error {
		for len(s.Queue) > 0 {
			p := s.Queue[0]
			r, ok := parseRef(p.Ref)
			if !ok {
				return errors.New("invalid monitored issue pointer")
			}
			i, err := issuemonitor.Read(ctx, issueMonitorGH, r.Repo, r.N)
			if err != nil {
				return err
			}
			org, _, _ := strings.Cut(r.Repo, "/")
			if i.State != emergencyOpenState || issuemonitor.Classify(ctx, issueMonitorGH, org, i.Author.Login) != issuemonitor.Employee {
				s.Queue = s.Queue[1:]
				rec := s.Issues[p.Ref]
				rec.State = "reported-for-cleanup"
				s.Issues[p.Ref] = rec
				if err := save(s); err != nil {
					return err
				}
				continue
			}
			next = &loopBoardItem{Ref: p.Ref, URL: fmt.Sprintf("https://github.com/%s/issues/%d", r.Repo, r.N)}
			break
		}
		return nil
	})
	return next, err
}
func (a *app) monitoredIssueStarted(ctx context.Context, ref string) error {
	if _, err := os.Stat(a.issueMonitorDir()); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return issuemonitor.WithState(ctx, a.issueMonitorDir(), func(s *issuemonitor.State, _ func(*issuemonitor.State) error) error {
		if r, ok := s.Issues[ref]; ok {
			r.State = "owned"
			s.Issues[ref] = r
			s.Queue = slices.DeleteFunc(s.Queue, func(p issuemonitor.Pointer) bool { return p.Ref == ref })
		}
		return nil
	})
}

// allowMonitoredIssue also guards board picks: adding an external issue to the
// project board cannot bypass the monitor's trust boundary.
func (a *app) allowMonitoredIssue(ctx context.Context, ref string) error {
	if _, err := os.Stat(a.issueMonitorDir()); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	denied := false
	err := issuemonitor.WithState(ctx, a.issueMonitorDir(), func(s *issuemonitor.State, save func(*issuemonitor.State) error) error {
		repo, _, _ := strings.Cut(ref, "#")
		if _, known := s.Issues[ref]; !known && s.Repositories[repo] == "" {
			return nil
		}
		r, ok := parseRef(ref)
		if !ok {
			return errors.New("invalid monitored issue pointer")
		}
		i, err := issuemonitor.Read(ctx, issueMonitorGH, r.Repo, r.N)
		if err != nil {
			return err
		}
		org, _, _ := strings.Cut(r.Repo, "/")
		class := issuemonitor.Classify(ctx, issueMonitorGH, org, i.Author.Login)
		if class == issuemonitor.Employee && i.State == emergencyOpenState {
			return nil
		}
		denied = true
		s.Queue = slices.DeleteFunc(s.Queue, func(p issuemonitor.Pointer) bool { return p.Ref == ref })
		rec := s.Issues[ref]
		rec.Class = class
		rec.State = "reported-for-cleanup"
		s.Issues[ref] = rec
		return save(s)
	})
	if err != nil {
		return err
	}
	if denied {
		return issuemonitor.ErrExternal
	}
	return nil
}
