package cmd

import (
	"regexp"
	"strings"

	"github.com/Masterminds/semver/v3"
)

var releaseTarget = regexp.MustCompile(`^([\w.-]+/[\w.-]+)@([^\s@]+)$`)

// parseReleaseTarget retains the tag spelling: v1.2.3 and 1.2.3 can name
// different tags. Only explicit versions are accepted, never ranges.
func parseReleaseTarget(on string) (repo, version string, ok bool) {
	m := releaseTarget.FindStringSubmatch(on)
	if m == nil {
		return "", "", false
	}
	if _, err := semver.StrictNewVersion(strings.TrimPrefix(m[2], "v")); err != nil {
		return "", "", false
	}
	return m[1], m[2], true
}
