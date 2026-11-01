package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/execution"
	"github.com/giantswarm/beekeeper/internal/guard"
	"github.com/giantswarm/beekeeper/internal/machine"
	"github.com/giantswarm/beekeeper/internal/proc"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/pkg/project"
	"github.com/giantswarm/beekeeper/plugin"
)

func (a *app) hostRuntime() (*execution.Server, error) {
	if a.executionRuntime != nil {
		return a.executionRuntime()
	}
	if a.cfg.Agents.Execution == nil {
		return nil, refused("this machine has no agents.execution configuration")
	}
	if a.cfg.Sandbox.Required {
		return nil, refused("the SSH adapter does not provide a required host sandbox; hooks alone are not a sandbox")
	}
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	configPath := a.loadedCfg
	if configPath == "" {
		configPath = a.explicitConfig()
	}
	if !filepath.IsAbs(configPath) {
		return nil, refused("execution host requires an explicit absolute configuration path")
	}
	cfg := *a.cfg.Agents.Execution
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	// Rules are operator-provided text, never an authentication store. Known
	// credential files and credential-shaped content cannot become a prompt.
	if !guard.ExecutionRulesAllowed(cfg.RulesFile, a.cfg.Secret.Files) {
		return nil, refused("execution rules must not name a credential store")
	}
	custom, err := os.ReadFile(filepath.Clean(cfg.RulesFile))
	if err != nil {
		return nil, err
	}
	if len(custom) == 0 || len(custom) > 256<<10 || len(guard.CredentialShapes(string(custom))) > 0 {
		return nil, refused("execution rules are empty, oversized or credential-shaped")
	}
	var rules strings.Builder
	rules.WriteString(plugin.WorkerRules())
	for _, topic := range plugin.Topics {
		rules.WriteString("\n\n" + plugin.Skill(topic))
	}
	rules.WriteString("\n\nExecution host rules:\n" + string(custom))
	security, err := json.Marshal(struct {
		GitHub   any
		Hooks    any
		Secret   any
		Outbound any
		Sandbox  any
	}{a.cfg.GitHub, a.cfg.Hooks, a.cfg.Secret, a.cfg.Outbound, a.cfg.Sandbox})
	if err != nil {
		return nil, err
	}
	s := &execution.Server{Config: cfg, Root: filepath.Join(a.cfg.StateDir, "execution"), Executable: exe, ConfigPath: configPath, Version: project.Version(), SecurityDigest: execution.Digest(security), Rules: []byte(rules.String()), Launcher: plat.Launcher}
	s.GuardProbe = func() error {
		h := a.executionHook()
		for _, command := range []string{"git push origin main", "gh pr merge 1 --repo giantswarm/beekeeper", "gh issue create --repo forbidden/repository --title fixture --body fixture"} {
			raw, _ := json.Marshal(map[string]any{"session_id": "probe", "cwd": cfg.WorkspaceRoot, "tool_name": "Bash", "tool_input": map[string]any{hookTypeCommand: command}})
			var out struct {
				Hook struct {
					Decision string `json:"permissionDecision"`
				} `json:"hookSpecificOutput"`
			}
			b := h.DecideExecution(raw, "probe", cfg.WorkspaceRoot, a.executionProtected(s))
			if json.Unmarshal(b, &out) != nil || out.Hook.Decision != "deny" {
				return errors.New("execution hook conformance probe failed")
			}
		}
		return nil
	}
	s.Resources = func(ctx context.Context) (execution.Resources, error) {
		h := a.readHeadroom(ctx, a.liveCapacity(), a.watchSwap())
		disk, err := machine.ReadDisk(cfg.WorkspaceRoot)
		if err != nil {
			return execution.Resources{}, err
		}
		blocks := h.blocks(false)
		buildBlocks := []string{}
		if h.slotBusy() {
			buildBlocks = append(buildBlocks, noFreeSlot)
		}
		return execution.Resources{AvailableMiB: h.AvailableMiB, Load1: h.Load1, DiskFreeGiB: float64(disk.FreeMiB) / 1024, BuildBusy: h.slotBusy(), Blocks: blocks, BuildBlocks: buildBlocks, ObservedAt: time.Now().UTC()}, nil
	}
	return s, nil
}

func (a *app) executionHook() guard.Hook {
	return guard.Hook{Transcripts: a.transcriptPolicy(), Self: mustSelf(), Shell: a.cfg.Shell, Outbound: outboundGuard(a.cfg.Outbound), Upstream: guard.Upstream{Orgs: a.cfg.GitHub.AllowedOrgs, Review: a.cfg.GitHub.UpstreamReview}, SecretFiles: append(append([]string(nil), a.cfg.Secret.Files...), a.cfg.Secret.TokenFile), UnlockCommands: a.cfg.Secret.UnlockCommands, Production: a.cfg.Kube.Production, Kubeconfig: kubeconfigList(), MachineKubeconfig: machineKubeconfig(), PersonFolders: a.cfg.Hooks.PersonFolders, GoCache: goCacheDir(), GoModCache: goModCacheDir()}
}

func mustSelf() string { s, _ := os.Executable(); return s }

func (a *app) executionProtected(s *execution.Server) []string {
	paths := []string{a.cfg.StateDir, s.ConfigPath, s.Executable, s.Config.WorkspaceRoot, s.Config.RulesFile}
	for _, source := range s.Config.Repositories {
		paths = append(paths, source)
	}
	for _, h := range s.Config.Harnesses {
		if h.ConfigDir != "" {
			paths = append(paths, h.ConfigDir)
		}
	}
	return paths
}

func executionUnitID(unit string) string {
	if !strings.HasPrefix(unit, "beekeeper-execution-") || !strings.HasSuffix(unit, ".service") {
		return ""
	}
	x := strings.TrimSuffix(strings.TrimPrefix(unit, "beekeeper-execution-"), ".service")
	if len(x) < 38 {
		return ""
	}
	id := x[:36]
	if _, err := uuid.Parse(id); err != nil || x[36] != '-' {
		return ""
	}
	return id
}

func (a *app) managedExecutionID() string {
	if id := os.Getenv(execution.EnvID); id != "" {
		return id
	}
	if id := executionUnitID(ownUnit()); id != "" {
		return id
	}
	if a.cfg == nil || a.cfg.Agents.Execution == nil {
		return ""
	}
	table, err := proc.Read()
	if err != nil {
		return ""
	}
	for p := table.ByPID[os.Getpid()]; p != nil; p = table.ByPID[p.PPID] {
		if id := executionUnitID(filepath.Base(proc.Cgroup(p.PID))); id != "" {
			return id
		}
		if p.PID == p.PPID {
			break
		}
	}
	return ""
}

func (a *app) executionCaller() (*execution.Server, execution.Journal, error) {
	var j execution.Journal
	if a.as != "" {
		return nil, j, refused("managed execution identity cannot be overridden with --as")
	}
	s, err := a.hostRuntime()
	if err != nil {
		return nil, j, err
	}
	j, err = s.Caller(a.managedExecutionID())
	if err != nil {
		return nil, j, err
	}
	if co := os.Getenv(execution.EnvCoordinator); co != "" && co != j.Request.Coordinator {
		return nil, j, refused("execution coordinator identity cannot be overridden")
	}
	return s, j, nil
}

func (a *app) checkExecutionCommand(cmd *cobra.Command) error {
	if a.managedExecutionID() == "" {
		return nil
	}
	if _, _, err := a.executionCaller(); err != nil {
		return err
	}
	path := strings.TrimPrefix(cmd.CommandPath(), cmd.Root().Name()+" ")
	switch path {
	case "agents publish", "agents merge", "run", "agents idle", "agents park", "agents note", "note add", "log add":
		return nil
	case agentsName, "agents list", "supervisor status":
		cmd.RunE = func(cmd *cobra.Command, _ []string) error {
			s, j, err := a.executionCaller()
			if err != nil {
				return err
			}
			var rows json.RawMessage
			co := s.Config.Coordinators[j.Request.Coordinator]
			if err := execution.CoordinatorCall(cmd.Context(), co.Callback, string(j.Handle.Ref.Host), executionStatus, j.Handle.Ref, nil, j.Request.ControlKey, &rows, nil); err != nil {
				return err
			}
			_, err = fmt.Fprintln(a.out, string(rows))
			return err
		}
		return nil
	default:
		return refused("this control is unavailable to a managed SSH worker; use the coordinating component (https://github.com/giantswarm/beekeeper/issues/1714)")
	}
}

func (a *app) executionHostCmds() []*cobra.Command {
	var protocol int
	hostRPC := &cobra.Command{Use: "host-rpc", Hidden: true, Args: cobra.NoArgs, PersistentPreRunE: func(*cobra.Command, []string) error { a.hook = true; return a.load() }, RunE: func(cmd *cobra.Command, _ []string) error {
		if protocol != execution.Protocol || a.managedExecutionID() != "" {
			return refused("invalid host RPC caller or protocol")
		}
		s, err := a.hostRuntime()
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
		defer cancel()
		return s.Serve(ctx, cmd.InOrStdin(), cmd.OutOrStdout())
	}}
	hostRPC.Flags().IntVar(&protocol, "protocol", 0, "execution protocol")
	guardCmd := &cobra.Command{Use: "execution-guard", Hidden: true, Args: cobra.NoArgs, PersistentPreRunE: func(*cobra.Command, []string) error { a.hook = true; return nil }, RunE: func(cmd *cobra.Command, _ []string) error {
		var out []byte
		defer func() {
			if recover() != nil {
				_, _ = cmd.OutOrStdout().Write(guard.ExecutionDeny("execution guard failed; denied"))
			}
		}()
		deny := func() error {
			_, err := cmd.OutOrStdout().Write(guard.ExecutionDeny("execution identity, policy or coordinating authority could not be verified"))
			return err
		}
		if err := a.load(); err != nil {
			return deny()
		}
		s, j, err := a.executionCaller()
		if err != nil {
			return deny()
		}
		if _, err := s.InstalledPolicy(); err != nil {
			return deny()
		}
		raw, err := io.ReadAll(io.LimitReader(cmd.InOrStdin(), execution.MaxWire+1))
		if err != nil || len(raw) > execution.MaxWire {
			return deny()
		}
		var authority executionAuthority
		co := s.Config.Coordinators[j.Request.Coordinator]
		if err := execution.CoordinatorCall(cmd.Context(), co.Callback, string(j.Handle.Ref.Host), "guard", j.Handle.Ref, nil, j.Request.ControlKey, &authority, nil); err != nil || authority.Stopped {
			return deny()
		}
		h := a.executionHook()
		if authority.GitHubHeld {
			h.Upstream.Orgs = []string{"__held__"}
		}
		out = h.DecideExecution(raw, j.Handle.ConversationID, j.Handle.Workspace, a.executionProtected(s))
		if out != nil {
			_, err = cmd.OutOrStdout().Write(out)
		}
		return err
	}}
	run := &cobra.Command{Use: "execution-run <execution>", Hidden: true, Args: cobra.ExactArgs(1), PersistentPreRunE: func(*cobra.Command, []string) error { a.hook = true; return a.load() }, RunE: func(cmd *cobra.Command, args []string) error {
		s, err := a.hostRuntime()
		if err != nil {
			return err
		}
		j, err := s.Caller(args[0])
		if err != nil {
			return err
		}
		ctx, cancel := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
		defer cancel()
		return s.Run(ctx, j.Handle.Ref)
	}}
	return []*cobra.Command{hostRPC, guardCmd, run, a.executionPublishCmd(), a.executionMergeCmd(), a.executionAuthorizationsCmd(), a.executionCoordinatorCmd(), a.executionHostsCmd(), a.executionInspectCmd(), a.executionLogsCmd(), a.executionStopCmd(), a.executionRecoverCmd(), a.executionMessagesCmd()}
}

func (a *app) executionHostsCmd() *cobra.Command {
	c := &cobra.Command{Use: "hosts", Short: "Validate and prepare explicit SSH execution hosts", Args: cobra.NoArgs}
	c.AddCommand(&cobra.Command{Use: "key", Short: "Create the coordinator signing key once and print its public key", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		key, err := execution.SigningKey(filepath.Join(a.cfg.StateDir, "execution-coordinator"))
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(cmd.OutOrStdout(), execution.PublicKey(key))
		return err
	}})
	c.AddCommand(&cobra.Command{Use: "bundle", Short: "Print the policy manifest this host can prepare", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		s, err := a.hostRuntime()
		if err != nil {
			return err
		}
		return json.NewEncoder(cmd.OutOrStdout()).Encode(s.Policy())
	}})
	var bundle string
	prepare := &cobra.Command{Use: "prepare --bundle <file>", Short: "Install matching worker rules and hooks in this host's managed policy directory", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		s, err := a.hostRuntime()
		if err != nil {
			return err
		}
		b, err := os.ReadFile(filepath.Clean(bundle))
		if err != nil {
			return err
		}
		var p execution.Policy
		if err := execution.Decode(b, &p); err != nil {
			return err
		}
		digest, err := s.Prepare(p)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(cmd.OutOrStdout(), digest)
		return err
	}}
	prepare.Flags().StringVar(&bundle, "bundle", "", "policy manifest exported by hosts bundle")
	c.AddCommand(prepare)
	c.AddCommand(&cobra.Command{Use: "check <host>", Short: "Check SSH identity, versions, rules, hooks, authentication and resources without a model call", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		h, err := a.executionHost(args[0])
		if err != nil {
			return err
		}
		caps, err := a.probeExecution(cmd.Context(), h)
		if err != nil {
			return err
		}
		return json.NewEncoder(cmd.OutOrStdout()).Encode(caps)
	}})
	c.AddCommand(&cobra.Command{Use: "sync <host>", Short: "Reconcile recorded executions, pending controls and acknowledged stops", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error { return a.syncExecutionHost(cmd.Context(), args[0]) }})
	return c
}

func (a *app) executionHost(selector string) (execution.Host, error) {
	h, ok := a.cfg.Agents.Hosts[selector]
	if !ok || selector == executionLocal {
		return h, usageErr("unknown SSH execution host %q", selector)
	}
	return h, h.Validate()
}

func (a *app) executionClient(h execution.Host) execution.Transport {
	if a.executionTransport != nil {
		return a.executionTransport(h)
	}
	return execution.SSHTransport{Host: h}
}

func (a *app) probeExecution(ctx context.Context, h execution.Host) (execution.Capabilities, error) {
	var c execution.Capabilities
	var err error
	if a.executionProbe != nil {
		c, err = a.executionProbe(ctx, h)
	} else {
		c, err = (execution.SSHTransport{Host: h}).Check(ctx)
	}
	if err != nil {
		return c, err
	}
	// Protocol is the wire compatibility boundary. The pinned host policy
	// still binds its own binary and rules; a coordinator-only release must
	// not invalidate that independently prepared policy.
	if !c.Publication {
		return c, refused("execution host lacks coordinator publication support; upgrade and prepare the host before starting workers")
	}
	if c.Identity != h.Identity || c.Protocol != execution.Protocol || c.PolicyDigest != h.PolicyDigest || c.Harnesses[claudeComm] == "" {
		return c, refused("execution host identity, protocol, harness or policy does not match")
	}
	if err := c.Resources.Validate(time.Now()); err != nil {
		return c, refused("%v", err)
	}
	return c, nil
}

func executionRecord(st *state.State, ref execution.Ref) (*state.ExecutionRecord, int) {
	for i := range st.Starts {
		if x := st.Starts[i].Execution; x != nil && x.Handle.Ref == ref {
			return x, i
		}
	}
	return nil, -1
}
