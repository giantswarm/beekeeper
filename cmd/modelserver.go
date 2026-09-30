package cmd

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/lease"
	"github.com/giantswarm/beekeeper/internal/machine"
)

// modelBreach is a model the host's model server holds that no model-server
// lease covers: why, and whether the watch unloads it.
type modelBreach struct {
	Model  machine.HostModel
	Reason string
	Unload bool
}

func (b modelBreach) String() string {
	return fmt.Sprintf("%s %d MiB loaded by %s: %s", b.Model.Label(), b.Model.SizeMiB, cmp.Or(b.Model.Client, "an unknown client"), b.Reason)
}

// hostModels are the models every configured model server holds. A server
// that does not answer adds none; the error is set only when none answered,
// so one server down never hides the other's models.
func hostModels(ctx context.Context, cfg *config.Config) ([]machine.HostModel, error) {
	ollama, oerr := machine.OllamaModels(ctx, cfg.Ollama.URL, cfg.Ollama.Unit)
	lemonade, lerr := machine.LemonadeModels(ctx, cfg.Lemonade.URL)
	// An unconfigured server answers nothing and no error.
	switch {
	case oerr != nil && lerr != nil:
		return nil, errors.Join(oerr, lerr)
	case oerr != nil && cfg.Lemonade.URL == "":
		return nil, oerr
	case lerr != nil && cfg.Ollama.URL == "":
		return nil, lerr
	}
	return append(ollama, lemonade...), nil
}

// serverURL is the API of the model server holding m.
func serverURL(cfg *config.Config, m machine.HostModel) string {
	if m.Server == machine.Lemonade {
		return cfg.Lemonade.URL
	}
	return cfg.Ollama.URL
}

// kindNode is a kind node's container name: <cluster>-control-plane or
// <cluster>-worker<n>.
var kindNode = regexp.MustCompile(`^(.+)-(?:control-plane|worker\d*)$`)

// labOf is the lab resource whose kind cluster loaded m, "" for a client
// that is no kind node. A lab's resource is its cluster's name or, for the
// first of a numbered series whose cluster keeps the bare name (agentlab),
// that name with -1.
func labOf(m machine.HostModel, resources []string) string {
	c := kindNode.FindStringSubmatch(m.Node())
	if c == nil {
		return ""
	}
	for _, r := range []string{c[1], c[1] + "-1"} {
		if slices.Contains(resources, r) {
			return r
		}
	}
	return ""
}

// modelBreaches returns the models no model-server lease covers: loaded
// while nobody holds it, loaded by a lab another session holds, or beyond
// the holder's budget (the largest first, until the rest fits). A model a
// lab loaded, or one over a budget, is unloaded; one the host itself loaded
// without a lease is only named, since a person may be using it.
func modelBreaches(models []machine.HostModel, holders []lease.Holder, resources []string, budgetGiB int) []modelBreach {
	held := map[string]lease.Holder{}
	for _, h := range holders {
		held[h.Env] = h
	}
	ms, leased := held[config.ModelServer]
	var out []modelBreach
	var covered []machine.HostModel
	for _, m := range models {
		lab := labOf(m, resources)
		labHolder, labHeld := held[lab]
		switch {
		case !leased:
			out = append(out, modelBreach{Model: m, Reason: "nobody holds the model-server lease" + labText(lab, labHolder, labHeld), Unload: lab != ""})
		case lab != "" && (!labHeld || !labHolder.Party().Is(ms.Party())):
			out = append(out, modelBreach{Model: m, Unload: true,
				Reason: fmt.Sprintf("the model server is %q's%s", holderName(ms), labText(lab, labHolder, labHeld))})
		default:
			covered = append(covered, m)
		}
	}
	if !leased {
		return out
	}
	budget := cmp.Or(ms.BudgetGiB, budgetGiB)
	total := 0
	for _, m := range covered {
		total += m.SizeMiB
	}
	slices.SortStableFunc(covered, func(a, b machine.HostModel) int { return b.SizeMiB - a.SizeMiB })
	for _, m := range covered {
		if total <= budget<<10 {
			break
		}
		out = append(out, modelBreach{Model: m, Unload: true,
			Reason: fmt.Sprintf("%q's models hold %d MiB, over its model-server budget of %d GiB", holderName(ms), total, budget)})
		total -= m.SizeMiB
	}
	return out
}

func labText(lab string, h lease.Holder, held bool) string {
	switch {
	case lab == "":
		return ""
	case !held:
		return fmt.Sprintf(" (lab %s, not leased)", lab)
	}
	return fmt.Sprintf(" (lab %s, held by %q)", lab, holderName(h))
}

func holderName(h lease.Holder) string { return cmp.Or(h.Name, h.Holder) }

// modelServerLine is the watch's line for breaches, with what it unloaded.
func modelServerLine(bs []modelBreach, unloaded map[string]error) string {
	parts := make([]string, len(bs))
	for i, b := range bs {
		parts[i] = b.String()
		switch err, tried := unloaded[b.Model.Name]; {
		case tried && err == nil:
			parts[i] += "; unloaded"
		case tried:
			parts[i] += "; unload failed: " + err.Error()
		}
	}
	return strings.Join(parts, " | ")
}
