package guard

import (
	"strings"
	"testing"
)

// What the refusals name.
const buildCache, modCache = "the Go build cache", "the Go module cache"

// goCacheCorpus: every command and whether the Go cache guard refuses it,
// with the build cache at /home/u/.cache/go-build and the module cache at
// /home/u/go/pkg/mod.
var goCacheCorpus = []struct {
	cmd     string
	refused string
}{
	{`go clean -cache`, buildCache + " /home/u/.cache/go-build"},
	{`go clean --cache`, buildCache},
	{`go clean -cache=true`, buildCache},
	{`go clean -i -modcache`, modCache + " /home/u/go/pkg/mod"},
	{`cd x && go clean -cache -modcache`, buildCache},
	{`beekeeper run -- go clean -modcache`, modCache},
	{`bash -c 'go clean -cache'`, buildCache},
	{`GOCACHE=/home/u/.cache/go-build go clean -cache`, buildCache},
	{`rm -rf ~/.cache/go-build`, buildCache},
	{`rm -rf "$HOME"/.cache/go-build/*`, buildCache},
	{`rm -rf /home/u/.cache/go-build/`, buildCache},
	{`rm -rf "$(go env GOCACHE)"`, buildCache},
	{`rm -rf $GOMODCACHE`, modCache},
	{`chmod -R u+w ~/go/pkg/mod && rm -rf ~/go/pkg/mod`, modCache},
	{`rm -rf ~/go`, modCache},
	{`rm -rf ~/.cache`, buildCache},
	{`rm -rf ~/.cache/go-*`, buildCache},
	{`find ~/.cache/go-build -type f -delete`, buildCache},
	// passes
	{`go clean`, ""},
	{`go clean -testcache`, ""},
	{`go clean -fuzzcache ./...`, ""},
	{`go clean -cache=false`, ""},
	{`go clean cache`, ""},
	{`GOCACHE=$(mktemp -d) go clean -cache`, ""},
	{`GOMODCACHE=/tmp/m go clean -modcache`, ""},
	{`go test ./... -run Cache`, ""},
	{`echo "go clean -cache"`, ""},
	{`rm -rf ~/.cache/go-build/aa/xyz-d`, ""},
	{`rm -rf ~/go/pkg/mod/cache/download/example.com`, ""},
	{`rm -rf ~/.cache/other`, ""},
	{`rm -rf /tmp/go-build123`, ""},
	{`rm -rf go-build`, ""},
	{`beekeeper cache clear go --modcache`, ""},
}

func TestGoCacheGuardCorpus(t *testing.T) {
	t.Setenv("HOME", "/home/u")
	h := hook()
	h.GoCache, h.GoModCache = "/home/u/.cache/go-build", "/home/u/go/pkg/mod"
	for _, c := range goCacheCorpus {
		d := decide(t, h, "/", c.cmd, nil)
		refused := d != nil && d.PermissionDecision == decisionDeny
		switch {
		case c.refused == "" && refused:
			t.Errorf("%q is refused:\n%s", c.cmd, d.Reason)
		case c.refused == "":
		case !refused:
			t.Errorf("%q: want a refusal, got %+v", c.cmd, d)
		case !strings.Contains(d.Reason, c.refused) || !strings.Contains(d.Reason, "beekeeper cache clear go"):
			t.Errorf("%q: the refusal names neither %q nor the command:\n%s", c.cmd, c.refused, d.Reason)
		}
	}
}

// With no caches known, the guard is off.
func TestGoCacheGuardOff(t *testing.T) {
	if r := hook().goCacheRefusal("go clean -cache -modcache"); r != "" {
		t.Errorf("refused with no caches: %s", r)
	}
}
