// Package decision holds the decision points of phase 1 (contract §1.4): their rules, the inputs hash
// and the outcome of comparing JEV's answer with the rule's. The rule always decides in shadow; JEV's
// answer is recorded next to it.
package decision

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

// Decision points.
const (
	PointReady        = "ready?"
	PointNext         = "next?"
	PointParallelSafe = "parallel-safe?"
	PointBugSeverity  = "bug-severity?"
)

// Outcomes.
const (
	OutcomeAgree         = "agree"
	OutcomeDisagree      = "disagree"
	OutcomeJEVError      = "jev_error"
	OutcomeLowConfidence = "jev_low_confidence"
)

// Question is a decision a decide function made: the rule's answer and, in mode shadow, a question for
// JEV. The engine records it once per (Point, Subject, InputsHash) and asks JEV once for that key.
type Question struct {
	Point   string
	Subject string // an item's bare ref or a person's login
	Other   string // parallel-safe?'s other item
	Team    string
	// Inputs are what the question is about (never who it is addressed to); they are recorded and hashed.
	Inputs     map[string]any
	InputsHash string
	// Rule is the rule's answer, which is the outcome in every phase 1 mode.
	Rule    string
	Answers []string
	// Mode is shadow (ask JEV) or rule (don't).
	Mode string
	// Fields are what JEV gets: only what the question needs.
	Fields map[string]any
}

// Hash returns the canonical hash of inputs (sorted-key JSON, sha256, hex).
func Hash(inputs map[string]any) string {
	b, err := json.Marshal(inputs) // encoding/json sorts map keys
	if err != nil {
		panic(err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// Outcome compares JEV's answer with the rule's.
func Outcome(rule, jev string, confidence float64, jevErr error, floor float64) string {
	switch {
	case jevErr != nil:
		return OutcomeJEVError
	case confidence < floor:
		return OutcomeLowConfidence
	case jev == rule:
		return OutcomeAgree
	default:
		return OutcomeDisagree
	}
}
