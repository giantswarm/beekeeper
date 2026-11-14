package cmd

// checkResult is one check of a timer's condition: whether it holds and, if
// not, why, or why its reference could not be read or its probe not run.
type checkResult struct {
	holds, unreadable bool
	reason            string
}
