package cmd

import "testing"

// TestValidateProgressTaskIgnoresCase checks that the stalled item's pace
// exception admits the item's own brief whatever case GitHub gives its
// repository: a board ref keeps that case and namedRefs lower-cases.
//
// control: none: validateProgressTask is new in this pull request, so the base cannot build it
func TestValidateProgressTaskIgnoresCase(t *testing.T) {
	a := &app{progressItem: "Example/App#1"}
	if err := a.validateProgressTask("Continue https://github.com/Example/App/issues/1 now."); err != nil {
		t.Fatalf("the item's own brief was refused: %v", err)
	}
	if err := a.validateProgressTask("Continue https://github.com/Example/App/issues/2 now."); err == nil {
		t.Fatal("another item's brief was admitted")
	}
}
