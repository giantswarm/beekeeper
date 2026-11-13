package config

import (
	"errors"
	"slices"
	"strings"

	"github.com/giantswarm/beekeeper/internal/execution"
)

// GitHub configures the budget reading.
// WorkerGitHub is coordinator-only user-to-server GitHub App authority. The
// refresh token is a vault or SOPS reference on the coordinator, never a raw
// value or a file copied to an execution host.
type WorkerGitHub struct {
	ClientID     string `yaml:"clientID"`
	RefreshToken string `yaml:"refreshToken"`
}

func (w *WorkerGitHub) Validate() error {
	if w == nil {
		return nil
	}
	if !execution.ValidIdentity(w.ClientID) || !strings.Contains(w.RefreshToken, "://") || strings.ContainsAny(w.RefreshToken, "\r\n") {
		return errors.New("github.workerApp requires clientID and refreshToken reference")
	}
	// The full reference is parsed at use sites. Config cannot import secret
	// (its sandbox depends on config), but must reject read-only schemes here.
	writable := false
	if rest, ok := strings.CutPrefix(w.RefreshToken, "op://"); ok {
		parts := strings.Split(rest, "/")
		writable = len(parts) >= 3 && !slices.Contains(parts, "")
	}
	if rest, ok := strings.CutPrefix(w.RefreshToken, "sops://"); ok {
		file, path, found := strings.Cut(rest, "#")
		writable = found && file != "" && path != ""
	}
	if !writable {
		return errors.New("github.workerApp.refreshToken must name a writable op:// or sops:// secret reference")
	}
	return nil
}

type GitHub struct {
	WorkerApp *WorkerGitHub `yaml:"workerApp,omitempty"`

	// Floor is the remaining core budget under which GitHub work stops.
	Floor int `yaml:"floor"`
	// UrgentBound is the budget an urgent merge keeps for itself under the
	// floor (lanes urgent): it runs while at least this much remains.
	UrgentBound int `yaml:"urgentBound"`
	// ProbeRepo is the repository whose conditional GET reads the budget
	// headers; any repository the token can read (default: beekeeper's own).
	ProbeRepo string `yaml:"probeRepo"`
	// AllowedOrgs are the organizations and accounts whose repositories an
	// agent writes to (default giantswarm); the hook refuses every GitHub
	// write to any other repository. An empty list turns the guard off.
	AllowedOrgs []string `yaml:"allowedOrgs"`
	// PublicOnlyOwners restricts these owners to verified public repositories
	// at task admission and write boundaries, independently of AllowedOrgs.
	PublicOnlyOwners []string `yaml:"publicOnlyOwners"`
	// UpstreamReview is where a change meant for an upstream project is
	// queued for a human's decision (an issue, owner/repo#n), named in the
	// refusal; unset, the refusal names the team's upstream review list.
	UpstreamReview string `yaml:"upstreamReview"`
}

// GitHubHosts are the hosts the GitHub token goes to.
var GitHubHosts = []string{"github.com", "api.github.com", "uploads.github.com"}
