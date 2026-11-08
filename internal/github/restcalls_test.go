package github

import (
	"strings"
	"testing"
)

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
