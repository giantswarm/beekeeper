package cmd

// Capacity calls the command through Cmd.
func init() {
	Cmd = Command{
		Clock:       clock,
		Dur:         dur,
		EffortText:  effortText,
		Gib:         gib,
		OmpProvider: ompProvider,
		Truncate:    truncate,
	}
}
