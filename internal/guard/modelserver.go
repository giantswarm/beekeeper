package guard

import (
	"net/url"
	"regexp"
	"slices"
	"strings"

	"github.com/giantswarm/beekeeper/internal/lease"
)

// modelServerLease is the lease on the host's model server.
const modelServerLease = "model-server"

// ModelServer is what the hook knows of the host's model server.
type ModelServer struct {
	// URL is its API; "" guards nothing.
	URL string
	// LabTests are the agentlab subcommands that run turns on its models.
	LabTests []string
}

// mayLoad is what every loader contains: the cheap test before the
// configuration is read.
var mayLoad = regexp.MustCompile(`ollama|agentlab|/api/|/v1/`)

// loader returns the pattern of a command that makes the model server load a
// model: the ollama CLI, an HTTP request to its port on a path that runs or
// pulls one, a lab test that runs turns on it. nil when no server is known.
func (m ModelServer) loader() *regexp.Regexp {
	u, err := url.Parse(m.URL)
	if m.URL == "" || err != nil || u.Port() == "" {
		return nil
	}
	alts := []string{`ollama\s+(?:run|pull|create|push|cp)\b`}
	if len(m.LabTests) > 0 {
		tests := make([]string, len(m.LabTests))
		for i, t := range m.LabTests {
			tests[i] = regexp.QuoteMeta(t)
		}
		alts = append(alts, `agentlab\s+(?:`+strings.Join(tests, "|")+`)\b`)
	}
	http := `(?:curl|wget|xh|https?)\b[^;&|\n]*:` + regexp.QuoteMeta(u.Port()) +
		`/(?:api/(?:generate|chat|embed|embeddings|pull|create|push|copy)|v1/)`
	return regexp.MustCompile(`(?m)(?:` + pos + `(` + strings.Join(alts, "|") + `)|(` + http + `))`)
}

// modelServerRefusal returns why the hook refuses cmd, "" when it passes: a
// command that loads a model on the host's model server runs only in the
// session holding the model-server lease.
func (h Hook) modelServerRefusal(cmd, session string) string {
	if h.ModelServer == nil || h.Leases == nil || !mayLoad.MatchString(cmd) {
		return ""
	}
	re := h.ModelServer().loader()
	if re == nil {
		return ""
	}
	m := re.FindStringSubmatch(cmd)
	if m == nil {
		return ""
	}
	if slices.ContainsFunc(h.Leases(), func(l lease.Holder) bool {
		return l.Env == modelServerLease && session != "" && (l.Session == session || l.HostSession == "local_"+session)
	}) {
		return ""
	}
	return "Refused: `" + short(m[1]+m[2]) + "` makes the host's model server load a model into the iGPU's memory, RAM no cgroup " +
		"counts or caps. Only the session holding the model-server lease loads one: ask your supervisor for \"model-server\" " +
		"with the GiB your models need, claim it with `beekeeper lease claim model-server -p \"<purpose>\" --gib <n>` after its yes, " +
		"use models within that budget with keep_alive 0, and release the lease right after. The watch unloads a model no lease covers."
}
