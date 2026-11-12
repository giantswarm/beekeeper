package cmd

import (
	"time"
)

type diskHolding struct {
	At   time.Time `json:"at"`
	Text string    `json:"text"`
}
