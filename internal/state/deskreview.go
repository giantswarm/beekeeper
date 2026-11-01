package state

import "time"

// DeskReview binds an independent round to the concrete sessions the
// coordinating component started. A display name alone is never a credential.
type DeskReview struct {
	ID         string            `json:"id"`
	Ref        string            `json:"ref"`
	Head       string            `json:"head"`
	Round      int               `json:"round"`
	Worker     Party             `json:"worker"`
	Reviewer   Party             `json:"reviewer"`
	AssignedAt time.Time         `json:"assignedAt"`
	Result     *DeskReviewResult `json:"result,omitempty"`
}

// DeskReviewResult authenticates a specific public review through the assigned
// reviewer's durable completion report, not through their shared GitHub login.
type DeskReviewResult struct {
	ID           string    `json:"id"`
	URL          string    `json:"url"`
	Author       string    `json:"author"`
	Verdict      string    `json:"verdict"`
	BodyDigest   string    `json:"bodyDigest"`
	ReportDigest string    `json:"reportDigest"`
	ReportedAt   time.Time `json:"reportedAt"`
	By           Party     `json:"by"`
}
