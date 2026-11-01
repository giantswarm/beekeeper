package cmd

import "testing"

// The normal remote worker brief requires a branch and PR before it can park
// for review. This component command must exist even without host credentials.
func TestExecutionRemotePublicationSurface(t *testing.T) {
	a, _ := executionTestApp(t)
	for _, c := range a.executionHostCmds() {
		if c.Name() == "publish" {
			if c.Flags().Lookup("body-file") == nil || c.Flags().Lookup("sync-base") == nil {
				t.Fatal("remote publication lacks PR or base synchronization support")
			}
			return
		}
	}
	t.Fatal("remote workers have no supported branch and pull request publication command")
}
