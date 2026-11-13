package gate

// LaneTurn is where a merge stands in its lane.
type LaneTurn struct {
	Lane     string   `json:"lane"`
	Merge    string   `json:"merge"`
	Phase    string   `json:"phase"`
	Position int      `json:"position"`
	Turn     bool     `json:"turn"`
	Ahead    []string `json:"ahead"`
}
