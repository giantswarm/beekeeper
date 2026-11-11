package github

import (
	"context"
	"runtime"
	"strings"
)

// RESTObserver records an invocation, not measured request cost: gh may
// paginate or retry. Set once during command startup, before workers start.
// Arguments and URLs are deliberately excluded.
var RESTObserver func(string)

type restAdmissionKey struct{}

// WithRESTAdmission bounds background traffic before execution. Attach it to
// the command context so separate commands and worker contexts stay isolated.
func WithRESTAdmission(ctx context.Context, admit func(context.Context, string) error) context.Context {
	return context.WithValue(ctx, restAdmissionKey{}, admit)
}

// AdmitRESTCall checks the background allowance and records only admitted calls.
// The class must be a fixed source name, never an argument or credential.
func AdmitRESTCall(ctx context.Context, class string) error {
	if admit, ok := ctx.Value(restAdmissionKey{}).(func(context.Context, string) error); ok {
		if err := admit(ctx, class); err != nil {
			return err
		}
	}
	ObserveRESTCall(class)
	return nil
}

func observeREST() {
	ObserveRESTCall(restCaller())
}

func restCaller() string {
	pcs := make([]uintptr, 24)
	n := runtime.Callers(2, pcs)
	frames := runtime.CallersFrames(pcs[:n])
	for {
		frame, more := frames.Next()
		name := strings.TrimPrefix(frame.Function, "github.com/giantswarm/beekeeper/")
		if name != frame.Function && !strings.HasPrefix(name, "internal/github.execGH") && !strings.HasPrefix(name, "internal/github.tryRead") && !strings.HasPrefix(name, "internal/github.Retrying") && !strings.HasPrefix(name, "internal/github.(*repositoryCache)") && name != "internal/github.observeREST" {
			return name
		}
		if !more {
			return "beekeeper REST"
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
