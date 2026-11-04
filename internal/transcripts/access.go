package transcripts

import (
	"cmp"
	"strings"
	"time"

	"github.com/giantswarm/beekeeper/internal/state"
)

// Caller describes the session or operator requesting transcript access.
type Caller struct {
	Party      state.Party
	RosterName string
	Execution  string
	Role       string // "guide", "worker", or ""
	Repo       string // worker's repository (owner/repo)
	IsGuide    bool   // true when caller holds the guide role or is an unmanaged operator CLI call
}

// Allowed reports whether caller may read or search meta at now.
func Allowed(caller Caller, meta Meta, grants []state.TranscriptGrant, now time.Time) bool {
	if caller.IsGuide {
		return true
	}
	if isOwnTranscript(caller, meta) {
		return true
	}
	// Default worker access: transcripts of workers on the same repository.
	callerRepo := strings.TrimSpace(caller.Repo)
	metaRepo := cmp.Or(strings.TrimSpace(meta.Repo), RepoOfRef(meta.Ref))
	if callerRepo != "" && metaRepo != "" && strings.EqualFold(meta.Role, "worker") && strings.EqualFold(callerRepo, metaRepo) {
		return true
	}
	for _, g := range grants {
		if !g.Active(now) || !GrantAppliesTo(g, caller) {
			continue
		}
		if grantCovers(g, meta, metaRepo) {
			return true
		}
	}
	return false
}

// Filter returns the subset of metas that caller is allowed to read or search at now.
func Filter(metas []Meta, caller Caller, grants []state.TranscriptGrant, now time.Time) []Meta {
	if caller.IsGuide {
		return metas
	}
	var out []Meta
	for _, m := range metas {
		if Allowed(caller, m, grants, now) {
			out = append(out, m)
		}
	}
	return out
}

func isOwnTranscript(caller Caller, meta Meta) bool {
	if caller.Execution != "" && meta.Execution != "" && strings.EqualFold(caller.Execution, meta.Execution) {
		return true
	}
	if caller.Party.Session != "" && (strings.EqualFold(caller.Party.Session, meta.ID) || strings.EqualFold(caller.Party.Session, meta.HostSession)) {
		return true
	}
	if caller.Party.HostSession != "" && (strings.EqualFold(caller.Party.HostSession, meta.HostSession) || strings.EqualFold(caller.Party.HostSession, meta.ID)) {
		return true
	}
	if caller.RosterName != "" && meta.RosterName != "" && strings.EqualFold(caller.RosterName, meta.RosterName) {
		return true
	}
	return false
}

// GrantAppliesTo reports whether grant g was issued to caller.
func GrantAppliesTo(g state.TranscriptGrant, caller Caller) bool {
	if (caller.Party.Session != "" || caller.Party.HostSession != "") && g.To.Is(caller.Party) {
		return true
	}
	for _, target := range []string{g.To.Name, g.To.Session, g.To.HostSession} {
		if target == "" {
			continue
		}
		for _, own := range []string{caller.RosterName, caller.Party.Name, caller.Party.Session, caller.Party.HostSession, caller.Execution} {
			if own != "" && strings.EqualFold(target, own) {
				return true
			}
		}
	}
	return false
}

func grantCovers(g state.TranscriptGrant, meta Meta, metaRepo string) bool {
	if g.All {
		return true
	}
	if g.Repo != "" && metaRepo != "" && strings.EqualFold(g.Repo, metaRepo) {
		return true
	}
	if g.Session != "" {
		// Search accepts substrings, but an authorization must not expand a
		// grant for guide-1 to guide-10 or another similarly named session.
		if meta.ExactMatch(g.Session) {
			return true
		}
		if ref := NormalizeRef(g.Session); ref != "" {
			if strings.EqualFold(meta.Ref, ref) {
				return true
			}
			for _, r := range meta.Refs {
				if strings.EqualFold(r, ref) {
					return true
				}
			}
		}
	}
	return false
}
