package cmd

// kindBlock is a task kind at one of its caps (agents.kinds): the reason a
// start of one of its agents is refused.
type kindBlock struct {
	Kind   string `json:"kind"`
	Reason string `json:"reason"`
}

func (b kindBlock) line() string { return "no start " + b.Kind + ": " + b.Reason }
