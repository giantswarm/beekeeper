package github

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
)

const dependabotTestRepo = "giantswarm/example"

func TestReadDependabotAlert(t *testing.T) {
	for _, state := range []string{"open", "dismissed", "fixed", "auto_dismissed"} {
		t.Run(state, func(t *testing.T) {
			run := func(ctx context.Context, args ...string) ([]byte, error) {
				if Repo(ctx) != dependabotTestRepo || !slices.Equal(args, []string{"api", "--method", "GET", "repos/giantswarm/example/dependabot/alerts/8"}) {
					t.Fatalf("unexpected identity or request: %s %q", Repo(ctx), args)
				}
				return fmt.Appendf(nil, `{"number":8,"state":%q,"html_url":"https://github.com/giantswarm/example/security/dependabot/8","security_advisory":{"summary":"unneeded payload"}}`, state), nil
			}
			alert, err := ReadDependabotAlert(context.Background(), run, dependabotTestRepo, 8)
			if err != nil || alert.Number != 8 || alert.State != state || alert.URL == "" {
				t.Fatalf("alert %+v, error %v", alert, err)
			}
		})
	}
}

func TestDependabotUnavailableIsNeverFixed(t *testing.T) {
	for _, response := range []string{`{}`, `null`, `[]`, `{"number":8,"state":"fixed"}`, `{"number":7,"state":"fixed","html_url":"url"}`, `{"number":8,"state":"unexpected","html_url":"url"}`, `{"number":8,"html_url":"url"}`, `not JSON`} {
		run := func(context.Context, ...string) ([]byte, error) { return []byte(response), nil }
		if alert, err := ReadDependabotAlert(context.Background(), run, dependabotTestRepo, 8); err == nil || alert.State != "" {
			t.Errorf("response %q: alert %+v, error %v", response, alert, err)
		}
	}
	for _, status := range []string{"403", "404", "429", "connection reset"} {
		calls := 0
		cause := errors.New(status)
		run := func(context.Context, ...string) ([]byte, error) { calls++; return nil, cause }
		alert, err := ReadDependabotAlert(context.Background(), run, dependabotTestRepo, 8)
		if !errors.Is(err, cause) || !strings.Contains(err.Error(), "vulnerability_alerts") || alert.State != "" || calls != 1 {
			t.Errorf("status %s: %+v %v, calls %d", status, alert, err, calls)
		}
	}
}

func TestDependabotRejectsInvalidArgumentsBeforeGitHub(t *testing.T) {
	for _, input := range []struct {
		repo   string
		number int
	}{{dependabotTestRepo, 0}, {dependabotTestRepo, -1}, {"--hostname=elsewhere", 8}, {"giantswarm/example/../../user", 8}, {"giantswarm/example?state=fixed", 8}, {"giantswarm", 8}, {"/example", 8}} {
		run := func(context.Context, ...string) ([]byte, error) {
			t.Fatal("invalid input reached GitHub")
			return nil, nil
		}
		if _, err := ReadDependabotAlert(context.Background(), run, input.repo, input.number); err == nil {
			t.Errorf("accepted %+v", input)
		}
	}
}
