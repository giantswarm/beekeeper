package cmd

import (
	"context"
	"errors"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/machine"
)

// hostModels are the models every configured model server holds. A server
// that does not answer adds none; the error is set only when none answered,
// so one server down never hides the other's models.
func hostModels(ctx context.Context, cfg *config.Config) ([]machine.HostModel, error) {
	ollama, oerr := machine.OllamaModels(ctx, cfg.Ollama.URL, cfg.Ollama.Unit, plat.Machine.ServiceLog)
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
