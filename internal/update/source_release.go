//go:build !scenario

package update

// apiBase is the GitHub API the release lookup and the downloads go to; ""
// is api.github.com. A release build has no other: where its releases come
// from is not the environment's to say.
func apiBase() string { return "" }
