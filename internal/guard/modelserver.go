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

// loader is the pattern of a command that makes a model server load a model.
type loader struct {
	// start matches, at a command position, the ollama or Lemonade CLI, a lab
	// test that runs turns on the models, or an HTTP client (group 1).
	start *regexp.Regexp
	// call matches an HTTP client's whole call that runs or pulls a model:
	// the request to a server's port on such a path. It reads the command's
	// text as written, because a quoted URL is masked in the shell scan.
	call *regexp.Regexp
}

// loader returns what recognises a command that makes a model server load a
// model: the ollama or Lemonade CLI, an HTTP request to a server's port on a
// path that runs or pulls one, a lab test that runs turns on it. nil when no
// server is known.
func (m ModelServer) loader() *loader {
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
	alts = append(alts, httpClient)
	// A command position, or the command a kubectl exec (after --) or a
	// docker exec (after the container) runs: a mention in an argument, an
	// issue comment's body say, is no command.
	at := `(?:` + pos + `|\s--\s+|\bexec\s+(?:-\S+\s+)*[\w.-]+\s+)`
	return &loader{
		start: regexp.MustCompile(`(?m)` + at + `(` + strings.Join(alts, "|") + `)`),
		// ollama's native and OpenAI paths, and Lemonade's under /api/v1/.
		call: regexp.MustCompile(`^` + httpClient + `[^;&|\n]*:(?:` + strings.Join(ports, "|") +
			`)/(?:api/(?:generate|chat|embed|embeddings|pull|create|push|copy)|v1/|api/v1/(?:chat|completions|responses|embeddings|reranking|load|pull|audio|images))`),
	}
}

// httpClient is the commands that make an HTTP request.
const httpClient = `(?:curl|wget|xh|https?)\b`

var httpStart = regexp.MustCompile(`^` + httpClient)

// find returns the call of cmd that loads a model, "" for none: the first
// match of the loader at a command position of the shell (commandMatch) whose
// command is no help or version print and, for an HTTP client, whose call
// reaches a model path.
func (l *loader) find(cmd string) string {
	var call string
	keep := func(cmd string, m []int) bool {
		if !notHelp(cmd, m) {
			return false
		}
		if httpStart.MatchString(cmd[m[2]:m[3]]) {
			call = l.call.FindString(cmd[m[2]:])
			return call != ""
		}
		call = cmd[m[2]:m[3]]
		return true
	}
	if commandMatch(l.start, cmd, keep) != nil {
		return call
	}
	// A here-document fed to a shell a kubectl or docker exec runs is that
	// shell's program, which commandMatch (local shells only) leaves out.
	sc := scanShell(cmd)
	segs := sc.segments()
	for _, hd := range sc.heredocs {
		for _, sg := range segs {
			if hd.op >= sg.start && hd.op < sg.end && execShell.MatchString(sc.plain[sg.start:sg.end]) {
				if commandMatch(l.start, cmd[hd.start:hd.end], keep) != nil {
					return call
				}
			}
		}
	}
	return ""
}

// execShell is a command that execs a shell in a pod or container.
var execShell = regexp.MustCompile(`\bexec\b[^;&|\n]*\s(?:sh|bash|zsh)\b`)

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
// session holding the model-server lease. One that only prints its help or
// version (helpOnly) loads nothing and passes.
func (h Hook) modelServerRefusal(cmd, session string) string {
	if h.ModelServer == nil || h.Leases == nil || !mayLoad.MatchString(cmd) {
		return ""
	}
	re := h.ModelServer().loader()
	if re == nil {
		return ""
	}
	call := re.find(cmd)
	if call == "" {
		return ""
	}
	if slices.ContainsFunc(h.Leases(), func(l lease.Holder) bool {
		return l.Env == modelServerLease && session != "" && (l.Session == session || l.HostSession == "local_"+session)
	}) {
		return ""
	}
	return "Refused: `" + short(call) + "` makes a host model server load a model into the iGPU's memory, RAM no cgroup " +
		"counts or caps. Only the session holding the model-server lease loads one: ask your supervisor for \"model-server\" " +
		"with the GiB your models need, claim it with `beekeeper lease claim model-server -p \"<purpose>\" --gib <n>` after its yes, " +
		"use models within that budget with keep_alive 0, and release the lease right after. The watch unloads a model no lease covers."
}
