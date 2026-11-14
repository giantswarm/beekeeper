package cmd

// Because appends a named cause to a memory line.
func Because(cause string) string {
	if cause == "" {
		return ""
	}
	return "; cause: " + cause
}
