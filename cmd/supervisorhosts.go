package cmd

import (
	"github.com/giantswarm/beekeeper/internal/execution"
	"github.com/giantswarm/beekeeper/internal/state"
)

func (a *app) hostPinToolsCurrent(pin state.HostPin, h execution.Host) bool {
	if len(pin.Tools) == 0 {
		return true
	}
	need, err := a.hostRequiredTools(h)
	if err != nil || need[execution.ToolGo] == "" {
		return true
	}
	return execution.CheckTools(pin.Tools, need) == nil
}
