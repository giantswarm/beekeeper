package github

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestRESTAdmissionStopsBeforeExecution(t *testing.T) {
	previous := RESTObserver
	t.Cleanup(func() { RESTObserver = previous })
	observed := 0
	RESTObserver = func(string) { observed++ }
	refused := errors.New("background allowance spent")
	ctx := WithRESTAdmission(t.Context(), func(context.Context, string) error { return refused })
	// The refusal returns before looking for gh, retrying, or recording an
	// invocation. An unrelated worker context does not inherit this gate.
	_, err := Retrying(execGH, 0)(ctx, ghAPI, reposPrefix+cacheTestRepo)
	if !errors.Is(err, refused) || observed != 0 {
		t.Fatalf("refused call: %v, observed=%d", err, observed)
	}
	if err := AdmitRESTCall(t.Context(), "worker"); err != nil || observed != 1 {
		t.Fatalf("independent worker: %v, observed=%d", err, observed)
	}
}

func TestRESTObserverNamesSourceWithoutArguments(t *testing.T) {
	previous := RESTObserver
	t.Cleanup(func() { RESTObserver = previous })
	var classes []string
	RESTObserver = func(class string) { classes = append(classes, class) }
	observeREST()
	ObserveRESTCall("update.release")
	if len(classes) != 2 || !strings.Contains(classes[0], "TestRESTObserverNamesSourceWithoutArguments") || classes[1] != "update.release" {
		t.Fatalf("REST sources: %v", classes)
	}
}
