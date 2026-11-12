package cmd

type deliveryFlow struct {
	Class       string `json:"class"`
	Reason      string `json:"reason,omitempty"`
	MissingNote bool   `json:"missingNote,omitempty"`
}
