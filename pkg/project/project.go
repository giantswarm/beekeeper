// Package project holds the binary's identity; version and gitSHA are set at build time with
// -ldflags "-X github.com/giantswarm/beekeeper/pkg/project.version=… -X …project.gitSHA=…".
package project

var (
	description = "The beekeeper engine: observes the roadmap board and GitHub, and decides what it would do."
	gitSHA      = "n/a"
	name        = "beekeeper"
	source      = "https://github.com/giantswarm/beekeeper"
	version     = "1.0.0-dev"
)

// Description is a one-line description of the binary.
func Description() string { return description }

// GitSHA is the commit the binary was built from.
func GitSHA() string { return gitSHA }

// Name is the binary's name.
func Name() string { return name }

// Source is the binary's source repository.
func Source() string { return source }

// Version is the binary's version.
func Version() string { return version }
