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

// pos is a command position: start of line, after ; & | ( $( or
// then/do/else, with optional wrappers (timeout 600, time, nice, env,
// VAR=val, command).
const pos = `(?:^|[;&|(]\s*|\$\(\s*|\b(?:then|do|else)\s+)(?:(?:timeout\s+\S+|time|nice(?:\s+-n\s*\d+)?|env(?:\s+\w+=\S*)*|command|\w+=\S*)\s+)*`

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
	lab       = regexp.MustCompile(`(?m)` + pos + `(agentlab\s+up\b|kind\s+create\s+cluster\b)`)
	trivial   = regexp.MustCompile(`^\s*\S+(?:\s+\S+)?\s+(?:--version|-V|--help|-h|help)\s*$`)
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
	if strings.TrimSpace(cmd) == "" || trivial.MatchString(cmd) || wrapped.MatchString(cmd) {
		return nil
	}
	cwd := ev.CWD
	if cwd == "" {
		cwd, _ = os.Getwd()
	}

	if m := lab.FindStringSubmatchIndex(cmd); m != nil {
		running := h.Clusters()
		target := labTarget(cmd, m, cwd)
		if !slices.Contains(running, target) && len(running) >= MaxLabs {
			return answer(hookOutput{PermissionDecision: "deny", Reason: fmt.Sprintf(
				"Refused: %d kind labs already run (%s) and this machine allows at most %d. `%s` would create a third (%s). "+
					"Reuse a running lab — claim it with `beekeeper lease claim` — or wait until one is torn down; do not poll for it.\nleases:\n%s",
				len(running), strings.Join(running, ", "), MaxLabs, strings.TrimSpace(cmd[m[2]:m[3]]), target, leaseLines(h.Leases()))})
		}
	}

	if !isHeavy(cmd) {
		return nil
	}
	bg, _ := ev.ToolInput["run_in_background"].(bool)
	updated := make(map[string]any, len(ev.ToolInput)+1)
	for k, v := range ev.ToolInput {
		updated[k] = v
	}
	prefix := ""
	if bg {
		prefix = "MEMCAP_WAIT=60m "
	}
	updated["command"] = prefix + ShellQuote(h.Self) + " run -- zsh -c " + ShellQuote(cmd)
	if !bg {
		updated["timeout"] = max(toInt(ev.ToolInput["timeout"]), 600000)
	}
	return answer(hookOutput{PermissionDecision: "allow", UpdatedInput: updated})
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
