package cmd

import (
	"maps"
	"slices"

	"github.com/giantswarm/beekeeper/internal/state"
)

func (a *app) hostPlacementViews(st *state.State) []hostPlacementView {
	var views []hostPlacementView
	for _, selector := range slices.Sorted(maps.Keys(a.cfg.Agents.Hosts)) {
		placement := st.HostPlacements[selector]
		v := hostPlacementView{Selector: selector, At: placement.At, Repository: placement.Repository, Reason: placement.Reason}
		if !slices.Contains(a.cfg.Supervisor.Hosts, selector) {
			v.Reason = "not enabled in supervisor.hosts"
		} else if v.Reason == "" {
			v.Reason = "no placement attempt recorded"
		}
		if pin := st.HostPin(selector); pin != nil && pin.Identity == a.cfg.Agents.Hosts[selector].Identity {
			v.Update = pin.UpdateError
			if hostHoldBlocks(*pin) {
				v.Reason = "held: " + pin.Held
			} else if pin.Held != "" {
				v.Update = pin.Held
			}
		}
		views = append(views, v)
	}
	return views
}
