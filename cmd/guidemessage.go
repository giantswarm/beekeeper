package cmd

import (
	"strings"

	"github.com/giantswarm/beekeeper/internal/state"
)

// guideMessageTarget resolves the role at send time. Old run identities are
// refused rather than silently delivering a request to an unattended session.
func guideMessageTarget(st *state.State, q string) (string, error) {
	q = strings.TrimSpace(q)
	role := guideRole.Get(st)
	if strings.EqualFold(q, "guide") || strings.EqualFold(q, "the guide") {
		if role.Holder == nil {
			return "", refused("no current guide; start a guide before sending this message")
		}
		return wakeID(role.Holder.Party), nil
	}
	matches := func(p state.Party) bool {
		return q != "" && (strings.EqualFold(q, p.Name) || q == p.Session || q == p.HostSession)
	}
	if role.Holder != nil && matches(role.Holder.Party) {
		return q, nil
	}
	for _, r := range role.Relieved {
		if matches(r.Party) {
			if role.Holder == nil {
				return "", refused("%q is a relieved guide; no current guide holds the role", q)
			}
			return "", refused("%q is a relieved guide; current guide is %q; send to guide instead", q, role.Holder.Name)
		}
	}
	return q, nil
}
