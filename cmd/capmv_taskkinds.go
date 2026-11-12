package cmd

// KindBlock is a task kind at one of its caps (agents.kinds): the reason a
// start of one of its agents is refused.
type KindBlock struct {
	Kind   string `json:"kind"`
	Reason string `json:"reason"`
}

func (b KindBlock) Line() string { return "no start " + b.Kind + ": " + b.Reason }
