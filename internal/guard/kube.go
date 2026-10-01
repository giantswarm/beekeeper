package guard

import (
	"cmp"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// The kube guard refuses a Bash command that switches a kubeconfig's
// current context or writes to a production cluster.
// The machine kubeconfig keeps no current context, so that a command
// without an explicit target fails instead of reaching production; a
// command that would set one is refused. Reads stay allowed everywhere.
// False positives beat a production write. Without a production
// installation (Hook.Production) it is off.

const (
	envKubeconfig  = "KUBECONFIG"
	envHelmContext = "HELM_KUBECONTEXT"
	flagDryRun     = "--dry-run"
	flagKubeconfig = "--kubeconfig"
	verbLogin      = "login"
)

var (
	nameParts = regexp.MustCompile(`[-._@/:]`)
	// contextFlag: a context given anywhere in a command, for a kubectl
	// run through a function or a variable that carries it.
	contextFlag = regexp.MustCompile(`--(?:kube-)?context[= ]\s*["']?([^\s"';&|)]+)`)
	envWord     = regexp.MustCompile(`^(\w+)=(.*)$`)
	// kubectl verbs that change the cluster; rollout and certificate only
	// with the sub-verbs in writeSub.
	kubectlWrites = setOf("apply", "create", "patch", "edit", "delete", "replace", "scale", "annotate", "label",
		"cordon", "uncordon", "drain", "taint", "set", "expose", "autoscale", "run", "exec", "cp", "attach", "debug")
	writeSub = map[string]map[string]bool{
		"rollout":     setOf("restart", "undo", "pause", "resume"),
		"certificate": setOf("approve", "deny"),
		"auth":        setOf("reconcile"),
	}
	// kubectlBuiltins: kubectl's own commands; any other first word runs a
	// plugin, kubectl-<word>.
	kubectlBuiltins = setOf("get", "describe", "logs", "top", "explain", "api-resources", "api-versions", "cluster-info",
		"version", "config", "auth", "diff", "wait", "port-forward", "proxy", "completion", "kustomize", "plugin", "events",
		"alpha", "certificate", "rollout", "options", "help", "convert")
	// pluginReads: plugin subcommands that only read; readOnlyPlugins:
	// plugins that never write. Any other plugin command counts as a write.
	pluginReads     = setOf("get", "list", "ls", "describe", "logs", "log", "top", "tree", "show", "view", "status", "version", "help", "completion", "template", "validate", "info", "whoami", "explain", "diff")
	readOnlyPlugins = setOf("tree", "access-matrix", "resource-capacity", "who-can", "neat", "krew", "oidc-login", "ns")
	// pluginValue: flags of kubectl plugins that take a value, kubectl-ate's.
	pluginValue = map[string]map[string]bool{
		"ate": setOf("-a", "--atespace", "--endpoint", "--token-file", "--tag", "--scope", "--actor", "--ca-id",
			"--secret-namespace", "--name", "--key-type", "--key-id", "--sandbox-class"),
	}
	// pluginBinary: a kubectl plugin run as its own binary.
	pluginBinary = regexp.MustCompile(`^kubectl-([\w-]+)$`)
	// pluginFunc, pluginVar: a shell function or a variable that runs a
	// kubectl plugin binary.
	pluginFunc = regexp.MustCompile(`(?:^|[\s;&|(])(?:function\s+([\w-]+)\s*(?:\(\))?|([\w-]+)\s*\(\))\s*\{[^}]*?(?:^|[\s/])kubectl-([\w-]+)\s`)
	pluginVar  = regexp.MustCompile(`(?:^|[\s;&|(])(\w+)=\(?\s*["']?(?:[^\s"'()]*/)?kubectl-([\w-]+)(?:[\s"');&|]|$)`)
	helmWrites = setOf("install", "upgrade", "uninstall", "un", "delete", "del", "rollback", "test")
	fluxWrites = setOf("suspend", "resume", "reconcile", "create", "delete", "bootstrap", "install", "uninstall")
	// helm and flux flags that take a value.
	helmValue = setOf("-n", "--namespace", "--kube-context", "--kubeconfig", "-f", "--values", "--set", "--set-string",
		"--set-file", "--version", "--repo", "--timeout", "-o", "--output", "--post-renderer", "--description")
	fluxValue = setOf("-n", "--namespace", "--context", "--kubeconfig", "--timeout", "--source", "--url", "--path",
		"--interval", "--branch", "--tag", "--chart", "--values", "--target-namespace", "-o", "--output")
)

func setOf(ws ...string) map[string]bool {
	m := make(map[string]bool, len(ws))
	for _, w := range ws {
		m[w] = true
	}
	return m
}

// kubeEnv is what a command's kube tools read from the environment, and
// the production context a kubectl wrapper in the command may carry.
type kubeEnv struct {
	kubeconfig, helmContext, wrapped string
}

// KubeGuardOff says why the kube guard is off, "" while it is on.
func KubeGuardOff(production string) string {
	if production != "" {
		return ""
	}
	return "kube guard off: kube.production is unset"
}

// kubeRefusal returns why the hook refuses cmd, "" when it passes.
func (h Hook) kubeRefusal(cmd string) string {
	return h.scanKube(cmd, kubeEnv{kubeconfig: h.Kubeconfig}, 0)
}

func (h Hook) scanKube(cmd string, env kubeEnv, depth int) string {
	if depth > 4 {
		return ""
	}
	sc := scanShell(cmd)
	// aliases: a function or variable that runs kubectl, with the plugin
	// it runs ("" for kubectl itself).
	aliases := map[string]string{}
	for _, re := range []*regexp.Regexp{kubectlFunc, kubectlVar} {
		for _, m := range re.FindAllStringSubmatch(sc.plain, -1) {
			aliases[strings.Join(m[1:], "")] = ""
		}
	}
	for _, re := range []*regexp.Regexp{pluginFunc, pluginVar} {
		for _, m := range re.FindAllStringSubmatch(sc.plain, -1) {
			aliases[strings.Join(m[1:len(m)-1], "")] = strings.ReplaceAll(m[len(m)-1], "_", "-")
		}
	}
	for _, m := range contextFlag.FindAllStringSubmatch(sc.plain, -1) {
		if isProduction(m[1], h.Production) {
			env.wrapped = m[1]
		}
	}
	for _, sg := range sc.segments() {
		words := shellWords(sc.plain[sg.start:sg.end])
		at := strings.Join(words, " ")
		if r := h.simpleKube(words, &env, aliases, at); r != "" {
			return r
		}
		for k, w := range words {
			if !shells[path.Base(w)] {
				continue
			}
			for _, arg := range words[k+1:] {
				if strings.ContainsAny(arg, " \t\n|;&") {
					if r := h.scanKube(arg, env, depth+1); r != "" {
						return r
					}
				}
			}
			for _, d := range sc.heredocs {
				if d.op >= sg.start && d.op < sg.end {
					if r := h.scanKube(cmd[d.start:d.end], env, depth+1); r != "" {
						return r
					}
				}
			}
			break
		}
	}
	return ""
}

// simpleKube decides one simple command. An assignment-only command
// (KUBECONFIG=…, export KUBECONFIG=…, unset KUBECONFIG) changes env for the
// commands after it; a prefix assignment only for its own command.
func (h Hook) simpleKube(words []string, env *kubeEnv, aliases map[string]string, at string) string {
	local := *env
	k := 0
	if k < len(words) && (words[k] == "export" || words[k] == "unset") {
		if words[k] == "unset" {
			for _, w := range words[k+1:] {
				switch w {
				case envKubeconfig:
					env.kubeconfig = h.MachineKubeconfig
				case envHelmContext:
					env.helmContext = ""
				}
			}
			return ""
		}
		k++
	}
	for _, w := range words[k:] {
		m := envWord.FindStringSubmatch(w)
		if m == nil {
			break
		}
		switch m[1] {
		case envKubeconfig:
			local.kubeconfig = expandHome(m[2])
		case envHelmContext:
			local.helmContext = m[2]
		}
		k++
	}
	if k == len(words) {
		*env = local
		return ""
	}
	for i := k; i < len(words); i++ {
		w := words[i]
		if m := envWord.FindStringSubmatch(w); m != nil && m[1] == envKubeconfig {
			local.kubeconfig = expandHome(m[2])
		}
		name, args := path.Base(w), words[i+1:]
		if m := pluginBinary.FindStringSubmatch(name); m != nil {
			name, args = kubectlCmd, append([]string{strings.ReplaceAll(m[1], "_", "-")}, args...)
		}
		if plugin, ok := aliases[strings.Trim(strings.TrimSuffix(strings.TrimSuffix(w, "[@]}"), "[*]}"), "${}")]; ok && name != kubectlCmd {
			name = kubectlCmd
			if local.wrapped != "" {
				args = append([]string{"--context", local.wrapped}, args...)
			}
			if plugin != "" {
				args = append([]string{plugin}, args...)
			}
		}
		if h.Production == "" {
			continue
		}
		var r string
		switch name {
		case kubectlCmd:
			r = h.kubectlRefusal(args, local, at)
		case "kubectx", "kubeswitch", "switcher", "kubie":
			r = switcherRefusal(args, at)
		case "helm":
			r = h.helmRefusal(args, local, at)
		case "flux":
			r = h.fluxRefusal(args, local, at)
		case "tsh":
			r = h.tshRefusal(args, local, at)
		case "kind":
			r = h.kindRefusal(args, local, at)
		default:
			continue
		}
		return r
	}
	return ""
}

// kubeArgs splits a kube tool's arguments into its words that are no flag
// and the values of the flags in values (last one wins), up to "--".
func kubeArgs(args []string, values map[string]bool) ([]string, map[string]string) {
	var pos []string
	flags := map[string]string{}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			break
		}
		if !strings.HasPrefix(a, "-") || a == "-" {
			pos = append(pos, a)
			continue
		}
		name, val, eq := strings.Cut(a, "=")
		if !eq && values[name] && i+1 < len(args) {
			i++
			val = args[i]
		}
		if !eq && !values[name] {
			val = "true"
		}
		flags[name] = val
	}
	return pos, flags
}

func dryRun(flags map[string]string) bool {
	v, ok := flags[flagDryRun]
	return ok && v != "none" && v != "false"
}

func (h Hook) kubectlRefusal(args []string, env kubeEnv, at string) string {
	pos, flags := kubeArgs(args, kubectlValue)
	if len(pos) == 0 {
		return ""
	}
	if !kubectlBuiltins[pos[0]] {
		pos, flags = kubeArgs(args, pluginValues(pos[0]))
	}
	verb, sub := pos[0], ""
	if len(pos) > 1 {
		sub = pos[1]
	}
	switch {
	case verb == "config" && (sub == "use-context" || sub == "use" || sub == "set" && len(pos) > 2 && pos[2] == "current-context"):
		return switchReason(at)
	case verb == "ctx" && len(pos) > 1, verb == "gs" && sub == verbLogin && flags["--self-contained"] == "":
		return switchReason(at)
	case kubectlBuiltins[verb] && !writeSub[verb][sub], dryRun(flags):
		return ""
	case !kubectlBuiltins[verb] && !kubectlWrites[verb] && (readOnlyPlugins[verb] || sub == "" || pluginReads[sub]):
		// A plugin: its subcommand reads, or it has none.
		return ""
	}
	if f := flags[flagKubeconfig]; f != "" {
		env.kubeconfig = expandHome(f)
	}
	return h.writeReason(at, flags["--context"], flags["--cluster"], env.kubeconfig)
}

// pluginValues: the value flags of a kubectl plugin's command line,
// kubectl's own and the plugin's.
func pluginValues(plugin string) map[string]bool {
	values := make(map[string]bool, len(kubectlValue)+len(pluginValue[plugin]))
	for _, m := range []map[string]bool{kubectlValue, pluginValue[plugin]} {
		for f := range m {
			values[f] = true
		}
	}
	return values
}

func (h Hook) helmRefusal(args []string, env kubeEnv, at string) string {
	pos, flags := kubeArgs(args, helmValue)
	if len(pos) == 0 || !helmWrites[pos[0]] || dryRun(flags) {
		return ""
	}
	if f := flags[flagKubeconfig]; f != "" {
		env.kubeconfig = expandHome(f)
	}
	ctx := flags["--kube-context"]
	if ctx == "" {
		ctx = env.helmContext
	}
	return h.writeReason(at, ctx, "", env.kubeconfig)
}

func (h Hook) fluxRefusal(args []string, env kubeEnv, at string) string {
	pos, flags := kubeArgs(args, fluxValue)
	if len(pos) == 0 || !fluxWrites[pos[0]] || flags["--export"] == "true" {
		return ""
	}
	if f := flags[flagKubeconfig]; f != "" {
		env.kubeconfig = expandHome(f)
	}
	return h.writeReason(at, flags["--context"], "", env.kubeconfig)
}

// tshRefusal: tsh kube login, and tsh login with a Kubernetes cluster, set
// the current context of the kubeconfig they write.
func (h Hook) tshRefusal(args []string, env kubeEnv, at string) string {
	pos, flags := kubeArgs(args, setOf("--kube-cluster", "--proxy", "--user", "--auth", "-l", "--login", "--ttl", "-i", "--identity"))
	kube := len(pos) > 1 && pos[0] == "kube" && pos[1] == verbLogin || len(pos) > 0 && pos[0] == verbLogin && flags["--kube-cluster"] != ""
	if !kube || !h.machineKubeconfig(env.kubeconfig) {
		return ""
	}
	return "Refused: `" + short(at) + "` sets the current context of the machine kubeconfig. " + noDefault +
		" The machine kubeconfig already holds every installation's context: pass it on each command " +
		"(kubectl --context " + cmp.Or(h.ContextHint, "<context>") + "). For a context it lacks, log in into a kubeconfig of your own: " +
		"KUBECONFIG=<file> tsh kube login <cluster>."
}

// kindRefusal: kind create cluster and kind export kubeconfig set the
// current context of the kubeconfig they write.
func (h Hook) kindRefusal(args []string, env kubeEnv, at string) string {
	pos, flags := kubeArgs(args, setOf("--name", "--kubeconfig", "--config", "--image", "--wait"))
	if len(pos) < 2 {
		return ""
	}
	if verb := pos[0] + " " + pos[1]; verb != "create cluster" && verb != "export kubeconfig" {
		return ""
	}
	if f := flags[flagKubeconfig]; f != "" {
		env.kubeconfig = expandHome(f)
	}
	if !h.machineKubeconfig(env.kubeconfig) {
		return ""
	}
	return "Refused: `" + short(at) + "` sets the current context of the machine kubeconfig. " + noDefault +
		" Write the lab's kubeconfig to a file of its own: --kubeconfig <file>, then pass it and --context kind-<name> on each command."
}

// switcherRefusal: a context switcher with an argument switches; alone,
// with -c or --current it lists or prints.
func switcherRefusal(args []string, at string) string {
	for _, a := range args {
		if a == "-" || !strings.HasPrefix(a, "-") {
			return switchReason(at)
		}
	}
	return ""
}

const noDefault = "The machine kubeconfig has no current context, so that a command without an explicit target fails instead of reaching production."

func switchReason(at string) string {
	return "Refused: `" + short(at) + "` switches a kubeconfig's current context. " + noDefault +
		" Pass the target on each command instead: kubectl --context <name>, helm --kube-context <name>, flux --context <name> " +
		"(`kubectl config get-contexts -o name` lists them); a lab's own kubeconfig with --kubeconfig <file>."
}

// writeReason refuses a write whose target is a production cluster: the
// context given (or else the kubeconfig's current one), its cluster, or
// the cluster given.
func (h Hook) writeReason(at, context, cluster, kubeconfig string) string {
	kc := readKubeconfig(kubeconfig)
	if context == "" {
		context = kc.current
	}
	target := ""
	for _, n := range []string{context, cluster, kc.clusters[context]} {
		if isProduction(n, h.Production) {
			target = n
			break
		}
	}
	if target == "" {
		return ""
	}
	return "Refused: `" + short(at) + "` writes to " + target + ", a production cluster (" + h.Production + "). " +
		"Agents never change production directly: changes go through a GitOps pull request and platformctl. " +
		"Reads stay allowed (kubectl get, describe, logs, top, auth can-i; helm list, status, get; flux get), " +
		"and so do writes to a lab with --context kind-<lab>."
}

// isProduction reports whether a context or cluster name has production as
// one of its components.
func isProduction(name, production string) bool {
	if production == "" {
		return false
	}
	for _, p := range nameParts.Split(name, -1) {
		if p == production {
			return true
		}
	}
	return false
}

// machineKubeconfig reports whether a kubeconfig list is (or starts with)
// the machine kubeconfig; "" is kubectl's default, the machine's. A hook
// that knows no machine kubeconfig guards none.
func (h Hook) machineKubeconfig(kubeconfig string) bool {
	if h.MachineKubeconfig == "" {
		return false
	}
	if kubeconfig == "" {
		return true
	}
	first, _, _ := strings.Cut(kubeconfig, string(os.PathListSeparator))
	return filepath.Clean(first) == filepath.Clean(h.MachineKubeconfig)
}

func short(at string) string {
	at = strings.Join(strings.Fields(at), " ")
	if len(at) > 200 {
		at = at[:200] + "…"
	}
	return at
}

// kubeconfig is what the guard reads of a kubeconfig: the current context
// and each context's cluster. Users and their credentials are never read.
type kubeconfig struct {
	current  string
	clusters map[string]string
}

// readKubeconfig merges a KUBECONFIG list as kubectl does: the first
// current-context and the first context of a name win. Unreadable files
// count as empty.
func readKubeconfig(list string) kubeconfig {
	kc := kubeconfig{clusters: map[string]string{}}
	for _, p := range filepath.SplitList(os.ExpandEnv(list)) {
		raw, err := os.ReadFile(expandHome(p)) //nolint:gosec // a kubeconfig the command itself names
		if err != nil {
			continue
		}
		var f struct {
			Current  string `yaml:"current-context"`
			Contexts []struct {
				Name    string `yaml:"name"`
				Context struct {
					Cluster string `yaml:"cluster"`
				} `yaml:"context"`
			} `yaml:"contexts"`
		}
		if yaml.Unmarshal(raw, &f) != nil {
			continue
		}
		if kc.current == "" {
			kc.current = f.Current
		}
		for _, c := range f.Contexts {
			if _, ok := kc.clusters[c.Name]; !ok {
				kc.clusters[c.Name] = c.Context.Cluster
			}
		}
	}
	return kc
}

// CurrentContext is the machine kubeconfig's current context, "" when it
// has none or cannot be read.
func CurrentContext(kubeconfig string) string {
	return readKubeconfig(kubeconfig).current
}
