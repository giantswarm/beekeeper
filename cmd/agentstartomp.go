package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/omp"
	"github.com/giantswarm/beekeeper/internal/sandbox"
	"github.com/giantswarm/beekeeper/internal/secret"
	"github.com/giantswarm/beekeeper/internal/state"
)

// modelFlag names a harness's model on its command line.
const modelFlag = "--model"

// inboxWait bounds the wait for a started omp agent to open its inbox.
const inboxWait = 30 * time.Second

// ompUnit is the transient user unit an omp agent runs in.
func ompUnit(id string) string { return "beekeeper-omp-" + id[:8] }

// startOmpAgent reads the agent's provider key, records and registers the
// agent, starts it in a transient user unit with its stdin on its inbox,
// hands it the key through the inbox and sends it the brief.
func (a *app) startOmpAgent(ctx context.Context, sp agentStart) error {
	dir, err := filepath.Abs(sp.dir)
	if err != nil {
		return err
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return usageErr("--dir %s: not a directory", dir)
	}
	bin, err := exec.LookPath(omp.Comm)
	if err != nil {
		return err
	}
	model, err := ompModel(sp.model, a.cfg.Omp.Model, func() ([]string, error) { return ompModels(ctx, bin) })
	if err != nil {
		return err
	}
	creds, err := a.ompCredentials(model)
	if err != nil {
		return err
	}
	// the keys are read before anything is recorded or started: nothing
	// runs on a key that does not answer
	var keys *secret.Handover
	if len(creds) > 0 {
		ops, err := a.secretOps()
		if err != nil {
			return err
		}
		if keys, err = ops.Handover(ctx, creds); err != nil {
			return vaultExit(fmt.Errorf("%s: its provider's key: %w", sp.name, err))
		}
	}
	shell, err := omp.WriteToolShell(a.cfg.StateDir)
	if err != nil {
		return err
	}
	by, err := a.caller()
	if err != nil {
		return err
	}
	sessions, _, err := a.sessions()
	if err != nil {
		return err
	}
	id := uuid.NewString()
	inbox := omp.InboxPath(a.cfg.StateDir, id)
	if err := omp.MakeInbox(inbox); err != nil {
		return err
	}
	s := state.Start{Party: state.Party{HostSession: omp.HostPrefix + id, Name: sp.name}, Mode: state.ModeBypass, Dir: dir, By: by, At: a.now.UTC(), Harness: omp.Harness, Model: model}
	var reg registration
	err = a.store.Update(func(st *state.State) ([]state.Event, error) {
		var err error
		reg, err = recordStart(st, s, sp.task, sp.serves, func(p state.Party) bool { _, ok := claude.Live(sessions, p); return ok })
		if err != nil {
			return nil, err
		}
		return []state.Event{event(by, "agents.start", "%s: omp agent %s on %s in %s, yolo%s, busy with %q", sp.name, id, model, dir, credentialsText(creds), reg.task)}, nil
	})
	if err != nil {
		return err
	}
	unit := ompUnit(id)
	names := credentialNames(creds)
	if err := launch(unit, dir, a.explicitConfig(), nil, omp.ShellArgv(inbox, names, ompArgv(bin, model)...),
		omp.EnvAgent+"="+id, omp.EnvName+"="+sp.name, "SHELL="+shell, omp.EnvCredentials+"="+strings.Join(names, " ")); err != nil {
		return fmt.Errorf("starting %s: %w (the start stays recorded; beekeeper agents remove %q takes it off the roster)", sp.name, err, sp.name)
	}
	if keys != nil {
		// the unit's shell reads the keys from the inbox before omp runs
		err := whenOpen(ctx, inbox, inboxWait, func() error {
			return omp.Write(inbox, func(w io.Writer) error { _, err := keys.WriteTo(w); return err })
		})
		if err != nil {
			return fmt.Errorf("%s: its provider's key did not reach it: %w (journalctl --user -u %s)", sp.name, err, unit)
		}
	}
	if err := sendWhenOpen(ctx, inbox, sp.brief, inboxWait); err != nil {
		return fmt.Errorf("%s: the brief did not reach it: %w (journalctl --user -u %s)", sp.name, err, unit)
	}
	_, err = fmt.Fprintf(a.out, "started %s: omp agent omp_%s, yolo, on %s, in %s%s, busy with %q\n"+
		"its brief is its first message; agents wake %q <message> reaches it at its next tool round (journalctl --user -u %s)\n",
		sp.name, id, model, dir, credentialsText(creds), reg.task, sp.name, unit)
	return err
}

// ompProvider is the provider of a model selector, provider/id.
func ompProvider(model string) string {
	provider, _, _ := strings.Cut(model, "/")
	return provider
}

// ompCredentials are the provider keys an agent on model gets in its
// environment: for a provider omp.providers names, the variable the models
// file names for it, from the reference. A provider whose key the models
// file reads from a variable that no reference fills, or whose key the file
// carries as a value while a reference names it, refuses the start: nothing
// starts on a key omp would not have, or on one a file holds.
func (a *app) ompCredentials(model string) ([]secret.Credential, error) {
	provider := ompProvider(model)
	name, err := omp.ProviderKey(a.cfg.Omp.ModelsFile, provider)
	ref := a.cfg.Omp.Providers[provider].APIKey
	// the variable the key is handed under, as a suggestion
	suggested := strings.ToUpper(strings.ReplaceAll(provider, "-", "_")) + "_API_KEY"
	switch {
	case errors.Is(err, omp.ErrKeyValue) && ref == "":
		// a provider whose key the file holds and the config does not know: omp's own, as before
		return nil, nil
	case errors.Is(err, omp.ErrKeyValue):
		return nil, usageErr("%v, while omp.providers.%s.apiKey names its reference: replace the value with the variable the key is handed under (apiKey: %s)", err, provider, suggested)
	case err != nil:
		return nil, err
	case name == "" && ref != "":
		return nil, usageErr("%s: providers.%s.apiKey names no variable for the key omp.providers.%s.apiKey refers to: set it to the variable the key is handed under (apiKey: %s)", a.cfg.Omp.ModelsFile, provider, provider, suggested)
	case name != "" && ref == "":
		return nil, usageErr("%s: provider %s reads its key from $%s, which beekeeper fills from no reference: omp.providers.%s.apiKey: op://<vault>/<item>/<field>", a.cfg.Omp.ModelsFile, provider, name, provider)
	case name == "":
		return nil, nil
	}
	r, err := secret.ParseRef(ref)
	if err != nil {
		return nil, fmt.Errorf("omp.providers.%s.apiKey: %w", provider, err)
	}
	return []secret.Credential{{Name: name, Ref: r}}, nil
}

// ompStartBrokered reports whether an omp start on model runs through the
// host's broker: its provider's key comes from the vault, whose session
// lives in the broker alone (secret.session), and this is not the broker's
// own call.
func (a *app) ompStartBrokered(model string) bool {
	return a.cfg.Secret.Session && os.Getenv(sandbox.Brokered) == "" && a.ompNeedsVault(model)
}

// ompNeedsVault reports whether an omp start on model reads the vault: its
// provider is one of omp.providers.
func (a *app) ompNeedsVault(model string) bool {
	if model == "" {
		model = a.cfg.Omp.Model
	}
	_, ok := a.cfg.Omp.Providers[ompProvider(model)]
	return ok
}

// ompStartNeedsVault reports whether a brokered agents start, by its
// arguments (start --harness=omp|auto [--model=m] [--exclude=e] …), reads the
// vault.
func (a *app) ompStartNeedsVault(args []string) bool {
	if len(args) == 0 || args[0] != agentStartName {
		return false
	}
	f := parseStartArgsForVault(args)
	switch f.harness {
	case omp.Harness:
		return a.ompNeedsVault(f.model)
	case harnessAuto:
		return a.autoStartNeedsVault(f)
	}
	return false
}

type startVaultFlags struct {
	harness    string
	model      string
	excludes   []string
	review     bool
	builds     bool
	force      bool
	scratchCfg string
}

func parseStartArgsForVault(args []string) startVaultFlags {
	var f startVaultFlags
	for i := 1; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			break
		}
		switch {
		case strings.HasPrefix(arg, "--harness="):
			f.harness = strings.TrimPrefix(arg, "--harness=")
		case arg == "--harness" && i+1 < len(args):
			i++
			f.harness = args[i]
		case strings.HasPrefix(arg, modelFlag+"="):
			f.model = strings.TrimPrefix(arg, modelFlag+"=")
		case arg == modelFlag && i+1 < len(args):
			i++
			f.model = args[i]
		case strings.HasPrefix(arg, "--exclude="):
			f.excludes = append(f.excludes, strings.Trim(strings.TrimPrefix(arg, "--exclude="), "[]"))
		case arg == "--exclude" && i+1 < len(args):
			i++
			f.excludes = append(f.excludes, strings.Trim(args[i], "[]"))
		case arg == "--review" || arg == "--review=true":
			f.review = true
		case arg == "--builds" || arg == "--builds=true":
			f.builds = true
		case arg == "--force" || arg == "--force=true":
			f.force = true
		case strings.HasPrefix(arg, "--config="):
			f.scratchCfg = strings.TrimPrefix(arg, "--config=")
		case arg == "--config" && i+1 < len(args):
			i++
			f.scratchCfg = args[i]
		}
	}
	return f
}

func (a *app) autoStartNeedsVault(f startVaultFlags) bool {
	target := *a
	if target.now.IsZero() {
		target.now = time.Now()
	}
	if f.scratchCfg != "" {
		if !filepath.IsAbs(f.scratchCfg) {
			return false
		}
		p := target.sandboxPolicy()
		cwd, err := os.Getwd()
		if err != nil || target.sandboxPaths([]string{f.scratchCfg}, nil) != nil || !p.Readable(f.scratchCfg, cwd) {
			return false
		}
		c, err := config.Load(f.scratchCfg)
		if err != nil {
			return false
		}
		if target.sandboxPaths(nil, []string{c.StateDir, c.LeaseDir}) != nil ||
			(c.StateDir != "" && !p.Writable(c.StateDir, cwd)) ||
			(c.LeaseDir != "" && !p.Writable(c.LeaseDir, cwd)) {
			return false
		}
		cfgCopy := *a.cfg
		cfgCopy.StateDir, cfgCopy.LeaseDir = c.StateDir, c.LeaseDir
		target.cfg = &cfgCopy
		target.store = nil
		if fi, err := os.Stat(c.StateDir); err == nil && fi.IsDir() {
			if stStore, err := state.Open(c.StateDir); err == nil {
				target.store = stStore
			}
		}
	}
	k := target.liveCapacity()
	if len(k.Choices) == 0 {
		return false
	}
	wantModel := strings.TrimSpace(f.model)
	if wantModel != "" && !choicesHaveModel(k.Choices, wantModel) {
		return false
	}
	choicesCap := filterChoiceModel(k, wantModel)
	if len(k.Providers) > 0 {
		st := &state.State{}
		if target.store != nil {
			if s, err := target.store.Peek(); err == nil {
				st = s
			}
		}
		sessions, _, _ := target.sessions()
		c := countAgents(st, sessions, target.now)
		pa := countProviderAgents(st, sessions, k, target.now)
		readings := make(map[string]providerReadingResult, len(k.Providers))
		for _, name := range k.ProviderNames() {
			p := k.Providers[name]
			if p.SelfHosted() {
				// A self-hosted provider reads no subscription usage; its
				// ceiling may follow the serving mode of the model it
				// serves, and a node constraint holds the model down to the
				// mode free of the node before any switch ran.
				readings[name] = target.serveModeReadings(context.Background(), name, p)
				continue
			}
			if !p.Enabled() {
				continue
			}
			r, err := target.resolveReading(context.Background(), usageProviderFile(name), p.Usage)
			readings[name] = providerReadingResult{reading: r, err: err, ok: err == nil}
		}
		h := &headroom{
			AvailableMiB:     max(64<<10, k.AvailMinMiB+1024),
			AvailMinMiB:      k.AvailMinMiB,
			SwapGrowthMaxMiB: k.SwapGrowthMaxMiB,
			PSIMax:           k.SwapPSIMax,
			LoadMax:          64,
			Slots:            1,
			SlotsFree:        1,
			MaxLabs:          1,
		}
		v := buildProvidersCapacity(c, pa, choicesCap, readings, h, false, f.excludes, f.review, target.now).stopped(&target, st.Stop)
		ch, err := pickAutoChoice(v, f.force)
		if err != nil || ch == nil {
			return false
		}
		return ch.Harness == omp.Harness && target.ompNeedsVault(ch.Model)
	}
	normEx := normalizeExcludes(f.excludes)
	for _, ch := range choicesCap.Choices {
		if !choiceExcluded(ch, normEx, nil) {
			return ch.Harness == omp.Harness && target.ompNeedsVault(ch.Model)
		}
	}
	return false
}

// credentialNames are the variables the credentials fill.
func credentialNames(creds []secret.Credential) []string {
	names := make([]string, len(creds))
	for i, c := range creds {
		names[i] = c.Name
	}
	return names
}

// credentialsText says, in a start's record and output, which keys the agent
// got and from where; "" for none.
func credentialsText(creds []secret.Credential) string {
	var parts []string
	for _, c := range creds {
		parts = append(parts, "$"+c.Name+" from "+c.Ref.String())
	}
	if len(parts) == 0 {
		return ""
	}
	return ", " + strings.Join(parts, ", ")
}

// ompModel is the model an omp agent starts on: --model, else omp.model.
// It is an exact selector omp lists (provider/id): with none, omp would
// start on its own default, and a pattern on whatever it matches first.
func ompModel(flag, configured string, listed func() ([]string, error)) (string, error) {
	model := flag
	if model == "" {
		model = configured
	}
	if model == "" {
		return "", usageErr("an omp agent needs its model: --model, or omp.model in the config (omp models lists them as provider/id)")
	}
	models, err := listed()
	if err != nil {
		return "", fmt.Errorf("listing omp's models: %w", err)
	}
	if !slices.Contains(models, model) {
		return "", usageErr("omp lists no model %q: name one as provider/id, as omp models lists it", model)
	}
	return model, nil
}

// ompModels are the selectors (provider/id) of the chat models omp lists.
func ompModels(ctx context.Context, bin string) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, inboxWait)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, "models", "--json").Output() //nolint:gosec // omp from PATH
	if err != nil {
		return nil, err
	}
	var list struct {
		Models []struct {
			Selector string `json:"selector"`
		} `json:"models"`
	}
	if err := json.Unmarshal(out, &list); err != nil {
		return nil, err
	}
	models := make([]string, len(list.Models))
	for i, m := range list.Models {
		models[i] = m.Selector
	}
	return models, nil
}

// ompArgv is an omp agent's command line: the rpc protocol, no approvals,
// extensions headless, on its model.
func ompArgv(bin, model string) []string {
	return []string{bin, "--mode", "rpc", "--no-ui", "--approval-mode", "yolo", modelFlag, model}
}

// sendWhenOpen sends msg to the inbox once the agent opened it, waiting up
// to wait for that.
func sendWhenOpen(ctx context.Context, inbox, msg string, wait time.Duration) error {
	return whenOpen(ctx, inbox, wait, func() error { return omp.Send(inbox, msg) })
}

// whenOpen runs write, a write to the inbox, once the agent opened it
// (write answers ErrNotRunning before), waiting up to wait for that.
func whenOpen(ctx context.Context, inbox string, wait time.Duration, write func() error) error {
	deadline := time.Now().Add(wait)
	for {
		err := write()
		if !errors.Is(err, omp.ErrNotRunning) || time.Now().After(deadline) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(awayPoll):
		}
	}
}
