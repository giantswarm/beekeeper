package watch

// CheckResult is one check of a timer's condition: whether it holds and, if
// not, why, or why its reference could not be read or its probe not run.
type CheckResult struct {
	Holds, Unreadable bool
	Reason            string
}
