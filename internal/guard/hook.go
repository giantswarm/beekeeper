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
	"github.com/giantswarm/beekeeper/internal/sandbox"
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

// devctlOwned is one of devctl's commands the gate runs (pr merge, release
// promote, pr wait, release wait, rollout wait), devctl by name or by a path
// that starts with /, ~/, ./, ../ or a variable ($HOME/bin/devctl).
const devctlOwned = `(?:(?:~|\.\.?|\$\{?\w+\}?)?/(?:[^\s;&|()'"<>=]*/)?)?devctl\s+(?:pr\s+(?:merge|wait)|release\s+(?:wait|promote)|rollout\s+wait)\b`

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
	// owned: a blocking devctl command at a command position, the gate goes
	// before it.
	owned = regexp.MustCompile(`(?m)` + mergePos + `(` + devctlOwned + `)`)
	// anyOwned: one anywhere; gated: the gate ends the text before it.
	anyOwned = regexp.MustCompile(devctlOwned)
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

// Hook decides one PreToolUse event.
type Hook struct {
	// Self is the absolute path of the beekeeper binary a rewrite names.
	Self string
	// ConfigErr is why the configuration did not load: every Bash call is
	// refused with it, since the guards it configures cannot run.
	ConfigErr error
	// Shell is the shell a rewritten command runs in (sh -c semantics).
	Shell string
	// MaxLabs is how many kind clusters the machine runs at most; read
	// only for a command that creates one.
	MaxLabs func() int
	// Production is the installation the kube guard protects; empty, the
	// guard is off.
	Production string
	// ContextHint is how a refusal writes an installation's context
	// ("login.example.com-<installation>"); empty, "<context>".
	ContextHint string
	// Clusters lists the running kind clusters.
	Clusters func() []string
	// Leases lists the held leases.
	Leases func() []lease.Holder
	// Guide reports whether the session is the guide's, and the person the
	// guide asks; read only for an AskUserQuestion call and a call that
	// does work (guide.go).
	Guide func(session string) (bool, string)
	// CheckQuestion names what one of the guide's questions lacks for its
	// person to answer it, as `note add` checks a note; nil checks nothing.
	CheckQuestion func(Question) []string
	// Role names the session a message to a role (RoleOf) goes to: the
	// name its running CLI takes messages under, else its desktop session
	// id; an error (no holder, an ambiguous name) refuses the send. Nil
	// leaves a role's name as written.
	Role func(role string) (string, error)
	// Peer names the running CLI of a desktop session id, "" when none
	// runs; an error refuses the send. Nil passes every SendMessage.
	Peer func(host string) (string, error)
	// Absent says why a message by name reaches nobody: the name is a
	// roster agent's whose CLI does not run (its headless turn ended, its
	// import waits); "" passes the send. Nil passes every one.
	Absent func(name string) string
	// Project is the session's own project ($CLAUDE_PROJECT_DIR), whose
	// instructions Claude Code loads itself; "" takes the call's cwd.
	Project string
	// Reads reports whether a call is the session's first write in the
	// repository, and records it; nil adds no repository reads.
	Reads func(session, repo string) bool
	// Kubeconfig is the kubeconfig list the session's commands use by
	// default ($KUBECONFIG, else MachineKubeconfig).
	Kubeconfig string
	// MachineKubeconfig is the machine kubeconfig (~/.kube/config), which
	// keeps no current context.
	MachineKubeconfig string
	// ModelServer reads the host's model server, whose loads need its
	// lease; called only for a command that may load a model, nil guards
	// nothing.
	ModelServer func() ModelServer
	// Outbound is the outbound secret guard's configuration; its token
	// patterns apply without one.
	Outbound Outbound
	// Sandbox is the agent sandbox's policy, which the file tools are held
	// to; nil in a session no sandbox holds.
	Sandbox *sandbox.Policy
	// Labs lists the held leases of kind labs, unnamed (a lease file read,
	// no process scan); nil, none.
	Labs func() []lease.Holder
}

// event is the part of a PreToolUse event the hook reads.
type event struct {
	ToolName  string         `json:"tool_name"`
	ToolInput map[string]any `json:"tool_input"`
	CWD       string         `json:"cwd"`
	Session   string         `json:"session_id"`
	Agent     string         `json:"agent_id"`
}

type hookOutput struct {
	HookEventName      string         `json:"hookEventName"`
	PermissionDecision string         `json:"permissionDecision,omitempty"`
	Reason             string         `json:"permissionDecisionReason,omitempty"`
	UpdatedInput       map[string]any `json:"updatedInput,omitempty"`
	AdditionalContext  string         `json:"additionalContext,omitempty"`
}

// Decide returns the hook's JSON answer for the event, nil to let the call
// pass unchanged. Malformed input passes. A call the hook does not refuse
// that is the session's first write in another repository carries that
// repository's instructions as additional context.
func (h Hook) Decide(input []byte) []byte {
	var ev event
	dec := json.NewDecoder(bytes.NewReader(input))
	dec.UseNumber()
	if dec.Decode(&ev) != nil {
		return nil
	}
	out := h.labProxy(ev, h.decide(ev))
	var o map[string]hookOutput
	if out != nil && (json.Unmarshal(out, &o) != nil || o["hookSpecificOutput"].PermissionDecision == decisionDeny) {
		return out
	}
	reads := h.repoReads(ev)
	if reads == "" {
		return out
	}
	d := o["hookSpecificOutput"]
	d.AdditionalContext = reads
	return answer(d)
}

func (h Hook) decide(ev event) []byte {
	if r := h.guideRefusal(ev); r != "" {
		return answer(hookOutput{PermissionDecision: decisionDeny, Reason: r})
	}
	if r := h.sandboxRefusal(ev); r != "" {
		return answer(hookOutput{PermissionDecision: decisionDeny, Reason: r})
	}
	if ev.ToolName == AskTool {
		return h.ask(ev.Session, ev.ToolInput)
	}
	if ev.ToolName == SendMessageTool {
		return h.sendMessage(ev.ToolInput)
	}
	if ev.ToolName != bashTool {
		if r := h.Outbound.toolRefusal(ev.ToolName, ev.ToolInput); r != "" {
			return answer(hookOutput{PermissionDecision: decisionDeny, Reason: r})
		}
		return nil
	}
	cmd, _ := ev.ToolInput["command"].(string)
	if strings.TrimSpace(cmd) == "" || trivial.MatchString(cmd) {
		return nil
	}
	if h.ConfigErr != nil {
		return answer(hookOutput{PermissionDecision: decisionDeny, Reason: "Refused: beekeeper's configuration does not load, so its guards cannot run: " +
			h.ConfigErr.Error() + ". Fix the file (the Edit tool still works), then rerun."})
	}
	if r := h.kubeRefusal(cmd); r != "" {
		return answer(hookOutput{PermissionDecision: decisionDeny, Reason: r})
	}
	if r := h.browserRefusal(cmd, ev.Session); r != "" {
		return answer(hookOutput{PermissionDecision: decisionDeny, Reason: r})
	}
	if r := h.modelServerRefusal(cmd, ev.Session); r != "" {
		return answer(hookOutput{PermissionDecision: decisionDeny, Reason: r})
	}
	if l := secretLeak(cmd); l != nil {
		return answer(hookOutput{PermissionDecision: decisionDeny, Reason: l.reason()})
	}
	if r := deleteRefusal(cmd); r != "" {
		return answer(hookOutput{PermissionDecision: decisionDeny, Reason: r})
	}
	cwd := ev.CWD
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	if r := h.Outbound.bashRefusal(cmd, cwd); r != "" {
		return answer(hookOutput{PermissionDecision: decisionDeny, Reason: r})
	}
	bg, _ := ev.ToolInput[backgroundKey].(bool)
	cmd, gated := h.gate(cmd, bg)
	if fixed, ok := h.hiddenMerges(cmd, bg); ok {
		return answer(hookOutput{PermissionDecision: decisionDeny, Reason: "Refused: a devctl pr merge, release promote, pr wait, release wait or rollout wait " +
			"inside a shell's -c string runs outside the gate. Run it as its own command, or with the gate written in:\n" + fixed})
	}
	if wrapped.MatchString(cmd) {
		return h.rewrite(ev.ToolInput, cmd, gated, bg)
	}

	if m := lab.FindStringSubmatchIndex(cmd); m != nil {
		running := h.Clusters()
		target := labTarget(cmd, m, cwd)
		if limit := h.MaxLabs(); !slices.Contains(running, target) && len(running) >= limit {
			return answer(hookOutput{PermissionDecision: decisionDeny, Reason: fmt.Sprintf(
				"Refused: %d kind labs already run (%s) and this machine allows at most %d. `%s` would create another (%s). "+
					"Reuse a running lab — claim it with `beekeeper lease claim` — or wait until one is torn down; do not poll for it.\nleases:\n%s",
				len(running), strings.Join(running, ", "), limit, strings.TrimSpace(cmd[m[2]:m[3]]), target, leaseLines(h.Leases()))})
		}
	}

	if !isHeavy(cmd) {
		return h.rewrite(ev.ToolInput, cmd, gated, bg)
	}
	prefix := ""
	if bg {
		prefix = "MEMCAP_WAIT=60m "
	}
	return h.rewrite(ev.ToolInput, prefix+ShellQuote(h.Self)+" run -- "+ShellQuote(h.Shell)+" -c "+ShellQuote(cmd), true, bg)
}

// gate puts "beekeeper gate --" before every blocking devctl command at a
// command position, behind its prefix commands, so that only the devctl
// invocation is wrapped and pipelines and lists run as written.
func (h Hook) gate(cmd string, bg bool) (string, bool) {
	var at []int
	for _, m := range owned.FindAllStringSubmatchIndex(cmd, -1) {
		at = append(at, m[2])
	}
	return h.insertGate(cmd, at, bg), len(at) > 0
}

// hiddenMerges returns cmd with the gate before each blocking devctl command
// the gate rewrite left inside a sh, bash or zsh -c string, and whether there
// was one.
// The hook refuses such a command rather than rewriting a quoted string.
func (h Hook) hiddenMerges(cmd string, bg bool) (string, bool) {
	var at []int
	for _, m := range shellC.FindAllStringSubmatchIndex(cmd, -1) {
		body := cmd[m[1]:]
		end := closingQuote(body, cmd[m[2]])
		for _, mm := range anyOwned.FindAllStringIndex(body[:end], -1) {
			if !gated.MatchString(body[:mm[0]]) {
				at = append(at, m[1]+mm[0])
			}
		}
	}
	return h.insertGate(cmd, at, bg), len(at) > 0
}

// insertGate puts the gate before each offset in at, in ascending order; a
// background merge waits up to 30 minutes for its turn (a wait has none).
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

// Owned reports whether cmd runs one of the devctl commands the gate runs
// outside its caller, whose outcome reaches its owner unheard or not.
func Owned(cmd string) bool { return anyOwned.MatchString(cmd) }

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
