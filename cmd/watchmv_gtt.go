package cmd

// because appends a named cause to a memory line.
func because(cause string) string {
	if cause == "" {
		return ""
	}
	return "; cause: " + cause
}
