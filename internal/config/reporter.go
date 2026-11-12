package config

import (
	"time"
)

// Feedback configures the Slack feedback watch of the standby watch. It
// reads the thread of every report the reporter posted, through the Slack
// MCP server behind muster as the person, and delivers each reply of the
// report's author to the supervisor, or to the agent the reply names first
// ("<agent>: <message>").
type Feedback struct {
	// Context is the muster context whose Slack server it reads; unset, no
	// feedback watch runs. The binary and the call timeout are central's.
	Context string `yaml:"context"`
	// Server is the name muster registered the Slack MCP server under: its
	// tools are x_<server>_<tool> (default slack).
	Server string `yaml:"server"`
	// Every is how often the threads are read (default 5m).
	Every Duration `yaml:"every"`
	// Window is how long after its post a report's thread is read (default
	// 24h).
	Window Duration `yaml:"window"`
}

// Enabled says whether the feedback watch runs.
func (f Feedback) Enabled() bool { return f.Context != "" }

// Reporter configures the scheduled status reporter: once per Every the
// standby watch starts one headless session with Brief, which posts one
// report to Person and ends; beekeeper then takes it off the roster.
type Reporter struct {
	// Every is the interval; the reports start on its multiples (on the
	// hour for 1h). Unset, no reporter runs.
	Every Duration `yaml:"every"`
	// Brief is the reporter's brief file (~/ allowed): what to report and
	// where to post it.
	Brief string `yaml:"brief"`
	// Model is the session's model (default: Claude Code's).
	Model string `yaml:"model"`
	// Person is who the report is for (default: guide.person).
	Person string `yaml:"person"`
	// Dir is the session's working directory (~/ allowed; default: home).
	Dir string `yaml:"dir"`
	// Timeout is how long a reporter may run without posting before it is
	// stopped (20m).
	Timeout Duration `yaml:"timeout"`
	// TZ is the report's time zone, an IANA name (default: the machine's).
	TZ string `yaml:"tz"`
	// Reviews are the repositories (owner/repo) whose open draft pull
	// requests wait for the person's review.
	Reviews []string `yaml:"reviews"`
}

// Enabled reports whether a reporter is scheduled.
func (r Reporter) Enabled() bool { return r.Every.Duration > 0 && r.Brief != "" }

func (r *Reporter) defaults(home, person string) {
	setDur(&r.Timeout, 20*time.Minute)
	setStr(&r.Person, person)
	setStr(&r.Dir, home)
	r.Brief, r.Dir = homePath(home, r.Brief), homePath(home, r.Dir)
}
