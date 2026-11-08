package github

import (
	"runtime"
	"strings"
)

// RESTObserver records an invocation, not measured request cost: gh may
// paginate or retry. Set once during command startup, before workers start.
// Arguments and URLs are deliberately excluded.
var RESTObserver func(string)

func observeREST() {
	if RESTObserver == nil {
		return
	}
	pcs := make([]uintptr, 24)
	n := runtime.Callers(2, pcs)
	frames := runtime.CallersFrames(pcs[:n])
	for {
		frame, more := frames.Next()
		name := strings.TrimPrefix(frame.Function, "github.com/giantswarm/beekeeper/")
		if name != frame.Function && !strings.HasPrefix(name, "internal/github.execGH") && !strings.HasPrefix(name, "internal/github.tryRead") && !strings.HasPrefix(name, "internal/github.Retrying") {
			RESTObserver(name)
			return
		}
		if !more {
			RESTObserver("beekeeper REST")
			return
		}
	}
}

// ObserveRESTCall records a fixed source class for a native HTTP client.
// Callers must never pass a URL, command argument or credential here.
func ObserveRESTCall(class string) {
	if RESTObserver != nil {
		RESTObserver(class)
	}
}
