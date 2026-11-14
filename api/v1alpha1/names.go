package v1alpha1

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
)

// LabelTeam labels engine-owned objects with their team.
const LabelTeam = "beekeeper.giantswarm.io/team"

// Ref is a short GitHub reference: owner/repo#n for issues and PRs.
type Ref struct {
	Owner  string
	Repo   string
	Number int
}

// ParseRef parses owner/repo#n.
func ParseRef(s string) (Ref, error) {
	repo, num, ok := strings.Cut(s, "#")
	owner, name, ok2 := strings.Cut(repo, "/")
	n, err := strconv.Atoi(num)
	if !ok || !ok2 || owner == "" || name == "" || err != nil || n <= 0 {
		return Ref{}, fmt.Errorf("invalid ref %q, want owner/repo#n", s)
	}
	return Ref{Owner: owner, Repo: name, Number: n}, nil
}

// FullRepo is owner/repo.
func (r Ref) FullRepo() string { return r.Owner + "/" + r.Repo }

func (r Ref) String() string { return fmt.Sprintf("%s/%s#%d", r.Owner, r.Repo, r.Number) }

// ItemName is the object name of the Item for an issue ref: giantswarm/roadmap#101 → giantswarm-roadmap-101.
// Refs the readable form can't tell apart get a hash suffix (see objectName).
func ItemName(ref string) string { return objectName(ref, "") }

// ChangeName is the object name of the Change for a PR ref: giantswarm/klaus#55 → giantswarm-klaus-pr-55.
func ChangeName(ref string) string { return objectName(ref, "pr-") }

// PersonName is the object name of the Person for a login.
func PersonName(login string) string { return sanitize(login) }

// objectName is owner-repo-[infix]n in lower case. GitHub names are case-insensitive, so that is exact
// while the owner has no '-' and the repo only letters, digits and '-': the owner ends at the first '-'
// and the number starts after the last. Other refs (foo.bar and foo_bar both read foo-bar, an owner
// with a '-' shifts the split) get "-h" and a hash of the ref, which no exact name ends in (its last
// segment is the number).
func objectName(ref, infix string) string {
	r, err := ParseRef(ref)
	if err != nil {
		return sanitize(ref)
	}
	owner, repo := strings.ToLower(r.Owner), strings.ToLower(r.Repo)
	name := sanitize(owner) + "-" + sanitize(repo) + "-" + infix + strconv.Itoa(r.Number)
	if strings.Contains(owner, "-") || sanitize(owner) != owner || sanitize(repo) != repo {
		sum := sha256.Sum256([]byte(owner + "/" + repo + "#" + strconv.Itoa(r.Number)))
		name += "-h" + hex.EncodeToString(sum[:4])
	}
	return name
}

func sanitize(s string) string {
	var b strings.Builder
	for _, c := range strings.ToLower(s) {
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
			b.WriteRune(c)
		default:
			b.WriteByte('-')
		}
	}
	return strings.Trim(b.String(), "-")
}
