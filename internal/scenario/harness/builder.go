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
	// shell and coreutils: go, golangci-lint and jq, each a fake. An image
	// without golangci-lint or jq is one the desk's merge validation has
	// to install them into.
	Tools []string `yaml:"tools"`
	// TestsCall are the programs the repository's tests run, such as jq:
	// the pod's go test fails, as such a test does, while one of them is
	// not on the pod's PATH.
	TestsCall []string `yaml:"testsCall"`
}

// BuilderTools are the toolchain programs a builder image may carry, and
// what the pod's go install installs: gojq is the jq a checker installs.
var BuilderTools = []string{"go", "golangci-lint", "jq", "gojq"}

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
// fake go, golangci-lint and jq answer there. builderTestsCallEnv names the
// pod's TestsCall to its go test.
const (
	builderEnv          = "BEEKEEPER_SCENARIO_BUILDER"
	builderTestsCallEnv = "BEEKEEPER_SCENARIO_BUILDER_TESTS_CALL"
)

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
	called := map[string]bool{}
	for _, program := range b.TestsCall {
		if program == "" || strings.ContainsAny(program, "/ \t\n") {
			errs = append(errs, fmt.Errorf("builder.testsCall: %q is no program name", program))
		}
		if called[program] {
			errs = append(errs, fmt.Errorf("builder.testsCall: %q twice", program))
		}
		called[program] = true
	}
	return errors.Join(errs...)
}
