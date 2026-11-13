package config

import (
	"fmt"
	"net"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Hooks configures where beekeeper's Claude Code hooks act.
type Hooks struct {
	// Guards enables PreToolUse refusals and rewrites. Unset, enabled.
	Guards *bool `yaml:"guards"`
	// GuardPersonalSearch refuses cross-repository issue searches in personal sessions too.
	GuardPersonalSearch bool `yaml:"guardPersonalSearch"`
	// Scope is the desk's part of the machine; out of it the hooks are
	// inert.
	Scope HookScope `yaml:"scope"`
	// TranscriptGrants authorize exact transcript files or fixture directories per session ID.
	// Set by the supervisor, never inferred from a hook event.
	TranscriptGrants map[string][]string `yaml:"transcriptGrants"`
	// PersonFolders are the person's folders (~/ and globs allowed, e.g.
	// [~/, ~/projects, ~/projects/*]): the PreToolUse hook refuses a file a
	// session would create directly in one, and one git neither tracks nor
	// ignores in the main working tree of a checkout that sits directly in
	// one; scratch files go in the session's scratchpad. Empty, off.
	PersonFolders []string `yaml:"personFolders"`
}

// GuardsEnabled preserves enforcement unless explicitly disabled.
func (h Hooks) GuardsEnabled() bool { return h.Guards == nil || *h.Guards }

// HookScope is where the hooks act: in a session beekeeper started or
// knows (an agents start, a role holder, an agent on the roster), and in a
// session whose working directory or project lies under one of Dirs.
type HookScope struct {
	// Dirs are the desk's directories (~/ allowed); empty, every session
	// is in scope.
	Dirs []string `yaml:"dirs"`
}

// Outbound configures the outbound secret guard. The token patterns are
// built in; these add what is specific to the machine.
type Outbound struct {
	// Phrases never leave the machine: outbound text that contains one
	// (case-insensitive) is refused, naming its number, never the phrase.
	Phrases []string `yaml:"phrases"`
	// Paths are globs (~/ allowed) of files whose Write and Edit count as
	// outbound (plans, posts); a directory covers every file under it.
	Paths []string `yaml:"paths"`
	// StoreDeny are the secret-store writes the hook refuses.
	StoreDeny []StoreRule `yaml:"storeDeny"`
	// SweepRoots are the directories (~/ allowed; default: home) the watch
	// sweeps, SweepDepth levels deep (default 5), every SweepEvery (default
	// 15m), for world-readable key files and git remote URLs that carry a
	// credential.
	SweepRoots []string `yaml:"sweepRoots"`
	SweepDepth int      `yaml:"sweepDepth"`
	SweepEvery Duration `yaml:"sweepEvery"`
}

// Sandbox is the agent sandbox: the paths and hosts an agent session's
// commands and file tools reach besides beekeeper's own. The home directory
// is denied, so a path nobody lists stays unreadable.
type Sandbox struct {
	// AllowRead are the paths (~/ allowed) under the home directory
	// commands and file tools may read: the checkouts, toolchains and lab
	// kubeconfigs sessions need.
	AllowRead []string `yaml:"allowRead"`
	// AllowWrite are the paths (~/ allowed) they may write, readable too:
	// the checkouts and worktrees, build and module caches.
	AllowWrite []string `yaml:"allowWrite"`
	// Domains are the hosts commands reach besides GitHub's: muster, the
	// registries and module proxies builds need, the labs' API servers.
	Domains []string `yaml:"domains"`
	// ProxyPort is the port of the broker's egress proxy on the host's
	// loopback (default 3190): the sandbox's only way out, which holds it
	// to the domains and sets the GitHub token's header itself.
	ProxyPort int `yaml:"proxyPort"`
	// Devctl is the devctl binary the broker runs on the host (~/ allowed;
	// default: devctl on the broker's PATH): it renews the egress proxy's
	// GitHub token from devctl's App login and runs a sandboxed session's
	// gated devctl commands, which read the keychain the sandbox closes.
	Devctl string `yaml:"devctl"`
	// Required refuses every tool call of a Claude Code session the policy
	// does not hold (no BEEKEEPER_SANDBOX: the policy is not installed, or
	// the session started before it was), SendMessage aside, and tells the
	// session so at its start. Off by default: set it once the policy is
	// installed and sessions work under it.
	Required bool `yaml:"required"`
}

func (s *Sandbox) validateEach(report func(error)) {
	for key, ps := range map[string][]string{"sandbox.allowRead": s.AllowRead, "sandbox.allowWrite": s.AllowWrite} {
		for _, p := range ps {
			if !filepath.IsAbs(p) {
				report(fmt.Errorf("%s: %q is neither absolute nor under ~/", key, p))
			}
		}
	}
	if s.ProxyPort < 0 || s.ProxyPort > 65535 {
		report(fmt.Errorf("sandbox.proxyPort: %d is no TCP port", s.ProxyPort))
	}
	for _, d := range s.Domains {
		if openLoopback(d) {
			report(fmt.Errorf("sandbox.domains: %q opens every loopback listener on the host (other sessions' port-forwards, local servers) to the sandbox: name a lab's API server by its port, 127.0.0.1:<port>", d))
		}
		if host, port, err := net.SplitHostPort(d); err == nil && Loopback(host) && port == strconv.Itoa(s.ProxyPort) {
			report(fmt.Errorf("sandbox.domains: %q is the egress proxy itself (sandbox.proxyPort)", d))
		}
	}
}

// openLoopback reports whether the egress entry d opens loopback beyond one
// port: a bare loopback host or a wildcard port would open every listener on
// the host through the sandbox proxy.
func openLoopback(d string) bool {
	if d == "*" {
		return true
	}
	host, port, err := net.SplitHostPort(d)
	if err != nil {
		host, port = strings.Trim(d, "[]"), ""
	}
	return Loopback(strings.TrimPrefix(host, "*.")) && (port == "" || port == "*")
}

// Loopback reports whether host is a loopback name or address.
func Loopback(host string) bool {
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	return host == "localhost" || strings.HasSuffix(host, ".localhost")
}

func (s *Sandbox) defaults(home string) {
	for _, ps := range [][]string{s.AllowRead, s.AllowWrite} {
		for i := range ps {
			ps[i] = filepath.Clean(homePath(home, ps[i]))
		}
	}
	if s.ProxyPort == 0 {
		s.ProxyPort = 3190
	}
	if s.Devctl == "" {
		s.Devctl = "devctl"
	}
	s.Devctl = homePath(home, s.Devctl)
}

func (o *Outbound) defaults(home string) {
	if len(o.SweepRoots) == 0 {
		o.SweepRoots = []string{home}
	}
	setInt(&o.SweepDepth, 5)
	setDur(&o.SweepEvery, 15*time.Minute)
	for _, ps := range [][]string{o.Paths, o.SweepRoots} {
		for i := range ps {
			ps[i] = homePath(home, ps[i])
		}
	}
}
