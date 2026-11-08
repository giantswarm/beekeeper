package github

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

const (
	VisibilityPublic   = "public"
	VisibilityPrivate  = "private"
	VisibilityInternal = "internal"
)

// VisibilityPolicy restricts selected owners to repositories proven public.
// Missing readers, unresolved targets and unreadable metadata fail closed.
// It is independent of the owner allowlist: disabling that list must not
// disable an explicitly configured visibility restriction.
type VisibilityPolicy struct {
	PublicOnlyOwners []string
	Read             func(string) (string, error)
}

func (p VisibilityPolicy) Restricted(owner string) bool {
	return slices.ContainsFunc(p.PublicOnlyOwners, func(o string) bool { return strings.EqualFold(o, owner) })
}

func (p VisibilityPolicy) Check(repo string) error {
	owner, name, ok := strings.Cut(repo, "/")
	if len(p.PublicOnlyOwners) > 0 && (!ok || owner == "") {
		return fmt.Errorf("cannot resolve repository visibility")
	}
	if !p.Restricted(owner) {
		return nil
	}
	deny := func() error {
		return fmt.Errorf("repository %s refused: github.publicOnlyOwners requires verified public visibility", repo)
	}
	if !ok || name == "" || name == "*" || strings.ContainsAny(name, "/$`?#\\") || p.Read == nil {
		return deny()
	}
	visibility, err := p.Read(repo)
	if err != nil || visibility != VisibilityPublic {
		return deny()
	}
	return nil
}

// RepositoryVisibility reads fresh metadata. A missing visibility field is not
// public; in particular a zero-valued private boolean cannot grant access.
func RepositoryVisibility(ctx context.Context, run GH, repo string) (string, error) {
	raw, err := run(ActingOn(ctx, repo), ghAPI, "repos/"+repo)
	if err != nil {
		return "", err
	}
	var metadata struct {
		Visibility string `json:"visibility"`
	}
	if err := json.Unmarshal(raw, &metadata); err != nil {
		return "", err
	}
	switch metadata.Visibility {
	case VisibilityPublic, VisibilityPrivate, VisibilityInternal:
		return metadata.Visibility, nil
	default:
		return "", fmt.Errorf("repository visibility unreadable")
	}
}
