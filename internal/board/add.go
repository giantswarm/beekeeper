package board

import (
	"context"
	"fmt"

	"github.com/giantswarm/beekeeper/internal/github"
)

// Fields describes an issue's item on the configured project.
type Fields struct {
	Ref     string `json:"ref"`
	Item    string `json:"item,omitempty"`
	OnBoard bool   `json:"onBoard"`
	Status  string `json:"status"`
	Team    string `json:"team"`
	Kind    string `json:"kind"`
}

func fieldsOf(ref string, item *projectItem) *Fields {
	out := &Fields{Ref: ref}
	if item == nil {
		return out
	}
	out.OnBoard, out.Item = true, item.ID
	if item.Status != nil {
		out.Status = item.Status.Name
	}
	if item.Team != nil {
		out.Team = item.Team.Name
	}
	if item.Kind != nil {
		out.Kind = item.Kind.Name
	}
	return out
}

// Show reads only the issue's projectItems, never the project's items.
func (c *Client) Show(ctx context.Context, ref string) (*Fields, error) {
	owner, repo, n, err := Ref(ref)
	if err != nil {
		return nil, err
	}
	ctx = github.ActingOn(ctx, owner+"/"+repo)
	m, err := c.meta(ctx)
	if err != nil {
		return nil, err
	}
	is, err := c.lookup(ctx, m, ref, owner, repo, n)
	if err != nil {
		return nil, err
	}
	return fieldsOf(ref, is.Found), nil
}

// Add ensures the issue is on the board and sets the requested fields.
// Empty status means Backlog; an omitted Kind preserves the current value.
func (c *Client) Add(ctx context.Context, ref, status, kind string) (*Fields, error) {
	owner, repo, n, err := Ref(ref)
	if err != nil {
		return nil, err
	}
	ctx = github.ActingOn(ctx, owner+"/"+repo)
	m, err := c.meta(ctx)
	if err != nil {
		return nil, err
	}
	if status == "" {
		status = "backlog"
	}
	sf := m.Fields[StatusField]
	status, err = Resolve(StatusField, sf.names(), status)
	if err != nil {
		return nil, &Refusal{err.Error()}
	}
	team, err := c.team(m)
	if err != nil {
		return nil, &Refusal{err.Error()}
	}
	options := []option{{field: sf.ID, id: sf.option(status), name: status}}
	if team != nil {
		options = append(options, *team)
	}
	if kind != "" {
		kf := m.Fields[KindField]
		kind, err = Resolve(KindField, kf.names(), kind)
		if err != nil {
			return nil, &Refusal{err.Error()}
		}
		options = append(options, option{field: kf.ID, id: kf.option(kind), name: kind})
	}
	ref = fmt.Sprintf("%s/%s#%d", owner, repo, n)
	item, err := c.item(ctx, m, &Moved{Ref: ref}, owner, repo, n)
	if err != nil {
		return nil, err
	}
	out := fieldsOf(ref, &item)
	for _, opt := range options {
		if err := c.set(ctx, m.ID, item.ID, opt.field, opt.id); err != nil {
			return nil, err
		}
	}
	out.Status = status
	if team != nil {
		out.Team = team.name
	}
	if kind != "" {
		out.Kind = kind
	}
	return out, nil
}
