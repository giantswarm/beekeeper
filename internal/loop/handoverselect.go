package loop

import (
	"time"
)

// answered is a person's answer recorded: the answer.recorded event read
// back; Comment is its comment's URL, "" when none was posted. ID is the note it answered, 0 for none.
type Answered struct {
	About    string
	Comment  string
	ID       int
	For      string
	Via      string
	Answer   string
	Asker    string
	Question string
	Audience string // original note audience; empty in older audit events.
	At       time.Time
	By       string
}
