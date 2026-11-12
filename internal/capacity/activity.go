package capacity

// Counts cover the last 24 hours, including retired workers. Starts count
// session identities, not the several agents.start lines one launch writes.
type ProviderActivity struct {
	Wakes       int `json:"wakes"`
	Resumes     int `json:"resumes"`
	FreshStarts int `json:"freshStarts"`
}
