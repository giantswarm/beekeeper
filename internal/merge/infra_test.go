package merge

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// goProxy is the Go module proxy's signature.
const goProxy = "Go module proxy"

// proxyStreamError is the go-lint job's failure of a module download from
// proxy.golang.org, as the Actions log carries it.
const proxyStreamError = "2026-10-14T14:55:02.1146372Z ##[error]../../../../go/pkg/mod/github.com/spf13/afero@v1.15.0/util.go:28:2: " +
	`golang.org/x/text@v0.41.0: read "https://proxy.golang.org/golang.org/x/text/@v/v0.41.0.zip": stream error: stream ID 143; INTERNAL_ERROR; received from peer`

func TestInfraLine(t *testing.T) {
	cases := []struct {
		name, log, signature, line string
	}{
		{
			name: "proxy.golang.org stream error",
			log: strings.Join([]string{
				"2026-10-14T14:55:01.0000000Z go: downloading golang.org/x/text v0.41.0",
				proxyStreamError,
				"2026-10-14T14:55:02.1168717Z make: *** [Makefile:196: go/bin/golangci-lint] Error 1",
				"2026-10-14T14:55:02.1173601Z ##[error]Process completed with exit code 2.",
				"2026-10-14T14:55:02.3247512Z Cleaning up orphan processes",
			}, "\n"),
			signature: goProxy,
			line:      `../../../../go/pkg/mod/github.com/spf13/afero@v1.15.0/util.go:28:2: golang.org/x/text@v0.41.0: read "https://proxy.golang.org/golang.org/x/text/@v/v0.41.0.zip": stream error: stream ID 143; INTERNAL_ERROR; received from peer`,
		},
		{
			name:      "terminal escapes around a registry rate limit",
			log:       "2026-10-14T10:00:00.0Z \x1b[31merror pulling image: toomanyrequests: You have reached your pull rate limit\x1b[0m",
			signature: "container registry",
			line:      "error pulling image: toomanyrequests: You have reached your pull rate limit",
		},
		{
			name:      "checkout against a GitHub 502",
			log:       "fatal: unable to access 'https://github.com/o/r/': The requested URL returned error: 502",
			signature: "git checkout",
			line:      "fatal: unable to access 'https://github.com/o/r/': The requested URL returned error: 502",
		},
		{
			name:      "the runner went away",
			log:       "##[error]The runner has received a shutdown signal.",
			signature: "runner",
			line:      "The runner has received a shutdown signal.",
		},
		{name: "a failing test is the change's", log: "--- FAIL: TestX (0.00s)\n    x_test.go:12: got 1, want 2\nFAIL"},
		{name: "a lint finding is the change's", log: "main.go:3:1: exported function Foo should have comment (golint)"},
		{name: "an auth refusal is no flake", log: "fatal: unable to access 'https://github.com/o/r/': The requested URL returned error: 403"},
		{
			name: "a match above the tail is not searched",
			log:  proxyStreamError + strings.Repeat("\nok", infraTail),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sig, line, ok := InfraLine(tc.log)
			if ok != (tc.signature != "") || sig != tc.signature || line != tc.line {
				t.Errorf("InfraLine = %q, %q, %v; want %q, %q", sig, line, ok, tc.signature, tc.line)
			}
		})
	}
}

func TestInfraReds(t *testing.T) {
	doc := []byte(`{"exitCode":1,"verdict":"red","checks":[` +
		`{"name":"go-lint","conclusion":"failure","url":"https://github.com/o/r/actions/runs/38584550284/job/115923395354"},` +
		`{"name":"go-test","conclusion":"failure","url":"https://github.com/o/r/actions/runs/38584550284/job/2"},` +
		`{"name":"gone","conclusion":"failure","url":"https://github.com/o/r/actions/runs/38584550284/job/3"},` +
		`{"name":"ci/circleci: build","conclusion":"failure","url":"https://circleci.com/gh/o/r/1"},` +
		`{"name":"green","conclusion":"success","url":"https://github.com/o/r/actions/runs/1/job/4"}]}`)
	var asked []string
	gh := func(_ context.Context, args ...string) ([]byte, error) {
		path := args[len(args)-1]
		asked = append(asked, path)
		switch path {
		case "repos/o/r/actions/jobs/115923395354/logs":
			return []byte(proxyStreamError + "\n##[error]Process completed with exit code 2."), nil
		case "repos/o/r/actions/jobs/2/logs":
			return []byte("--- FAIL: TestX"), nil
		}
		return nil, errors.New("HTTP 410: gone")
	}
	infra := InfraReds(context.Background(), gh, doc)
	if len(infra) != 1 || infra[0].Check != "go-lint" || infra[0].Signature != goProxy {
		t.Fatalf("infra %+v, want go-lint on the Go module proxy", infra)
	}
	if got, want := infra[0].Rerun(), "gh run rerun 38584550284 --job 115923395354 --repo o/r"; got != want {
		t.Errorf("rerun %q, want %q", got, want)
	}
	if want := fmt.Sprint([]string{"repos/o/r/actions/jobs/115923395354/logs", "repos/o/r/actions/jobs/2/logs", "repos/o/r/actions/jobs/3/logs"}); fmt.Sprint(asked) != want {
		t.Errorf("read %v, want only the red Actions jobs %v", asked, want)
	}
	if note := InfraNote(infra); !strings.HasPrefix(note, "go-lint: a transient infrastructure failure, not this pull request's breakage (Go module proxy: \"…") ||
		!strings.Contains(note, "stream error: stream ID 143; INTERNAL_ERROR; received from peer\"") || !strings.HasSuffix(note, "rerun the job (`gh run rerun 38584550284 --job 115923395354 --repo o/r`), then the same merge again") {
		t.Errorf("note %q", note)
	}
}
