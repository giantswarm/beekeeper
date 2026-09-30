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

// ModelServer is what the hook knows of the host's model servers.
type ModelServer struct {
	// URL is ollama's API, LemonadeURL Lemonade Server's; "" guards
	// neither.
	URL         string
	LemonadeURL string
	// LabTests are the agentlab subcommands that run turns on its models.
	LabTests []string
}

// mayLoad is what every loader contains: the cheap test before the
// configuration is read.
var mayLoad = regexp.MustCompile(`ollama|lemonade|agentlab|/api/|/v1/`)

// loader returns the pattern of a command that makes a model server load a
// model: the ollama or Lemonade CLI, an HTTP request to a server's port on a
// path that runs or pulls one, a lab test that runs turns on it. nil when no
// server is known.
func (m ModelServer) loader() *regexp.Regexp {
	var alts, ports []string
	if p := port(m.URL); p != "" {
		alts = append(alts, `ollama\s+(?:run|pull|create|push|cp)\b`)
		ports = append(ports, p)
	}
	if p := port(m.LemonadeURL); p != "" {
		alts = append(alts, `lemonade\s+(?:run|launch|chat|load|pull|bench)\b`)
		ports = append(ports, p)
	}
	if len(ports) == 0 {
		return nil
	}
	if len(m.LabTests) > 0 {
		tests := make([]string, len(m.LabTests))
		for i, t := range m.LabTests {
			tests[i] = regexp.QuoteMeta(t)
		}
		alts = append(alts, `agentlab\s+(?:`+strings.Join(tests, "|")+`)\b`)
	}
	// ollama's native and OpenAI paths, and Lemonade's under /api/v1/.
	alts = append(alts, `(?:curl|wget|xh|https?)\b[^;&|\n]*:(?:`+strings.Join(ports, "|")+
		`)/(?:api/(?:generate|chat|embed|embeddings|pull|create|push|copy)|v1/|api/v1/(?:chat|completions|responses|embeddings|reranking|load|pull|audio|images))`)
	// A command position, or the command a kubectl exec (after --) or a
	// docker exec (after the container) runs: a mention in an argument, an
	// issue comment's body say, is no command.
	at := `(?:` + pos + `|\s--\s+|\bexec\s+(?:-\S+\s+)*[\w.-]+\s+)`
	return regexp.MustCompile(`(?m)` + at + `(` + strings.Join(alts, "|") + `)`)
}

// port is url's port, "" when it has none.
func port(raw string) string {
	u, err := url.Parse(raw)
	if raw == "" || err != nil {
		return ""
	}
	return regexp.QuoteMeta(u.Port())
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
	return "Refused: `" + short(m[1]) + "` makes a host model server load a model into the iGPU's memory, RAM no cgroup " +
		"counts or caps. Only the session holding the model-server lease loads one: ask your supervisor for \"model-server\" " +
		"with the GiB your models need, claim it with `beekeeper lease claim model-server -p \"<purpose>\" --gib <n>` after its yes, " +
		"use models within that budget with keep_alive 0, and release the lease right after. The watch unloads a model no lease covers."
}
