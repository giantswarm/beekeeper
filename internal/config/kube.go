package config

import (
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Kube configures the kube guard and how an installation's name becomes its
// kube context.
type Kube struct {
	// APIReads are operator-provisioned API identities and exact read RPCs.
	APIReads map[string]APIRead `yaml:"apiReads"`
	// Production is the installation whose clusters agents never write to:
	// a context or cluster name with it as a component. Empty, the kube
	// guard is off unless ProtectedContexts is set.
	Production string `yaml:"production"`
	// ProtectedContexts lists additional production-class contexts (including
	// full test installations) whose clusters agents must not change directly.
	ProtectedContexts []string `yaml:"protectedContexts"`
	// ReadOnlyExecQueries lists exact SELECT queries allowed through psql -X -c.
	ReadOnlyExecQueries []string `yaml:"readOnlyExecQueries"`
	// ProductionNoteDeletes lets an answered note authorise one literal
	// production kubectl delete. Off by default, and unsafe to switch on where
	// agents can record answers: nothing proves a person gave the answer, so an
	// agent that can record one can authorise its own deletion.
	ProductionNoteDeletes bool `yaml:"productionNoteDeletes"`
	// ContextTemplate is the kube context of an installation, with
	// {installation} for its name ("login.example.com-{installation}");
	// empty, an installation's context is its name or the one ending in
	// @<name>.
	ContextTemplate string `yaml:"contextTemplate"`
	// Kubeconfig lists the kubeconfig files beekeeper's own reads resolve
	// a context in, merged as kubectl merges a KUBECONFIG list (~/
	// allowed): the k8s:// source of beekeeper secret, whose Secret read a
	// kind lab's context makes through kind instead. Empty, the files
	// KUBECONFIG lists, else the machine kubeconfig ~/.kube/config.
	Kubeconfig []string `yaml:"kubeconfig"`
}

// APIRead binds a reader identity to one TLS endpoint and token audience.
// The API must enforce read-only authorization for this service account.
type APIRead struct {
	Context        string   `yaml:"context"`
	Namespace      string   `yaml:"namespace"`
	ServiceAccount string   `yaml:"serviceAccount"`
	Audience       string   `yaml:"audience"`
	Endpoint       string   `yaml:"endpoint"`
	Methods        []string `yaml:"methods"`
}

// Teleport configures either the supervised Machine ID bot or the periodic
// SSO keeper, which renews under the browser lease.
type Teleport struct {
	// Bot selects headless Machine ID operation instead of SSO renewal.
	Bot TeleportBot `yaml:"bot"`
	// Proxy is tsh login's --proxy. Without a proxy or a bot, no keeper
	// runs and the watch reads no identity.
	Proxy string `yaml:"proxy"`
	// Auth is tsh login's --auth, the SSO connector; empty, the cluster's
	// default.
	Auth string `yaml:"auth"`
	// Tsh is the tsh binary (default tsh).
	Tsh string `yaml:"tsh"`
	// Home is the profile directory the kube contexts' tsh reads (default
	// $TELEPORT_HOME, else ~/.tsh).
	Home string `yaml:"home"`
	// RenewBefore is how long before the expiry the keeper renews (default
	// 90m); WarnBefore is when the watch says the login is about to expire
	// (default 1h), at most RenewBefore.
	RenewBefore Duration `yaml:"renewBefore"`
	WarnBefore  Duration `yaml:"warnBefore"`
	// LoginTimeout bounds one login, the browser's round trip included
	// (default 3m).
	LoginTimeout Duration `yaml:"loginTimeout"`
	// Every is how often the keeper's timer checks the expiry (default 10m).
	Every Duration `yaml:"every"`
}

// TeleportBot points at an administrator-provisioned tbot configuration and outputs.
// The configuration, rather than beekeeper's config or argv, holds the join token.
type TeleportBot struct {
	Config      string `yaml:"config"`
	Binary      string `yaml:"binary"`
	Certificate string `yaml:"certificate"`
	Kubeconfig  string `yaml:"kubeconfig"`
}

// Enabled reports whether Machine ID replaces SSO.
func (b TeleportBot) Enabled() bool { return b.Config != "" }

// Enabled reports whether a keeper is configured.
func (t Teleport) Enabled() bool { return t.Proxy != "" || t.Bot.Enabled() }

func (t *Teleport) defaults(home string) {
	setStr(&t.Bot.Binary, "tbot")
	t.Bot.Config = homePath(home, t.Bot.Config)
	t.Bot.Certificate = homePath(home, t.Bot.Certificate)
	t.Bot.Kubeconfig = homePath(home, t.Bot.Kubeconfig)
	setStr(&t.Tsh, "tsh")
	setStr(&t.Home, os.Getenv("TELEPORT_HOME"))
	setStr(&t.Home, filepath.Join(home, ".tsh"))
	setDur(&t.RenewBefore, 90*time.Minute)
	setDur(&t.WarnBefore, time.Hour)
	setDur(&t.LoginTimeout, 3*time.Minute)
	setDur(&t.Every, 10*time.Minute)
}

// Context is the templated context of installation, "" without a template.
func (k Kube) Context(installation string) string {
	if k.ContextTemplate == "" {
		return ""
	}
	return strings.ReplaceAll(k.ContextTemplate, "{installation}", installation)
}
