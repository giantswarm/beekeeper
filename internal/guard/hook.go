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

// start is where a command starts: start of line (indented or not), after
// ; & | ( $( or then/do/else.
const start = `(?:^[ \t]*|[;&|(]\s*|\$\(\s*|\b(?:then|do|else)\s+)`

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
	`|nohup|time(?:\s+-p)?|command|exec|eval|\w+=\S*` +
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
		`beekeeper\s+scan\s+sweep\b`,
	}, "|") + `)`)
	// lightMake: make targets that build nothing (RE2 has no lookahead).
	lightMake = regexp.MustCompile(`^\s+(?:-n\b|--dry-run\b|help\b|version\b|clean\b|fmt\b|print-|list\b)`)
	// owned: a blocking devctl command at a command position, the gate goes
	// before it.
	owned = regexp.MustCompile(`(?m)` + mergePos + `(` + devctlOwned + `)`)
	// anyOwned: one anywhere; gated: the gate ends the text before it.
	anyOwned = regexp.MustCompile(devctlOwned)
	gated    = regexp.MustCompile(`\bgate\s+(?:--(?:wait|limit)\s+\S+\s+)*--\s+$`)
	// shellC: a shell's -c option, or eval, up to the quote opening its
	// command string.
	shellC = regexp.MustCompile(`(?:^|[\s;&|(/])(?:(?:ba|z|da|k)?sh\s+(?:-[a-zA-Z]+\s+)*-[a-zA-Z]*c[a-zA-Z]*|eval)\s+(['"])`)
	lab    = regexp.MustCompile(`(?m)` + pos + `(agentlab\s+up\b|kind\s+create\s+cluster\b)`)
	// labRuntime: a lab's creation or teardown, which takes the container
	// runtime's socket the agent sandbox closes.
	labRuntime = regexp.MustCompile(`(?m)` + pos + `(agentlab\s+(?:--lab[= ]\s*\S+\s+)?(?:up|down)\b|kind\s+(?:create|delete)\s+clusters?\b)`)
	trivial    = regexp.MustCompile(`^\s*\S+(?:\s+\S+)?\s+(?:--version|-V|--help|-h|help)\s*$`)
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
	// vaultCall: a beekeeper secret call on the vault, which may wait for
	// the person's unlock (secret.unlockWait) and gets the 10 minutes.
	vaultCall = regexp.MustCompile(`(?:^|[\s;&|(/])beekeeper["']?\s+secret\s+(?:compare|fingerprint|copy|set|rotate)\b[^;&|\n]*op://`)
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
	// UnlockCommands are the person's own vault unlock helpers
	// (secret.unlockCommands), refused like op signin.
	UnlockCommands []string
	// SecretFiles are the files known to hold secret values the config adds
	// to the built-in SecretFiles (secret.files), globs allowed; no session
	// reads them whole.
	SecretFiles []string
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
	// Holds names the role a message by name addresses: a name the role's
	// holder carries or carried on the roster ("Supervisor run 82" for a
	// holder its desktop has since titled "klaus-lab-14"), or its session
	// id; "" when it holds none. Such a message goes where Role sends it.
	// Nil reads every name as written.
	Holds func(name string) string
	// Peer names the running CLI of a desktop session id, "" when none
	// runs; an error refuses the send. Nil passes every SendMessage.
	Peer func(host string) (string, error)
	// Absent says why a message by name reaches nobody: the name is a
	// roster agent's whose CLI does not run (its headless turn ended, its
	// import waits); "" passes the send. Nil passes every one.
	Absent func(name string) string
	// Yours records a `yours <resource>` in a message from the session
	// holding the supervisor role as the resource's grant to the message's
	// target (to, after the hook's own redirects) and returns what the
	// sender is told: the grant recorded, or why none was; "" says nothing.
	// Nil records nothing.
	Yours func(session, to, message string) string
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
	// Started reports whether beekeeper agents start started the session
	// in bypassPermissions; read only for a browser call in acceptEdits,
	// nil refuses no browser call.
	Started func(session string) bool
	// Apps lists the Apps the person owns and asked a consent for, what a
	// Chrome call on a consent page is decided from (consent.go); read only
	// for a browser call that acts on a page, nil decides no consent page.
	Apps func() []App
	// GraphQL is the GraphQL budget as beekeeper last read it, for a
	// board-read refusal; nil or "", unknown.
	GraphQL func() string
	// StopTask ends the gate a TaskStop of the session's task runs and says
	// what it ended, "" for nothing; nil ends nothing.
	StopTask func(session, task string) string
}

// event is the part of a PreToolUse event the hook reads.
type event struct {
	ToolName  string         `json:"tool_name"`
	ToolInput map[string]any `json:"tool_input"`
	CWD       string         `json:"cwd"`
	Session   string         `json:"session_id"`
	Agent     string         `json:"agent_id"`
	Mode      string         `json:"permission_mode"`
	// TranscriptPath is the session's transcript, where a Chrome call's
	// page is read from.
	TranscriptPath string `json:"transcript_path"`
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
	d.AdditionalContext = strings.TrimSpace(d.AdditionalContext + "\n\n" + reads)
	return answer(d)
}

func (h Hook) decide(ev event) []byte {
	if r := h.guideRefusal(ev); r != "" {
		return answer(hookOutput{PermissionDecision: decisionDeny, Reason: r})
	}
	if r := h.sandboxRefusal(ev); r != "" {
		return answer(hookOutput{PermissionDecision: decisionDeny, Reason: r})
	}
	if r := h.desktopBrowserRefusal(ev); r != "" {
		return answer(hookOutput{PermissionDecision: decisionDeny, Reason: r})
	}
	if out := h.consent(ev); out != nil {
		return out
	}
	if ev.ToolName == AskTool {
		return h.ask(ev.Session, ev.ToolInput)
	}
	if ev.ToolName == SendMessageTool {
		return h.sendMessage(ev.Session, ev.ToolInput)
	}
	if ev.ToolName == TaskStopTool {
		return h.taskStop(ev.Session, ev.ToolInput)
	}
	if r := mentionRefusal(ev.ToolName, ev.ToolInput, ev.CWD); r != "" {
		return answer(hookOutput{PermissionDecision: decisionDeny, Reason: r})
	}
	if r := h.secretFileRefusal(ev); r != "" {
		return answer(hookOutput{PermissionDecision: decisionDeny, Reason: r})
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
	if r := h.boardReadRefusal(cmd); r != "" {
		return answer(hookOutput{PermissionDecision: decisionDeny, Reason: r})
	}
	cwd := ev.CWD
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	if r := h.membersRefusal(cmd, ev.Session, cwd); r != "" {
		return answer(hookOutput{PermissionDecision: decisionDeny, Reason: r})
	}
	if r := h.searchRefusal(cmd, ev.Session); r != "" {
		return answer(hookOutput{PermissionDecision: decisionDeny, Reason: r})
	}
	if l := (secretGuard{unlock: h.UnlockCommands, cwd: cwd, files: newSecretFiles(h.SecretFiles)}).leak(cmd); l != nil {
		return answer(hookOutput{PermissionDecision: decisionDeny, Reason: l.reason()})
	}
	if r := deleteRefusal(cmd); r != "" {
		return answer(hookOutput{PermissionDecision: decisionDeny, Reason: r})
	}
	if r := h.Outbound.bashRefusal(cmd, cwd); r != "" {
		return answer(hookOutput{PermissionDecision: decisionDeny, Reason: r})
	}
	bg, _ := ev.ToolInput[backgroundKey].(bool)
	cmd, gated := h.gate(cmd, bg)
	if fixed, ok := h.hiddenMerges(cmd, bg); ok {
		return answer(hookOutput{PermissionDecision: decisionDeny, Reason: "Refused: a devctl pr merge, release promote, pr wait, release wait or rollout wait " +
			"inside a shell's -c or eval string runs outside the gate. Run it as its own command, or with the gate written in:\n" + fixed})
	}
	if r := h.ungatedRefusal(cmd, cwd); r != "" {
		return answer(hookOutput{PermissionDecision: decisionDeny, Reason: r})
	}
	if wrapped.MatchString(cmd) {
		return h.rewrite(ev.ToolInput, cmd, gated, bg)
	}

	if h.Sandbox != nil {
		if m := labRuntime.FindStringSubmatchIndex(cmd); m != nil {
			var held []lease.Holder
			if h.Leases != nil {
				held = h.Leases()
			}
			return answer(hookOutput{PermissionDecision: decisionDeny, Reason: fmt.Sprintf(
				"Refused: `%s` needs the container runtime, which the agent sandbox closes. The host creates and tears down a lab "+
					"whose lease you hold: `beekeeper lease up <lab>` or `beekeeper lease down <lab>`, run in the lab's directory "+
					"(its agentlab.yaml names the lease's cluster) or anywhere for a lab agentlab knows.\nleases:\n%s",
				strings.TrimSpace(cmd[m[2]:m[3]]), leaseLines(held))})
		}
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
		return h.rewrite(ev.ToolInput, cmd, gated || vaultCall.MatchString(cmd), bg)
	}
	prefix := ""
	if bg {
		prefix = "MEMCAP_WAIT=60m "
	}
	return h.rewrite(ev.ToolInput, prefix+ShellQuote(h.Self)+" run -- "+ShellQuote(h.Shell)+" -c "+ShellQuote(cmd), true, bg)
}

// gate puts "beekeeper gate --" before every blocking devctl command at a
// command position, behind its prefix commands, so that only the devctl
// invocation is wrapped and pipelines and lists run as written. A
// here-document a shell reads as its program is a command line too, gated in
// place; one that is content (a file written, an issue body sent) stays as
// written.
func (h Hook) gate(cmd string, bg bool) (string, bool) {
	content := contentHeredocs(cmd)
	var at []int
	for _, m := range owned.FindAllStringSubmatchIndex(cmd, -1) {
		if !slices.ContainsFunc(content, func(r [2]int) bool { return m[2] >= r[0] && m[2] < r[1] }) {
			at = append(at, m[2])
		}
	}
	return h.insertGate(cmd, at, bg), len(at) > 0
}

// contentHeredocs returns the bodies of the command line's here-documents no
// local shell reads as its program: the command the operator is on is no
// shell, and neither is a stage after it in its pipeline.
func contentHeredocs(cmd string) [][2]int {
	sc := scanShell(cmd)
	segs := sc.segments()
	var out [][2]int
	for _, hd := range sc.heredocs {
		if !shellReads(sc, segs, hd.op) {
			out = append(out, [2]int{hd.start, hd.end})
		}
	}
	return out
}

// shellReads reports whether a local shell reads the here-document whose
// operator is at op as its program.
func shellReads(sc shellScan, segs []segment, op int) bool {
	for i, sg := range segs {
		if op < sg.start || op >= sg.end {
			continue
		}
		for j := i; j < len(segs); j++ {
			if words := shellWords(sc.plain[segs[j].start:segs[j].end]); runsLocalShell(words) {
				return true
			}
			if segs[j].after != "|" {
				break
			}
		}
		return false
	}
	return false
}

// hiddenMerges returns cmd with the gate before each blocking devctl command
// the gate rewrite left inside a sh, bash or zsh -c string or an eval string,
// and whether there was one.
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
// background merge waits up to 30 minutes for its turn (a wait has none) and
// runs without the tool limit, as no Bash call limit stops a background task.
func (h Hook) insertGate(cmd string, at []int, bg bool) string {
	gate := ShellQuote(h.Self) + " gate -- "
	if bg {
		gate = ShellQuote(h.Self) + " gate --wait 30m --limit 0 -- "
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
	if cl, ok := LabCluster(d); ok {
		return cl
	}
	return "a new lab in " + d
}

// LabCluster is the kind cluster of the agentlab lab in dir, its
// agentlab.yaml's clusterName (agentlab's default without one), and
// whether dir holds an agentlab.yaml at all.
func LabCluster(dir string) (string, bool) {
	raw, err := os.ReadFile(filepath.Join(dir, "agentlab.yaml")) //nolint:gosec // the lab's own configuration
	if err != nil {
		return "", false
	}
	for line := range strings.SplitSeq(string(raw), "\n") {
		if c := clusterRe.FindStringSubmatch(line); c != nil {
			return c[1], true
		}
	}
	return "agentlab", true
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
