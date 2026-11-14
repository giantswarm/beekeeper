// Package jev asks JEV (TypeSafe's systemone) for its answer to a decision point, in shadow.
package jev

import "context"

// Request is a question for JEV. Fields carry only what the question needs.
type Request struct {
	Point   string
	Subject string
	// Answers are the allowed answers.
	Answers []string
	Fields  map[string]any
	// Rule is the rule's answer. It is never sent to JEV; fakes use it to agree with the rule.
	Rule string
}

// Answer is JEV's answer.
type Answer struct {
	Answer     string
	Confidence float64
}

// Client asks JEV. Errors and timeouts are returned as errors; the caller records them as jev_error.
type Client interface {
	Ask(ctx context.Context, r Request) (Answer, error)
}
