//go:build scenario && linux

package harness

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"gopkg.in/yaml.v3"
)

// An execution host of a scenario is a root of its own in the sandbox:
//
//	/work/hosts/<name>/home        its account's home
//	/work/hosts/<name>/bin         beekeeper, git and the harnesses
//	/work/hosts/<name>/tools       the pinned worker toolchain (go, gh)
//	/work/hosts/<name>/src         the source checkouts it serves
//	/work/hosts/<name>/workspaces  its executions' workspaces
//	/work/hosts/<name>/config.yaml its own configuration (agents.execution)
//
// The fake ssh runs a remote command there, under the host's environment,
// and the real beekeeper of the host answers it. The host calls its
// coordinator back over ssh too, under CoordinatorAlias.

// CoordinatorAlias is the ssh destination a host reaches the desk under.
const CoordinatorAlias = "coordinator"

// The worker toolchain a host pins: OldGo is what a host with the
// older-go fault pins, below the NewGo its repository's go.mod names.
const (
	NewGo     = "1.26.0"
	OldGo     = "1.21.0"
	GhVersion = "2.80.0"
)

// HostRoot is the root of a host.
func HostRoot(name string) string { return filepath.Join(HostsRoot, name) }

// HostEnv is the environment of a host's account, from the desk's: its own
// home and PATH, and no configuration of the desk's.
func HostEnv(desk []string, name string) []string {
	root := HostRoot(name)
	home := filepath.Join(root, "home")
	out := slices.DeleteFunc(slices.Clone(desk), func(kv string) bool {
		k, _, _ := strings.Cut(kv, "=")
		return slices.Contains([]string{"PATH", "HOME", "XDG_CONFIG_HOME", "XDG_STATE_HOME", "XDG_CACHE_HOME", "BEEKEEPER_CONFIG"}, k)
	})
	return append(out,
		"PATH="+filepath.Join(root, "bin"),
		"HOME="+home,
		"XDG_CONFIG_HOME="+filepath.Join(home, ".config"),
		"XDG_STATE_HOME="+filepath.Join(home, ".local", "state"),
		"XDG_CACHE_HOME="+filepath.Join(home, ".cache"),
	)
}

// The fake ssh: it maps the destination to its root in the scenario and
// runs the remote command there. A host's fault ends the call as OpenSSH
// ends it.
func ssh(c *client) int {
	args := c.args
	connect := 10 * time.Second
	i := 0
	for ; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			i++
			break
		}
		if !strings.HasPrefix(a, "-") {
			break
		}
		if slices.Contains([]string{"-o", "-p", "-i", "-l", "-F", "-J", "-L", "-R", "-D", "-E", "-S", "-W", "-b", "-c", "-e", "-m", "-w"}, a) && i+1 < len(args) {
			i++
			if v, ok := strings.CutPrefix(args[i], "ConnectTimeout="); ok && a == "-o" {
				if n, err := strconv.Atoi(v); err == nil && n > 0 {
					connect = time.Duration(n) * time.Second
				}
			}
		}
	}
	if i >= len(args) {
		fmt.Fprintln(os.Stderr, "usage: ssh destination [command]")
		return 255
	}
	dest := args[i]
	command := strings.Join(args[i+1:], " ")
	rep, err := c.ask(request{Op: opSSH, Dest: dest})
	if err != nil {
		return c.end(rep, err)
	}
	r := rep.SSH
	if r == nil {
		fmt.Fprintln(os.Stderr, rep.Message)
		return 255
	}
	if slow := time.Duration(r.Slow) * time.Millisecond; slow > 0 {
		if slow >= connect {
			time.Sleep(connect)
			fmt.Fprintf(os.Stderr, "ssh: connect to host %s port 22: Connection timed out\n", dest)
			return 255
		}
		time.Sleep(slow)
	}
	switch r.Fault {
	case Unreachable:
		fmt.Fprintf(os.Stderr, "ssh: connect to host %s port 22: Connection refused\n", dest)
		return 255
	case HostKeyChanged:
		fmt.Fprint(os.Stderr, "@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@\n"+
			"@    WARNING: REMOTE HOST IDENTIFICATION HAS CHANGED!     @\n"+
			"@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@\n"+
			"IT IS POSSIBLE THAT SOMEONE IS DOING SOMETHING NASTY!\n"+
			"Host key for "+dest+" has changed and you have requested strict checking.\n"+
			"Host key verification failed.\n")
		return 255
	case MissingTool:
		// The remote shell does not find the command's executable.
		_, _ = io.Copy(io.Discard, io.LimitReader(os.Stdin, 16<<20))
		word, _, _ := strings.Cut(strings.TrimSpace(command), " ")
		fmt.Fprintf(os.Stderr, "bash: line 1: %s: No such file or directory\n", strings.Trim(word, "'"))
		return 127
	}
	if strings.TrimSpace(command) == "" {
		fmt.Fprintln(os.Stderr, "ssh: the scenario's hosts run commands, no login shell")
		return 255
	}
	if err := os.Chdir(r.Home); err != nil {
		fmt.Fprintf(os.Stderr, "ssh: %v\n", err)
		return 255
	}
	if r.Fault == DropsSource && strings.Contains(command, hostRPCCommand) {
		return dropSource(dest, command, r.Env)
	}
	if err := syscall.Exec("/bin/sh", []string{"sh", "-c", command}, r.Env); err != nil {
		fmt.Fprintf(os.Stderr, "ssh: %v\n", err)
	}
	return 255
}

// A host's RPC: the command the coordinator calls a host with, its request
// on standard input, and the operation a start uploads its source bundle
// with.
const (
	hostRPCCommand        = "host-rpc"
	sourceUploadOperation = "source-chunk"
)

// dropSource runs a host-rpc call on a host with the drops-source fault:
// the request is read first, and a source upload's connection is dropped
// as OpenSSH reports a closed one; every other call runs as on a healthy
// host.
func dropSource(dest, command string, env []string) int {
	in, err := io.ReadAll(io.LimitReader(os.Stdin, 16<<20))
	if err != nil {
		fmt.Fprintf(os.Stderr, "ssh: %v\n", err)
		return 255
	}
	var req struct {
		Operation string `json:"operation"`
	}
	if json.Unmarshal(in, &req) == nil && req.Operation == sourceUploadOperation {
		fmt.Fprintf(os.Stderr, "Connection to %s closed by remote host.\n", dest)
		return 255
	}
	c := exec.Command("/bin/sh", "-c", command) //nolint:gosec // the scenario's own remote command, as ssh runs it
	c.Env = env
	c.Stdin, c.Stdout, c.Stderr = bytes.NewReader(in), os.Stdout, os.Stderr
	if err := c.Run(); err != nil {
		if exit, ok := errors.AsType[*exec.ExitError](err); ok {
			return exit.ExitCode()
		}
		fmt.Fprintf(os.Stderr, "ssh: %v\n", err)
		return 255
	}
	return 0
}

// Desk is what provisioning the hosts needs of the scenario's desk.
type Desk struct {
	// Candidate is the desk's beekeeper, Fake the fake executable, Git
	// the real git, Config the desk's configuration file.
	Candidate, Fake, Git, Config string
	// Identity is the desk's identity.host; Orgs its github.allowedOrgs.
	Identity string
	Orgs     []string
	// Env is the desk's environment.
	Env []string
}

// Provision builds every host's root as an operator would provision the
// host, prepares its policy with the real beekeeper and returns the desk's
// agents.hosts entries, each pinned to the digest its host prepared.
func Provision(ctx context.Context, desk Desk, hosts []Host, repositories []string) (map[string]any, error) {
	if len(hosts) == 0 {
		return nil, nil
	}
	key, err := run(ctx, desk.Env, "", desk.Candidate, "--config", desk.Config, "agents", "hosts", "key")
	if err != nil {
		return nil, fmt.Errorf("the desk's coordinator key: %w", err)
	}
	entries := map[string]any{}
	for _, h := range hosts {
		entry, err := provision(ctx, desk, h, repositories, strings.TrimSpace(key))
		if err != nil {
			return nil, fmt.Errorf("host %s: %w", h.Name, err)
		}
		entries[h.Name] = entry
	}
	return entries, nil
}

func provision(ctx context.Context, desk Desk, h Host, repositories []string, key string) (map[string]any, error) {
	root := HostRoot(h.Name)
	env := HostEnv(desk.Env, h.Name)
	goVersion := NewGo
	if h.Fault == OlderGo {
		goVersion = OldGo
	}
	goBin := filepath.Join(root, "tools", "go"+goVersion, "bin")
	ghBin := filepath.Join(root, "tools", "gh", "bin")
	for _, dir := range []string{"home/.config", "home/.local/state", "home/.cache", "home/.claude", "home/.codex", "home/.gemini", "home/.omp", "bin", "state", "leases", "workspaces", "src"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o700); err != nil {
			return nil, err
		}
	}
	for _, dir := range []string{goBin, ghBin} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, err
		}
	}
	if err := linkOrCopy(desk.Candidate, filepath.Join(root, "bin", "beekeeper")); err != nil {
		return nil, err
	}
	links := map[string]string{
		filepath.Join(root, "bin", "git"): desk.Fake,
		filepath.Join(root, "bin", "sh"):  "/bin/sh",
		filepath.Join(goBin, "go"):        desk.Fake,
		filepath.Join(ghBin, "gh"):        desk.Fake,
	}
	for _, name := range append(slices.Clone(Programs), "gh", "devctl", "journalctl") {
		links[filepath.Join(root, "bin", name)] = desk.Fake
	}
	for link, target := range links {
		if err := os.Symlink(target, link); err != nil {
			return nil, err
		}
	}
	for file, version := range map[string]string{filepath.Join(filepath.Dir(goBin), "VERSION"): goVersion, filepath.Join(filepath.Dir(ghBin), "VERSION"): GhVersion} {
		if err := os.WriteFile(file, []byte(version+"\n"), 0o600); err != nil {
			return nil, err
		}
	}
	rules := filepath.Join(root, "rules.md")
	if err := os.WriteFile(rules, []byte("Work in the workspace this execution owns and publish through the coordinator.\n"), 0o600); err != nil {
		return nil, err
	}
	served := h.Repositories
	if len(served) == 0 {
		served = repositories
	}
	sources := map[string]any{}
	for _, repo := range served {
		src := filepath.Join(root, "src", repo)
		if err := os.MkdirAll(filepath.Dir(src), 0o700); err != nil {
			return nil, err
		}
		// A host's checkout names its repository as GitHub does; the
		// scenario's git leads that URL to the origin.
		if _, err := run(ctx, env, "", desk.Git, "clone", "--quiet", "https://github.com/"+repo+".git", src); err != nil {
			return nil, err
		}
		if h.Fault == OlderGo {
			// The repository names a newer Go than the host pins.
			if err := os.WriteFile(filepath.Join(src, "go.mod"), []byte("module example.invalid/"+filepath.Base(repo)+"\n\ngo "+NewGo+"\n"), 0o600); err != nil {
				return nil, err
			}
			for _, args := range [][]string{{"-C", src, "add", "go.mod"}, {"-C", src, "commit", "--quiet", "-m", "name the Go version"}} {
				if _, err := run(ctx, env, "", desk.Git, args...); err != nil {
					return nil, err
				}
			}
		}
		sources[repo] = src
	}
	harnesses := h.Harnesses
	if len(harnesses) == 0 {
		harnesses = []string{Claude}
	}
	pinned := map[string]any{}
	for _, name := range harnesses {
		hc := map[string]any{"executable": filepath.Join(root, "bin", name)}
		switch name {
		case Claude:
			hc["version"] = ClaudeVersion
		case Codex:
			hc["version"], hc["configDir"] = CodexVersion, filepath.Join(root, "home", ".codex")
			if err := os.WriteFile(filepath.Join(root, "home", ".codex", "auth.json"), []byte("{}\n"), 0o600); err != nil {
				return nil, err
			}
		case Agy:
			hc["version"], hc["configDir"] = AgyVersion, filepath.Join(root, "home", ".gemini")
		case Omp:
			hc["version"], hc["configDir"] = OmpVersion, filepath.Join(root, "home", ".omp")
		}
		pinned[name] = hc
	}
	maxBusy := h.MaxBusy
	if maxBusy == 0 {
		maxBusy = 2
	}
	workspaces := filepath.Join(root, "workspaces")
	config := filepath.Join(root, "config.yaml")
	cfg := map[string]any{
		"stateDir": filepath.Join(root, "state"),
		"leaseDir": filepath.Join(root, "leases"),
		"identity": map[string]any{"person": "scenario", "team": "scenario", "host": h.Name},
		"github":   map[string]any{"floor": -1, "allowedOrgs": toAny(desk.Orgs)},
		"capacity": map[string]any{"ceiling": maxBusy, "reviewHeadroom": 0, "availMinMiB": 1},
		"watch":    map[string]any{"availMinMiB": 1, "loadMax": 1000000, "cpuPSIMax": 100, "psiMax": 100},
		"agents": map[string]any{
			"staleAfter": "-1s",
			"execution": map[string]any{
				"identity":      h.Name,
				"rulesFile":     rules,
				"workspaceRoot": workspaces,
				"repositories":  sources,
				"harnesses":     pinned,
				"maxBusy":       maxBusy,
				"coordinators": map[string]any{desk.Identity: map[string]any{
					"publicKey": key,
					"callback":  map[string]any{"ssh": CoordinatorAlias, "executable": desk.Candidate, "config": desk.Config},
				}},
				"tools": map[string]any{
					"go": map[string]any{"executable": filepath.Join(goBin, "go"), "version": goVersion},
					"gh": map[string]any{"executable": filepath.Join(ghBin, "gh"), "version": GhVersion},
				},
			},
		},
	}
	overlay(cfg, h.Config)
	raw, err := yaml.Marshal(cfg)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(config, raw, 0o600); err != nil {
		return nil, err
	}
	digest, err := prepare(ctx, env, root)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"ssh": h.Name, "executable": filepath.Join(root, "bin", "beekeeper"), "config": config, "identity": h.Name,
		"workspaceRoot": workspaces, "repositories": sources, "harnesses": toAny(harnesses),
		"policyDigest": digest, "maxBusy": maxBusy,
	}, nil
}

// Reconfigure changes the policy of the provisioned host name as an
// operator does: over is laid over the host's own configuration, and the
// host's beekeeper prepares its policy again. It returns the digest prepare
// printed, which the operator names in the desk's agents.hosts entry.
func Reconfigure(ctx context.Context, desk Desk, name string, over map[string]any) (string, error) {
	root := HostRoot(name)
	config := filepath.Join(root, "config.yaml")
	raw, err := os.ReadFile(config) //nolint:gosec // the host's own configuration in the sandbox
	if err != nil {
		return "", err
	}
	var cfg map[string]any
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return "", fmt.Errorf("host %s configuration: %w", name, err)
	}
	overlay(cfg, over)
	if raw, err = yaml.Marshal(cfg); err != nil {
		return "", err
	}
	if err := os.WriteFile(config, raw, 0o600); err != nil {
		return "", err
	}
	return prepare(ctx, HostEnv(desk.Env, name), root)
}

// prepare exports the policy bundle of the host at root and installs it
// with the host's beekeeper, as an operator does, and returns the digest
// prepare printed.
func prepare(ctx context.Context, env []string, root string) (string, error) {
	beekeeper, config, home := filepath.Join(root, "bin", "beekeeper"), filepath.Join(root, "config.yaml"), filepath.Join(root, "home")
	bundle, err := run(ctx, env, home, beekeeper, "--config", config, "agents", "hosts", "bundle")
	if err != nil {
		return "", fmt.Errorf("hosts bundle: %w", err)
	}
	bundleFile := filepath.Join(root, "policy.json")
	if err := os.WriteFile(bundleFile, []byte(bundle), 0o600); err != nil {
		return "", err
	}
	digest, err := run(ctx, env, home, beekeeper, "--config", config, "agents", "hosts", "prepare", "--bundle", bundleFile)
	if err != nil {
		return "", fmt.Errorf("hosts prepare: %w", err)
	}
	return strings.TrimSpace(digest), nil
}

func toAny(xs []string) []any {
	out := make([]any, len(xs))
	for i, x := range xs {
		out[i] = x
	}
	return out
}

// overlay lays src over dst, map by map; any other value replaces.
func overlay(dst, src map[string]any) {
	for k, v := range src {
		if sm, ok := v.(map[string]any); ok {
			if dm, ok := dst[k].(map[string]any); ok {
				overlay(dm, sm)
				continue
			}
		}
		dst[k] = v
	}
}

// run runs a command of the provisioning and returns its output.
func run(ctx context.Context, env []string, dir, name string, args ...string) (string, error) {
	c := exec.CommandContext(ctx, name, args...) //nolint:gosec // the scenario's own provisioning
	c.Env, c.Dir = env, dir
	var out, errOut bytes.Buffer
	c.Stdout, c.Stderr = &out, &errOut
	if err := c.Run(); err != nil {
		return out.String(), fmt.Errorf("%s %s: %w: %s%s", filepath.Base(name), strings.Join(args, " "), err, errOut.String(), out.String())
	}
	return out.String(), nil
}

// linkOrCopy hard-links src to dst, or copies it executable.
func linkOrCopy(src, dst string) error {
	if err := os.Link(src, dst); err == nil {
		return nil
	}
	in, err := os.Open(src) //nolint:gosec // the scenario's candidate
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o700) //nolint:gosec // the host's own bin directory
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return errors.Join(out.Close())
}
