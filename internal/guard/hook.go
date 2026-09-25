package guard

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/giantswarm/beekeeper/internal/lease"
)

// start is where a command starts: start of line, after ; & | ( $( or
// then/do/else.
const start = `(?:^|[;&|(]\s*|\$\(\s*|\b(?:then|do|else)\s+)`

// pos is a command position: a start with optional wrappers (timeout 600,
// time, nice, env, VAR=val, command).
const pos = start + `(?:(?:timeout\s+\S+|time|nice(?:\s+-n\s*\d+)?|env(?:\s+\w+=\S*)*|command|\w+=\S*)\s+)*`

// mergePos is a command position for the merge gate: a start with any of the
// prefix commands that run their arguments as a command, with their options.
// The build rewrite keeps pos: a wider one would change what it wraps.
const mergePos = start + `(?:(?:` +
	`flock(?:\s+(?:-[wE]\s*\S+|--(?:timeout|wait|conflict-exit-code)(?:=|\s+)\S+|-[a-zA-Z]+|--[\w-]+))*\s+[^\s;&|()-]\S*` +
	`|timeout(?:\s+(?:-[ks]\s*\S+|-\S+))*\s+\d\S*` +
	`|nice(?:\s+(?:-n\s*-?\d+|-\d+|--adjustment=-?\d+))?` +
	`|ionice(?:\s+(?:-[cnpP]\s*\S+|-\S+))*` +
	`|chrt(?:\s+-\S+)*\s+\d+` +
	`|stdbuf(?:\s+(?:-[ioe]\s*\S+|--\S+))+` +
	`|env(?:\s+(?:-[uC]\s*\S+|-\S+|\w+=\S*))*` +
	`|setsid(?:\s+-\S+)*` +
	`|nohup|time(?:\s+-p)?|command|exec|\w+=\S*` +
	`)\s+)*`

// devctlMerge is devctl pr merge, devctl by name or by a path that starts
// with /, ~/, ./, ../ or a variable ($HOME/bin/devctl).
const devctlMerge = `(?:(?:~|\.\.?|\$\{?\w+\}?)?/(?:[^\s;&|()'"<>=]*/)?)?devctl\s+pr\s+merge\b`

var (
	heavy = regexp.MustCompile(`(?m)` + pos + `(` + strings.Join([]string{
		`go\s+(?:test|build|vet|install|generate|run)\b`,
		`golangci-lint\b`,
		`cargo\s+(?:build|test|run|clippy|check|doc)\b`,
		`(?:yarn|npm|pnpm|bun)\s+(?:run\s+)?(?:test|build|tsc|lint|typecheck|ci(?::\S+)?|e2e\S*|test:\S+|build:\S+|backstage-cli|install|dedupe|workspace\s+\S+\s+(?:test|build|tsc|lint|backstage-cli))\b`,
		`npx\s+(?:jest|vitest|tsc|playwright|backstage-cli|eslint|webpack|vite|esbuild)\b`,
		`(?:jest|vitest|tsc|playwright|eslint|webpack|vite|esbuild|ginkgo|pytest|tox|mvn|gradle|bazel|staticcheck|govulncheck|trivy|controller-gen|kubebuilder|goreleaser)\b`,
		`ko\s+build\b`,
		`make\b`,
		`docker\s+(?:build|buildx\s+build)\b`,
		`kind\s+(?:load|build)\b`,
		`agentlab\s+(?:up|platform|test|platform-test|backstage-test|down)\b`,
		`graphify\s+(?:update|build|extract|label|cluster-only|scan)\b`,
	}, "|") + `)`)
	// lightMake: make targets that build nothing (RE2 has no lookahead).
	lightMake = regexp.MustCompile(`^\s+(?:-n\b|--dry-run\b|help\b|version\b|clean\b|fmt\b|print-|list\b)`)
	// merge: devctl pr merge at a command position, the gate goes before it.
	merge = regexp.MustCompile(`(?m)` + mergePos + `(` + devctlMerge + `)`)
	// anyMerge: devctl pr merge anywhere; gated: the gate ends the text before it.
	anyMerge = regexp.MustCompile(devctlMerge)
	gated    = regexp.MustCompile(`\bgate\s+(?:--wait\s+\S+\s+)?--\s+$`)
	// shellC: a shell's -c option up to the quote opening its command string.
	shellC  = regexp.MustCompile(`(?:^|[\s;&|(/])(?:ba|z|da|k)?sh\s+(?:-[a-zA-Z]+\s+)*-[a-zA-Z]*c[a-zA-Z]*\s+(['"])`)
	lab     = regexp.MustCompile(`(?m)` + pos + `(agentlab\s+up\b|kind\s+create\s+cluster\b)`)
	trivial = regexp.MustCompile(`^\s*\S+(?:\s+\S+)?\s+(?:--version|-V|--help|-h|help)\s*$`)
	// wrapped: the command invokes the wrapper itself, by name or path, at a
	// command position. A wrapper path merely mentioned (ls …/memcap,
	// m=$(ls …/memcap), M=…/memcap) is no wrapper, so pos's assignments may
	// not hold a substitution here.
	wrapped = regexp.MustCompile(`(?m)(?:^|[;&|(]\s*|\b(?:then|do|else)\s+)(?:(?:timeout\s+\S+|time|nice(?:\s+-n\s*\d+)?|env|command|\w+=[^\s$()]*)\s+)*` +
		`["']?(?:[^\s=;&|()'"]*/)?(?:memcap|beekeeper["']?\s+run)(?:["']?\s|$)`)
	kindName  = regexp.MustCompile(`--name[= ]\s*([\w.-]+)`)
	cdArg     = regexp.MustCompile(`(?:^|[;&|]\s*)cd\s+(\S+)`)
	clusterRe = regexp.MustCompile(`^\s*clusterName:\s*"?([\w.-]+)"?`)
	shellSafe = regexp.MustCompile(`^[\w@%+=:,./-]+$`)
)

// The hook's permission decisions and the Bash tool's background flag.
const (
	decisionAllow = "allow"
	decisionDeny  = "deny"
	backgroundKey = "run_in_background"
)

// MaxLabs is how many kind clusters the machine runs at most.
const MaxLabs = 2

// Hook decides one PreToolUse event.
type Hook struct {
	// Self is the absolute path of the beekeeper binary a rewrite names.
	Self string
	// Clusters lists the running kind clusters.
	Clusters func() []string
	// Leases lists the held leases.
	Leases func() []lease.Holder
}

type hookOutput struct {
	HookEventName      string         `json:"hookEventName"`
	PermissionDecision string         `json:"permissionDecision"`
	Reason             string         `json:"permissionDecisionReason,omitempty"`
	UpdatedInput       map[string]any `json:"updatedInput,omitempty"`
}

// Decide returns the hook's JSON answer for the event, nil to let the call
// pass unchanged. Malformed input passes.
func (h Hook) Decide(input []byte) []byte {
	var ev struct {
		ToolName  string         `json:"tool_name"`
		ToolInput map[string]any `json:"tool_input"`
		CWD       string         `json:"cwd"`
	}
	dec := json.NewDecoder(bytes.NewReader(input))
	dec.UseNumber()
	if dec.Decode(&ev) != nil || ev.ToolName != "Bash" {
		return nil
	}
	cmd, _ := ev.ToolInput["command"].(string)
	if strings.TrimSpace(cmd) == "" || trivial.MatchString(cmd) {
		return nil
	}
	bg, _ := ev.ToolInput[backgroundKey].(bool)
	cmd, gated := h.gate(cmd, bg)
	if fixed, ok := h.hiddenMerges(cmd, bg); ok {
		return answer(hookOutput{PermissionDecision: decisionDeny, Reason: "Refused: a devctl pr merge inside a shell's -c string " +
			"runs outside the merge gate. Run it as its own command, or with the gate written in:\n" + fixed})
	}
	if wrapped.MatchString(cmd) {
		return h.rewrite(ev.ToolInput, cmd, gated, bg)
	}
	cwd := ev.CWD
	if cwd == "" {
		cwd, _ = os.Getwd()
	}

	if m := lab.FindStringSubmatchIndex(cmd); m != nil {
		running := h.Clusters()
		target := labTarget(cmd, m, cwd)
		if !slices.Contains(running, target) && len(running) >= MaxLabs {
			return answer(hookOutput{PermissionDecision: decisionDeny, Reason: fmt.Sprintf(
				"Refused: %d kind labs already run (%s) and this machine allows at most %d. `%s` would create a third (%s). "+
					"Reuse a running lab — claim it with `beekeeper lease claim` — or wait until one is torn down; do not poll for it.\nleases:\n%s",
				len(running), strings.Join(running, ", "), MaxLabs, strings.TrimSpace(cmd[m[2]:m[3]]), target, leaseLines(h.Leases()))})
		}
	}

	if !isHeavy(cmd) {
		return h.rewrite(ev.ToolInput, cmd, gated, bg)
	}
	prefix := ""
	if bg {
		prefix = "MEMCAP_WAIT=60m "
	}
	return h.rewrite(ev.ToolInput, prefix+ShellQuote(h.Self)+" run -- zsh -c "+ShellQuote(cmd), true, bg)
}

// gate puts "beekeeper gate --" before every devctl pr merge at a command
// position, behind its prefix commands, so that only the devctl invocation
// is wrapped and pipelines and lists run as written.
func (h Hook) gate(cmd string, bg bool) (string, bool) {
	var at []int
	for _, m := range merge.FindAllStringSubmatchIndex(cmd, -1) {
		at = append(at, m[2])
	}
	return h.insertGate(cmd, at, bg), len(at) > 0
}

// hiddenMerges returns cmd with the gate before each devctl pr merge the gate
// rewrite left inside a sh, bash or zsh -c string, and whether there was one.
// The hook refuses such a command rather than rewriting a quoted string.
func (h Hook) hiddenMerges(cmd string, bg bool) (string, bool) {
	var at []int
	for _, m := range shellC.FindAllStringSubmatchIndex(cmd, -1) {
		body := cmd[m[1]:]
		end := closingQuote(body, cmd[m[2]])
		for _, mm := range anyMerge.FindAllStringIndex(body[:end], -1) {
			if !gated.MatchString(body[:mm[0]]) {
				at = append(at, m[1]+mm[0])
			}
		}
	}
	return h.insertGate(cmd, at, bg), len(at) > 0
}

// insertGate puts the gate before each offset in at, in ascending order; a
// background merge waits up to 30 minutes for its turn.
func (h Hook) insertGate(cmd string, at []int, bg bool) string {
	gate := ShellQuote(h.Self) + " gate -- "
	if bg {
		gate = ShellQuote(h.Self) + " gate --wait 30m -- "
	}
	for i := len(at) - 1; i >= 0; i-- {
		cmd = cmd[:at[i]] + gate + cmd[at[i]:]
	}
	return cmd
}

// closingQuote is the offset of the quote q closing the string s starts in,
// len(s) when it does not close.
func closingQuote(s string, q byte) int {
	for i := 0; i < len(s); i++ {
		switch {
		case s[i] == q:
			return i
		case s[i] == '\\' && q == '"':
			i++
		}
	}
	return len(s)
}

// rewrite allows the call with cmd as its command when changed, a
// foreground call's timeout raised to the Bash tool's 10 minutes.
func (h Hook) rewrite(input map[string]any, cmd string, changed, bg bool) []byte {
	if !changed {
		return nil
	}
	updated := make(map[string]any, len(input)+1)
	for k, v := range input {
		updated[k] = v
	}
	updated["command"] = cmd
	if !bg {
		updated["timeout"] = max(toInt(input["timeout"]), 600000)
	}
	return answer(hookOutput{PermissionDecision: decisionAllow, UpdatedInput: updated})
}

func isHeavy(cmd string) bool {
	for _, m := range heavy.FindAllStringSubmatchIndex(cmd, -1) {
		if cmd[m[2]:m[3]] != "make" || !lightMake.MatchString(cmd[m[1]:]) {
			return true
		}
	}
	return false
}

func answer(o hookOutput) []byte {
	o.HookEventName = "PreToolUse"
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(map[string]hookOutput{"hookSpecificOutput": o})
	return b.Bytes()
}

func toInt(v any) int64 {
	n, ok := v.(json.Number)
	if !ok {
		return 0
	}
	if i, err := n.Int64(); err == nil {
		return i
	}
	f, _ := n.Float64()
	return int64(f)
}

// labTarget is the kind cluster the command would create; m indexes the
// lab match in cmd.
func labTarget(cmd string, m []int, cwd string) string {
	if strings.HasPrefix(cmd[m[2]:m[3]], "kind") {
		if n := kindName.FindStringSubmatch(cmd); n != nil {
			return n[1]
		}
		return "kind"
	}
	d := cwd
	if cds := cdArg.FindAllStringSubmatch(cmd[:m[0]], -1); len(cds) > 0 {
		d = expandHome(strings.Trim(cds[len(cds)-1][1], `'"`))
	}
	if !filepath.IsAbs(d) {
		d = filepath.Join(cwd, d)
	}
	// A directory without agentlab.yaml is a new lab, whatever name it ends up with.
	raw, err := os.ReadFile(filepath.Join(d, "agentlab.yaml")) //nolint:gosec // the lab's own configuration
	if err != nil {
		return "a new lab in " + d
	}
	for line := range strings.SplitSeq(string(raw), "\n") {
		if c := clusterRe.FindStringSubmatch(line); c != nil {
			return c[1]
		}
	}
	return "agentlab"
}

func expandHome(p string) string {
	if p != "~" && !strings.HasPrefix(p, "~/") {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return p
	}
	return home + p[1:]
}

func leaseLines(hs []lease.Holder) string {
	if len(hs) == 0 {
		return "none held"
	}
	var b strings.Builder
	for _, h := range hs {
		who := h.Name
		if who == "" {
			who = h.Holder
		}
		fmt.Fprintf(&b, "  %s: %s since %s — %s\n", h.Env, who, h.Since, h.Purpose)
	}
	return strings.TrimSuffix(b.String(), "\n")
}

// ShellQuote quotes s for a POSIX shell as Python's shlex.quote does.
func ShellQuote(s string) string {
	if s == "" {
		return "''"
	}
	if shellSafe.MatchString(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}
