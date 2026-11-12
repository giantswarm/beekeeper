//go:build scenario

// Package fakegithub is the GitHub of a behavioral scenario: a model of
// repositories, issues, pull requests, reviews, comments, checks, workflow
// runs, releases, labels, sub-issues and a Projects v2 board, served as
// GitHub's REST and GraphQL APIs by one http.Handler.
//
// Everything that reaches GitHub in a scenario reaches that handler: code
// that calls HTTP directly through the API base variables (Env), and the
// fake gh, a translator that turns gh api, gh pr, gh issue, gh run and gh
// release into requests to it and renders gh's output and exit codes (GH).
// So budgets, ETags, faults and the call log hold for both.
//
// The handler keeps core, search and GraphQL budgets with their
// X-RateLimit headers and records what every caller spent, answers
// conditional reads with 304 Not Modified without spending, and applies
// the scenario's faults. A request for something the model does not have
// (a REST route, a GraphQL field, a gh flag) is recorded as unsupported and
// fails the scenario: it is never answered with a guess.
//
// See docs/scenarios-github.md.
package fakegithub
