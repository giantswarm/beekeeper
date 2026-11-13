//go:build scenario

package harness

import (
	"errors"
	"fmt"
	"slices"
	"strings"
)

// Builder is the builder block of a scenario: the Kubernetes builder of
// run.remote (docs/remote-builds.md). It gives the desk a fake kubectl, a
// cluster that holds the builder Jobs the desk creates, and one builder
// image: a tool directory in the sandbox. kubectl exec runs a command of
// the desk there, under the Job's environment, as a pod of the image would.
type Builder struct {
	// Tools are the toolchain programs the Go image carries beside its
	// shell and coreutils: go and golangci-lint, each a fake. An image
	// without golangci-lint is one the desk's merge validation has to
	// install the linter into.
	Tools []string `yaml:"tools"`
}

// BuilderTools are the toolchain programs a builder image may carry.
var BuilderTools = []string{"go", "golangci-lint"}

// The builder's place in the sandbox, and what the desk's run.remote names.
const (
	// BuilderRoot holds the image's tool directory and the kubeconfig.
	BuilderRoot = "/work/builder"
	// BuilderContext and BuilderNamespace are the cluster the fake kubectl
	// answers for; BuilderImage is its Go image.
	BuilderContext   = "scenario"
	BuilderNamespace = "builders"
	BuilderImage     = "scenario.invalid/builder-go:fake"
	// Kubectl is the fake's name on the desk's PATH.
	Kubectl = "kubectl"
)

// builderEnv marks a process as running in the builder's pod: the image's
// fake go and golangci-lint answer there.
const builderEnv = "BEEKEEPER_SCENARIO_BUILDER"

// Validate checks the block.
func (b *Builder) Validate() error {
	if b == nil {
		return nil
	}
	var errs []error
	seen := map[string]bool{}
	for _, tool := range b.Tools {
		if !slices.Contains(BuilderTools, tool) {
			errs = append(errs, fmt.Errorf("builder.tools: %q is none of %s", tool, strings.Join(BuilderTools, ", ")))
		}
		if seen[tool] {
			errs = append(errs, fmt.Errorf("builder.tools: %q twice", tool))
		}
		seen[tool] = true
	}
	return errors.Join(errs...)
}
