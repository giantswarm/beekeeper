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
	"strconv"
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
	rules.WriteString(strings.Replace(plugin.WorkerRules(), "Own worktree;", "Own worktree; "+a.entryRules()+" ", 1))
	for _, topic := range plugin.Topics {
		rules.WriteString("\n\n" + plugin.Skill(topic))
	}
	rules.WriteString("\n\nExecution host rules:\n" + string(custom))
	// The start guards ride with the security sections: every threshold that
	// gates a remote start on this host (the memory floor, the swap guard
	// with its measuring window, the load and disk limits, the build slots,
	// the kind lab cap) and every bound a capped run consumes under (memcap's
	// memory, CPU quota and desktop weight) belong in the pinned digest. A
	// guard weakened below the reviewed bundle must show up as a digest
	// mismatch in `agents hosts check`, not as a host that quietly starts on
	// weaker guards. What stays host-local of capacity, watch and memcap —
	// the supervisor's pace, watch's alert thresholds and cadence, the slot
	// location and hold bound — shapes this machine's watch and its own desk,
	// never what a pinned remote worker may run or consume.
	security, err := json.Marshal(struct {
		GitHub          any
		Hooks           any
		Secret          any
		Outbound        any
		Sandbox         any
		MaxKindClusters int
		AvailMinMiB     int
		SwapGrowthMax   int
		SwapPSIMax      float64
		SwapWindow      string
		LoadMax         float64
		LoadPerCoreMax  float64
		DiskMinMiB      int
		DiskFillWithin  string
		MemcapSlots     int
		MemcapMax       string
		MemcapCPUQuota  string
		MemcapCPUWeight int
	}{
		GitHub: a.cfg.GitHub, Hooks: a.cfg.Hooks, Secret: a.cfg.Secret, Outbound: a.cfg.Outbound, Sandbox: a.cfg.Sandbox,
		MaxKindClusters: a.cfg.MaxKindClusters,
		AvailMinMiB:     a.cfg.Capacity.AvailMinMiB, SwapGrowthMax: a.cfg.Capacity.SwapGrowthMaxMiB, SwapPSIMax: a.cfg.Capacity.SwapPSIMax, SwapWindow: a.cfg.Watch.SwapWindow.String(),
		LoadMax: a.cfg.Watch.LoadMax, LoadPerCoreMax: a.cfg.Watch.LoadPerCoreMax, DiskMinMiB: a.cfg.Watch.DiskMinMiB, DiskFillWithin: a.cfg.Watch.DiskFillWithin.String(),
		MemcapSlots: a.cfg.Memcap.Slots, MemcapMax: a.cfg.Memcap.Max, MemcapCPUQuota: a.cfg.Memcap.CPUQuota, MemcapCPUWeight: a.cfg.Memcap.CPUWeight,
	})
	if err != nil {
		return nil, err
	}
	s := &execution.Server{Config: cfg, Root: filepath.Join(a.cfg.StateDir, "execution"), Executable: exe, ConfigPath: configPath, Version: project.Version(), SecurityDigest: execution.Digest(security), GuardsDisabled: !a.cfg.Hooks.GuardsEnabled(), Rules: []byte(rules.String()), Launcher: plat.Launcher}
	s.GuardProbe = func() error {
		const probeSession = "probe"
		h := a.executionHook()
		for _, command := range []string{"git push origin main", "gh pr merge 1 --repo giantswarm/beekeeper", "gh issue create --repo forbidden/repository --title fixture --body fixture"} {
			raw, _ := json.Marshal(map[string]any{"session_id": probeSession, "cwd": cfg.WorkspaceRoot, "tool_name": "Bash", "tool_input": map[string]any{hookTypeCommand: command}})
			var out struct {
				Hook struct {
					Decision string `json:"permissionDecision"`
				} `json:"hookSpecificOutput"`
			}
			b := h.DecideExecution(raw, probeSession, cfg.WorkspaceRoot, a.executionProtected(s))
			if json.Unmarshal(b, &out) != nil || out.Hook.Decision != guardDeny {
				return errors.New("execution hook conformance probe failed")
			}
			if _, ok := cfg.Harnesses[execution.HarnessOmp]; ok {
				ompRaw, _ := json.Marshal(map[string]any{executionOmpSession: executionHookProbe, executionOmpCwd: cfg.WorkspaceRoot, executionHookTool: executionOmpBash, executionHookInput: map[string]any{hookTypeCommand: command}})
				out.Hook.Decision = ""
				b = h.DecideExecutionOmp(ompRaw, executionHookProbe, cfg.WorkspaceRoot, a.executionProtected(s))
				if json.Unmarshal(b, &out) != nil || out.Hook.Decision != guardDeny {
					return errors.New("omp execution hook conformance probe failed")
				}
			}
			if _, ok := cfg.Harnesses[execution.HarnessCodex]; ok {
				raw, _ = json.Marshal(map[string]any{"hook_event_name": "PreToolUse", "session_id": probeSession, "cwd": cfg.WorkspaceRoot, "tool_name": "exec_command", "tool_input": map[string]any{"cmd": command}})
				out.Hook.Decision = ""
				b = h.DecideExecutionCodex(raw, probeSession, cfg.WorkspaceRoot, a.executionProtected(s))
				if json.Unmarshal(b, &out) != nil || out.Hook.Decision != guardDeny {
					return errors.New("codex execution hook conformance probe failed")
				}
			}
			if _, ok := cfg.Harnesses[execution.HarnessAgy]; ok {
				raw, _ = json.Marshal(map[string]any{"conversationId": probeSession, "workspacePaths": []string{cfg.WorkspaceRoot}, "toolCall": map[string]any{"name": "run_command", "args": map[string]any{"CommandLine": command}}})
				var agyOut struct {
					Decision string `json:"decision"`
				}
				b = h.DecideExecutionAgy(raw, probeSession, cfg.WorkspaceRoot, a.executionProtected(s))
				if json.Unmarshal(b, &agyOut) != nil || agyOut.Decision != guardDeny {
					return errors.New("agy execution hook conformance probe failed")
				}
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
	return guard.Hook{Transcripts: a.transcriptPolicy(), GuideDesk: a.cfg.Guide.Desk, Self: mustSelf(), Shell: a.cfg.Shell, Outbound: outboundGuard(a.cfg.Outbound), Upstream: guard.Upstream{Orgs: a.cfg.GitHub.AllowedOrgs, Visibility: a.repositoryVisibilityPolicy(context.Background()), Review: a.cfg.GitHub.UpstreamReview}, SecretFiles: append(append([]string(nil), a.cfg.Secret.Files...), a.cfg.Secret.TokenFile), UnlockCommands: a.cfg.Secret.UnlockCommands, Production: a.cfg.Kube.Production, ProtectedContexts: a.cfg.Kube.ProtectedContexts, ReadOnlyExecQueries: a.cfg.Kube.ReadOnlyExecQueries, Kubeconfig: kubeconfigList(), MachineKubeconfig: machineKubeconfig(), ProjectsDir: a.cfg.Projects.Dir, WorktreePlaces: a.cfg.Projects.Worktrees, HomePlaces: a.cfg.Home.Places, PersonFolders: a.cfg.Hooks.PersonFolders, GoCache: goCacheDir(), GoModCache: goModCacheDir()}
}

// executionGuardDecision picks the guard for the hook's format. A hook in
// another harness's format is configuration that does not belong to this
// execution: it is denied in the format asked for, never translated. A codex
// or agy hook binds its calls to the thread the runner recorded.
func executionGuardDecision(h guard.Hook, format string, j execution.Journal, protected []string) func([]byte) []byte {
	h.ExecutionMCP = j.Request.MCP
	if format == execution.HarnessOmp {
		return func(raw []byte) []byte {
			if j.Request.Harness != execution.HarnessOmp {
				return []byte(`{"allow":false,"reason":"hook format does not match the execution harness"}`)
			}
			b := h.DecideExecutionOmp(raw, j.Thread, j.Handle.Workspace, protected)
			var answer struct {
				Output struct {
					Decision string         `json:"permissionDecision"`
					Reason   string         `json:"permissionDecisionReason"`
					Updated  map[string]any `json:"updatedInput"`
				} `json:"hookSpecificOutput"`
			}
			if len(b) > 0 && json.Unmarshal(b, &answer) != nil {
				return []byte(`{"allow":false,"reason":"invalid guard answer"}`)
			}
			// A guard rewrite must never be silently dropped by the extension.
			out, _ := json.Marshal(map[string]any{"allow": answer.Output.Decision != guardDeny && answer.Output.Updated == nil, "reason": answer.Output.Reason})
			return out
		}
	}
	deny := guard.ExecutionDeny
	switch format {
	case execution.HarnessCodex:
		deny = guard.ExecutionDenyCodex
	case execution.HarnessAgy:
		deny = guard.ExecutionDenyAgy
	}
	wantFormat := ""
	switch j.Request.Harness {
	case execution.HarnessCodex:
		wantFormat = execution.HarnessCodex
	case execution.HarnessAgy:
		wantFormat = execution.HarnessAgy
	case execution.HarnessOmp:
		wantFormat = execution.HarnessOmp
	}
	if format != wantFormat {
		return func([]byte) []byte { return deny("hook format does not match the execution's harness") }
	}
	switch format {
	case execution.HarnessCodex:
		return func(raw []byte) []byte { return h.DecideExecutionCodex(raw, j.Thread, j.Handle.Workspace, protected) }
	case execution.HarnessAgy:
		return func(raw []byte) []byte { return h.DecideExecutionAgy(raw, j.Thread, j.Handle.Workspace, protected) }
	default:
		return func(raw []byte) []byte {
			return h.DecideExecution(raw, j.Handle.ConversationID, j.Handle.Workspace, protected)
		}
	}
}

const (
	executionHookProbe  = "probe"
	executionHookInput  = "tool_input"
	executionHookTool   = "tool_name"
	executionOmpBash    = "bash"
	executionOmpCwd     = "cwd"
	executionOmpSession = "session_id"
)

// guardDeny is a hook's deny decision, in both harnesses' formats.
const guardDeny = "deny"

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
	case "agents github-cli", "agents github-git", fatalBudgetCommand, "agents publish", "agents review", "agents rerun", "agents merge", "agents mcp-proxy", workerGitHubRunGroup, "run stats", "run tail", "agents idle", "agents park", "agents note", "agents problem", "note add", "log add", "transcripts", "transcripts list", "transcripts search", "transcripts read", "transcripts request":
		return nil
	case logName:
		cmd.RunE = a.remoteExecutionLog
		return nil
	case supervisorCommand, "supervisor status":
		return refused("supervisor status is coordinator-only; run beekeeper supervisor status on the coordinator")
	case agentsName, "agents list":
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
	var guardFormat string
	guardCmd := &cobra.Command{Use: "execution-guard", Hidden: true, Args: cobra.NoArgs, PersistentPreRunE: func(*cobra.Command, []string) error { a.hook = true; return nil }, RunE: func(cmd *cobra.Command, _ []string) error {
		var out []byte
		denial := guard.ExecutionDeny
		switch guardFormat {
		case execution.HarnessCodex:
			denial = guard.ExecutionDenyCodex
		case execution.HarnessAgy:
			denial = guard.ExecutionDenyAgy
		}
		if guardFormat == execution.HarnessOmp {
			denial = guard.ExecutionDenyOmp
		}
		defer func() {
			if recover() != nil {
				_, _ = cmd.OutOrStdout().Write(denial("execution guard failed; denied"))
			}
		}()
		deny := func() error {
			_, err := cmd.OutOrStdout().Write(denial("execution identity, policy or coordinating authority could not be verified"))
			return err
		}
		if guardFormat != "" && guardFormat != execution.HarnessCodex && guardFormat != execution.HarnessAgy && guardFormat != execution.HarnessOmp {
			return deny()
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
		var guardReq any
		if a.cfg.Hooks.GuardsEnabled() {
			if allowed := executionGuardDecision(a.executionHook(), guardFormat, j, a.executionProtected(s))(raw); !guardDenied(allowed) {
				_, calls := guard.GitHubCalls(raw, guardFormat)
				_, restCalls := guard.GitHubRESTCalls(raw, guardFormat)
				if len(calls)+len(restCalls) > 0 {
					guardReq = executionGuardRequest{Calls: calls, RESTCalls: restCalls}
				}
			}
		}
		var authority executionAuthority
		co := s.Config.Coordinators[j.Request.Coordinator]
		if err := execution.CoordinatorCall(cmd.Context(), co.Callback, string(j.Handle.Ref.Host), "guard", j.Handle.Ref, guardReq, j.Request.ControlKey, &authority, nil); err != nil || authority.Stopped {
			return deny()
		}
		if !a.cfg.Hooks.GuardsEnabled() {
			return nil
		}
		h := a.executionHook()
		if authority.GitHubHeld {
			h.Upstream.Orgs = []string{"__held__"}
		}
		out = executionGuardDecision(h, guardFormat, j, a.executionProtected(s))(raw)
		if out != nil {
			_, err = cmd.OutOrStdout().Write(out)
		}
		return err
	}}
	guardCmd.Flags().StringVar(&guardFormat, "format", "", "hook format: empty for Claude, codex, agy or omp")
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
	return append(a.executionGitHubCommands(), []*cobra.Command{hostRPC, guardCmd, run, a.executionPublishCmd(), a.executionReviewCmd(), a.executionRerunCmd(), a.executionMergeCmd(), a.executionAuthorizationsCmd(), a.executionCoordinatorCmd(), a.executionMCPCmd(), a.executionHostsCmd(), a.executionInspectCmd(), a.executionLogsCmd(), a.executionStopCmd(), a.executionRecoverCmd(), a.executionMessagesCmd()}...)
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
		caps, probeErr := a.probeExecution(cmd.Context(), h)
		// Audit independently of readiness so a policy/tool failure does not
		// hide leftovers. An unreachable audit is explicitly unknown.
		var audit []execution.ResourceFinding
		auditErr := (execution.SSHTransport{Host: h}).Call(cmd.Context(), "resource-audit", execution.Ref{}, nil, &audit)
		if auditErr != nil {
			audit = []execution.ResourceFinding{{Ref: execution.Ref{Host: h.Identity}, State: execution.Unknown, Reason: "resource inventory unreachable or unavailable", Retry: "restore host access and retry hosts check; do not clean unknown resources"}}
		}
		if auditErr == nil {
			st, err := a.store.Peek()
			if err != nil {
				return err
			}
			var registered []execution.Ref
			for _, start := range st.Starts {
				if start.Execution != nil {
					registered = append(registered, start.Execution.Handle.Ref)
				}
			}
			execution.ReportOrphans(audit, a.cfg.Identity.Host, registered)
		}
		caps.ResourceAudit = audit
		caps.PolicyBundle = a.hostBundleReport(cmd.Context(), h)
		if err := json.NewEncoder(cmd.OutOrStdout()).Encode(caps); err != nil {
			return err
		}
		if probeErr != nil || auditErr != nil {
			return errors.Join(probeErr, auditErr)
		}
		// A host merely behind passes: its next start receives the bundle.
		if b := caps.PolicyBundle; b != nil && b.Problem != "" {
			return refused("execution host %s cannot run the coordinator policy bundle: %s", h.Identity, b.Problem)
		}
		return checkHostRelease(caps.Version)
	}})
	var releaseEvidence bool
	reconcile := &cobra.Command{Use: "reconcile <host> <execution> <generation>", Short: "Reconcile one terminal execution; retain recovery work", Args: cobra.ExactArgs(3), RunE: func(cmd *cobra.Command, args []string) error {
		if a.managedExecutionID() != "" {
			return refused("managed workers cannot reconcile host resources")
		}
		h, err := a.executionHost(args[0])
		if err != nil {
			return err
		}
		generation, err := strconv.ParseUint(args[2], 10, 64)
		if err != nil {
			return err
		}
		ref := execution.Ref{Host: h.Identity, Execution: execution.ExecutionID(args[1]), Generation: generation}
		if err := ref.Validate(); err != nil {
			return err
		}
		return (execution.SSHTransport{Host: h}).Call(cmd.Context(), "resource-reconcile", ref, releaseEvidence, &struct{}{})
	}}
	reconcile.Flags().BoolVar(&releaseEvidence, "release-evidence", false, "Attest this execution log is no longer needed for review or recovery; allow removal after 30 days")
	c.AddCommand(reconcile)
	c.AddCommand(&cobra.Command{Use: "provision-tools", Short: "On an idle execution host: install missing pinned Go and gh from checksum-verified official releases", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		if a.managedExecutionID() != "" {
			return refused("managed workers cannot provision host tools")
		}
		s, err := a.hostRuntime()
		if err != nil {
			return err
		}
		busy, release, err := s.Quiesce(cmd.Context())
		if err != nil {
			return err
		}
		defer release()
		if busy > 0 {
			return refused("execution host is busy; tool provisioning waits until executions end")
		}
		return s.ProvisionTools(cmd.Context())
	}})
	c.AddCommand(a.executionHostUpdateCmds()...)
	var loginClientID, loginRefreshRef string
	login := &cobra.Command{Use: "github-login", Short: "Authorize this coordinator's github.workerApp via GitHub OAuth device flow and store its refresh token", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		return a.authorizeWorkerGitHub(cmd.Context(), loginClientID, loginRefreshRef, cmd.OutOrStdout())
	}}
	login.Flags().StringVar(&loginClientID, "client-id", "", "GitHub App client ID (defaults to github.workerApp.clientID)")
	login.Flags().StringVar(&loginRefreshRef, "refresh-token", "", "secret reference for the refresh token (defaults to github.workerApp.refreshToken)")
	c.AddCommand(login)
	c.AddCommand(&cobra.Command{Use: "sync <host>", Short: "Reconcile recorded executions, pending controls and acknowledged stops", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error { return a.syncExecutionHost(cmd.Context(), args[0]) }})
	return c
}

// catalogHost is the agents.hosts entry selector names, as configured.
func (a *app) catalogHost(selector string) (execution.Host, error) {
	h, ok := a.cfg.Agents.Hosts[selector]
	if !ok || selector == executionLocal {
		return h, usageErr("unknown SSH execution host %q", selector)
	}
	if err := h.ValidateCatalog(); err != nil {
		return h, err
	}
	st, err := a.store.Peek()
	if err != nil {
		return h, err
	}
	return h, validateRetainedExecutionRoute(st, selector, h)
}

func validateRetainedExecutionRoute(st *state.State, selector string, h execution.Host) error {
	// Retained journals can still be inspected, stopped or reconciled after
	// exit. Keep their route until their records are retired, including when
	// another selector names the same physical host.
	for _, start := range st.Starts {
		r := start.Execution
		if r == nil || r.Endpoint == nil || (r.Selector != selector && r.Handle.Ref.Host != h.Identity) {
			continue
		}
		if r.Handle.Ref.Host != h.Identity || *r.Endpoint != h.Endpoint {
			return refused("execution host %s route changed while retained execution %s depends on its approved endpoint; restore the original host identity and SSH executable/config route before continuing", selector, r.Handle.Ref.Execution)
		}
	}
	return nil
}

// executionHost is the agents.hosts entry selector names, pinned to the
// policy digest beekeeper recorded for it (agents hosts update). The state
// is peeked, never waited on: callers run inside a state update too.
func (a *app) executionHost(selector string) (execution.Host, error) {
	h, err := a.catalogHost(selector)
	if err != nil {
		return h, err
	}
	st, err := a.store.Peek()
	if err != nil {
		return h, err
	}
	h = pinnedHost(st, selector, h)
	if h.PolicyDigest == "" {
		return h, refused("execution host %s has no pinned policy: beekeeper agents hosts update %s pins it", selector, selector)
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
	if !c.CIWait {
		return c, refused("execution host lacks coordinator CI wait support; upgrade and prepare the host before starting workers")
	}
	// A worker reads GitHub with a scoped credential of the worker App, which
	// takes the host's adapter (c.GitHub as the host reports it, and agents
	// start refuses a host without it), or with the host's own gh login. An
	// older host does not say whether it has one.
	if a.cfg.GitHub.WorkerApp == nil {
		c.GitHub = c.GitHubLogin != execution.GitHubLoggedOut
	}
	if a.cfg.GitHub.WorkerApp == nil && !c.GitHub {
		return c, refused("execution host gives its workers no GitHub reads: github.workerApp is not configured on the coordinator and gh is not logged in on the host; configure the App, or log gh in there, before starting workers")
	}
	if !c.SourcePreparation {
		return c, refused("execution host lacks source preparation protocol support (source upload and typed not-found); upgrade and prepare the host before starting workers")
	}
	offered := false
	for _, name := range h.Harnesses {
		offered = offered || c.Harnesses[name] != ""
	}
	if c.Identity != h.Identity || c.Protocol != execution.Protocol || c.PolicyDigest != h.PolicyDigest || !offered {
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

func checkHostRelease(version string) error {
	want := coordinatorVersion()
	if releaseVersion(want) && version != want {
		return refused("execution host runs %s, coordinator runs %s; run beekeeper agents hosts update <host>", version, want)
	}
	return nil
}
