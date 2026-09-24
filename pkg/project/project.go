// Package project carries the build identity of the beekeeper binary: its
// version, git commit and build time. The generated Makefile (devctl) and the
// architect CI stamp them through `-ldflags -X`; a plain `go build` or
// `go install` has no ldflags and takes the module build info, which Go
// derives from the VCS tag of the checkout.
package project

import (
	"runtime/debug"
	"strings"
)

// Name and Source identify the project in messages.
const (
	Name   = "beekeeper"
	Source = "https://github.com/giantswarm/beekeeper"
)

// Set at link time by the Makefile / architect (`-X <module>/pkg/project.version=…`).
var (
	version        string
	buildTimestamp string
	gitSHA         string
)

// Version returns the version in the form the tags use ("v0.1.0"): the
// linker-stamped one, else the module version Go recorded, else "dev".
func Version() string {
	if v := strings.TrimSpace(version); v != "" {
		if v[0] >= '0' && v[0] <= '9' {
			v = "v" + v
		}
		return v
	}
	if info, ok := debug.ReadBuildInfo(); ok {
		if v := info.Main.Version; v != "" && v != "(devel)" {
			return v
		}
	}
	return "dev"
}

// GitSHA returns the commit the binary was built from, or "" when unknown.
func GitSHA() string {
	if gitSHA != "" {
		return gitSHA
	}
	return buildSetting("vcs.revision")
}

// BuildTimestamp returns the build (or, unstamped, the commit) time in
// RFC 3339, or "" when unknown.
func BuildTimestamp() string {
	if buildTimestamp != "" {
		return buildTimestamp
	}
	return buildSetting("vcs.time")
}

// VersionLine is what `beekeeper version` prints:
// "v0.1.0 (commit 8b5caa0, built 2026-09-24T10:00:00Z)".
func VersionLine() string {
	line := Version()
	var details []string
	if sha := GitSHA(); sha != "" {
		if len(sha) > 7 {
			sha = sha[:7]
		}
		details = append(details, "commit "+sha)
	}
	if ts := BuildTimestamp(); ts != "" {
		details = append(details, "built "+ts)
	}
	if len(details) > 0 {
		line += " (" + strings.Join(details, ", ") + ")"
	}
	return line
}

func buildSetting(key string) string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	for _, s := range info.Settings {
		if s.Key == key {
			return s.Value
		}
	}
	return ""
}
