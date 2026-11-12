package loop

import (
	"time"
)

type DiskHolding struct {
	At   time.Time `json:"at"`
	Text string    `json:"text"`
}
