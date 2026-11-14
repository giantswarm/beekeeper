package harness

import (
	"context"
	"fmt"
	"sort"
)

// Tool is a step tool (contract §4.2). Args lists the allowed argument names: a step with any other
// argument fails to load, so nothing passes vacuously. Run returns the JSON-able result the
// expectations are evaluated against.
type Tool struct {
	Args []string
	Run  func(ctx context.Context, w *World, args map[string]any) (any, error)
}

var registry = map[string]Tool{}

// Register adds a tool; registering a name twice panics.
func Register(name string, t Tool) {
	if _, dup := registry[name]; dup {
		panic(fmt.Sprintf("harness: tool %q registered twice", name))
	}
	registry[name] = t
}

// Lookup returns a registered tool.
func Lookup(name string) (Tool, bool) {
	t, ok := registry[name]
	return t, ok
}

// Tools returns the registered tool names, sorted.
func Tools() []string {
	names := make([]string, 0, len(registry))
	for n := range registry {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}
