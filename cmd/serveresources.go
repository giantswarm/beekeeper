package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"

	"github.com/giantswarm/beekeeper/internal/feed"
	"github.com/giantswarm/beekeeper/internal/identity"
	"github.com/giantswarm/beekeeper/internal/mailbox"
	"github.com/giantswarm/beekeeper/internal/state"
)

// The resources beekeeper serve offers, each subscribable over
// subscriptions/listen: a change sends notifications/resources/updated
// with its URI to the streams subscribed to it, and the client reads it.
const (
	resourceScheme       = "beekeeper://"
	resourceEnvironments = resourceScheme + "environments/"
	resourceLanes        = resourceScheme + "lanes/"
	resourceNotes        = resourceScheme + "notes/"
	resourceMailbox      = resourceScheme + "mailbox/"
	resourceRoster       = resourceScheme + "roster"
	resourceFeed         = resourceScheme + "feed"
	// feedLength is how many of the most recent events the feed holds.
	feedLength = 200
)

// mayRead refuses a resource the caller may not read: the notes and the
// mailbox are their person's, the rest the organization's; the roster and
// the feed show each reader only what rosterScope lets them read.
func mayRead(who identity.Caller, uri string) error {
	switch {
	case uri == resourceRoster, uri == resourceFeed:
		return nil // each reader's share of them: rosterScope
	case strings.HasPrefix(uri, resourceEnvironments) && len(uri) > len(resourceEnvironments),
		strings.HasPrefix(uri, resourceLanes) && len(uri) > len(resourceLanes):
		return nil
	}
	for _, prefix := range []string{resourceNotes, resourceMailbox} {
		if person, ok := strings.CutPrefix(uri, prefix); ok {
			if mailbox.Key(person) != mailbox.Key(who.Email) {
				return fmt.Errorf("%s is %s's: only that person reads it", uri, person)
			}
			return nil
		}
	}
	return fmt.Errorf("%s is not a resource of beekeeper serve", uri)
}

// hub is the subscriptions/listen streams open on this instance.
type hub struct {
	mu        sync.Mutex
	listeners map[*listener]struct{}
	// dropped is called for a notification a full stream could not take.
	dropped func(who identity.Caller, uri string)
}

// listener is one subscriptions/listen stream: whose, the request id its
// notifications carry, and the URIs it subscribed.
type listener struct {
	who     identity.Caller
	session mcpserver.ClientSession
	id      any
	uris    map[string]bool
}

func newHub() *hub { return &hub{listeners: map[*listener]struct{}{}} }

// listen registers a stream until its request's context ends.
func (h *hub) listen(ctx context.Context, who identity.Caller, id any, uris []string) {
	session := mcpserver.ClientSessionFromContext(ctx)
	if session == nil {
		return
	}
	l := &listener{who: who, session: session, id: id, uris: map[string]bool{}}
	for _, u := range uris {
		l.uris[u] = true
	}
	h.mu.Lock()
	h.listeners[l] = struct{}{}
	h.mu.Unlock()
	go func() {
		<-ctx.Done()
		h.mu.Lock()
		delete(h.listeners, l)
		h.mu.Unlock()
	}()
}

// updated notifies every stream subscribed to uri.
func (h *hub) updated(uri string) {
	h.updatedWhere(func(_ identity.Caller, u string) bool { return u == uri })
}

// updatedWhere notifies every stream of each subscribed URI match accepts.
func (h *hub) updatedWhere(match func(who identity.Caller, uri string) bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for l := range h.listeners {
		for uri := range l.uris {
			if !match(l.who, uri) {
				continue
			}
			n := mcp.JSONRPCNotification{JSONRPC: mcp.JSONRPC_VERSION, Notification: mcp.Notification{
				Method: mcp.MethodNotificationResourceUpdated,
				Params: mcp.NotificationParams{Meta: map[string]any{mcp.MetaKeySubscriptionID: l.id}, AdditionalFields: map[string]any{"uri": uri}},
			}}
			select {
			case l.session.NotificationChannel() <- n:
			default:
				if h.dropped != nil {
					h.dropped(l.who, uri)
				}
			}
		}
	}
}

// subscriptionHooks authorizes each subscriptions/listen before it is
// served and registers the stream on the hub.
func (s *server) subscriptionHooks() *mcpserver.Hooks {
	hooks := &mcpserver.Hooks{}
	hooks.AddOnRequestInitialization(func(ctx context.Context, _ any, message any) error {
		raw, err := json.Marshal(message)
		if err != nil {
			return err
		}
		var req struct {
			Method string                        `json:"method"`
			Params mcp.SubscriptionsListenParams `json:"params"`
		}
		if json.Unmarshal(raw, &req) != nil || req.Method != string(mcp.MethodSubscriptionsListen) {
			return nil
		}
		who, ok := ctx.Value(callerKey{}).(identity.Caller)
		if !ok {
			return errors.New("no authenticated caller")
		}
		for _, uri := range req.Params.Notifications.ResourceSubscriptions {
			if err := mayRead(who, uri); err != nil {
				s.log.Info("subscribe", "caller", who.Email, "uri", uri, "outcome", "refused", "detail", err.Error())
				return err
			}
		}
		return nil
	})
	hooks.AddBeforeSubscriptionsListen(func(ctx context.Context, id any, req *mcp.SubscriptionsListenRequest) {
		who, _ := ctx.Value(callerKey{}).(identity.Caller)
		s.log.Info("subscribe", "caller", who.Email, "uris", req.Params.Notifications.ResourceSubscriptions, "outcome", "ok")
		s.hub.listen(ctx, who, id, req.Params.Notifications.ResourceSubscriptions)
	})
	return hooks
}

// addResources registers the resources and their readers.
func (s *server) addResources(m *mcpserver.MCPServer) {
	m.AddResource(mcp.NewResource(resourceRoster, "roster", mcp.WithResourceDescription("The agent roster: your own local agents, your team's that work on a shared installation, and every remote agent."), mcp.WithMIMEType("application/json")),
		s.read(func(_ context.Context, who identity.Caller, _ string) (any, error) { return s.roster(who) }))
	m.AddResource(mcp.NewResource(resourceFeed, "feed", mcp.WithResourceDescription("The most recent events of the shared state you may read, oldest first, schema "+feed.Schema+"."), mcp.WithMIMEType("application/json")),
		s.read(func(_ context.Context, who identity.Caller, _ string) (any, error) { return s.feed(who) }))
	m.AddResourceTemplate(mcp.NewResourceTemplate(resourceEnvironments+"{name}", "environment", mcp.WithTemplateDescription("An installation's Environment: its holder, grants and upgrades."), mcp.WithTemplateMIMEType("application/json")),
		s.readTemplate(func(_ context.Context, _ identity.Caller, uri string) (any, error) {
			return s.environment(strings.TrimPrefix(uri, resourceEnvironments))
		}))
	m.AddResourceTemplate(mcp.NewResourceTemplate(resourceLanes+"{name}", "lane", mcp.WithTemplateDescription("A merge lane: the running merge, the settling one and the queue."), mcp.WithTemplateMIMEType("application/json")),
		s.readTemplate(func(_ context.Context, _ identity.Caller, uri string) (any, error) {
			return s.lane(strings.TrimPrefix(uri, resourceLanes))
		}))
	m.AddResourceTemplate(mcp.NewResourceTemplate(resourceNotes+"{+person}", "notes", mcp.WithTemplateDescription("Your notes: those for you or your team, and those you filed. Only yours."), mcp.WithTemplateMIMEType("application/json")),
		s.readTemplate(func(_ context.Context, who identity.Caller, _ string) (any, error) { return s.notesOf(who) }))
	m.AddResourceTemplate(mcp.NewResourceTemplate(resourceMailbox+"{+person}", "mailbox", mcp.WithTemplateDescription("Your mailbox: how many messages wait; receive_messages returns them. Only yours."), mcp.WithTemplateMIMEType("application/json")),
		s.readTemplate(func(ctx context.Context, who identity.Caller, _ string) (any, error) { return s.mailboxOf(ctx, who) }))
}

// readTemplate is read for a resource template.
func (s *server) readTemplate(fill func(ctx context.Context, who identity.Caller, uri string) (any, error)) mcpserver.ResourceTemplateHandlerFunc {
	return mcpserver.ResourceTemplateHandlerFunc(s.read(fill))
}

// read is a resource's handler: the caller may read it, and its content is
// fill's view as JSON.
func (s *server) read(fill func(ctx context.Context, who identity.Caller, uri string) (any, error)) mcpserver.ResourceHandlerFunc {
	return func(ctx context.Context, req mcp.ReadResourceRequest) ([]mcp.ResourceContents, error) {
		who, ok := ctx.Value(callerKey{}).(identity.Caller)
		if !ok {
			return nil, errors.New("no authenticated caller")
		}
		uri := req.Params.URI
		if err := mayRead(who, uri); err != nil {
			s.log.Info("read", "caller", who.Email, "uri", uri, "outcome", "refused", "detail", err.Error())
			return nil, err
		}
		v, err := fill(ctx, who, uri)
		if err != nil {
			return nil, err
		}
		b, err := json.Marshal(v)
		if err != nil {
			return nil, err
		}
		return []mcp.ResourceContents{mcp.TextResourceContents{URI: uri, MIMEType: "application/json", Text: string(b)}}, nil
	}
}

// addressedAgent is a roster entry as the resource and list_agents show it,
// named by its address as the messaging plan's tools name it.
type addressedAgent struct {
	Address    string    `json:"address"`
	Name       string    `json:"name"`
	Person     string    `json:"person,omitempty"`
	Team       string    `json:"team,omitempty"`
	Host       string    `json:"host,omitempty"`
	State      string    `json:"state"`
	Task       string    `json:"task,omitempty"`
	Registered time.Time `json:"registered"`
	IdleSince  time.Time `json:"idleSince,omitzero"`
}

func addressed(a state.Agent) addressedAgent {
	return addressedAgent{Address: agentAddress(a), Name: a.Name, Person: a.Person, Team: a.Team, Host: a.Host,
		State: agentState(a), Task: a.Task, Registered: a.Registered, IdleSince: a.IdleSince}
}

func agentAddress(a state.Agent) string { return localPrefix + a.Host + "/" + a.Name }

func agentState(a state.Agent) string {
	switch {
	case a.Done:
		return "ended"
	case a.Task != "":
		return "busy"
	}
	return "idle"
}

func (s *server) roster(who identity.Caller) (map[string]any, error) {
	agents, err := s.agentsFor(who, false)
	return map[string]any{agentsName: agents}, err
}

// agentsFor is the roster as who reads it, only their team's with team.
func (s *server) agentsFor(who identity.Caller, team bool) ([]addressedAgent, error) {
	sc, err := s.scope(who)
	if err != nil {
		return nil, err
	}
	st, err := s.store.Read()
	if err != nil {
		return nil, err
	}
	agents := []addressedAgent{}
	for _, a := range st.Agents {
		if ag := addressed(a); sc.readsAgent(ag) && (!team || ag.Team == who.Team) {
			agents = append(agents, ag)
		}
	}
	return agents, nil
}

// feed is the most recent events who reads, at most feedLength.
func (s *server) feed(who identity.Caller) (*feed.Feed, error) {
	sc, err := s.scope(who)
	if err != nil {
		return nil, err
	}
	st, err := s.store.Read()
	if err != nil {
		return nil, err
	}
	evs, err := s.store.Feed(0)
	if err != nil {
		return nil, err
	}
	out := []feed.Event{}
	for _, ev := range evs {
		if sc.readsEvent(ev, st.Agents) {
			out = append(out, ev)
		}
	}
	if len(out) > feedLength {
		out = out[len(out)-feedLength:]
	}
	return &feed.Feed{Schema: feed.Schema, Events: out}, nil
}

// record is a resource's view of an Environment or MergeLane.
type record struct {
	Name   string `json:"name"`
	Spec   any    `json:"spec"`
	Status any    `json:"status"`
}

func (s *server) environment(name string) (*record, error) {
	envs, err := s.store.Environments()
	if err != nil {
		return nil, err
	}
	for _, e := range envs {
		if e.Name == name {
			return &record{Name: e.Name, Spec: e.Spec, Status: e.Status}, nil
		}
	}
	return nil, fmt.Errorf("%s is not an Environment on this installation", name)
}

func (s *server) lane(name string) (*record, error) {
	ls, err := s.store.Lanes()
	if err != nil {
		return nil, err
	}
	for _, l := range ls {
		if l.Name == name {
			return &record{Name: l.Name, Spec: l.Spec, Status: l.Status}, nil
		}
	}
	return nil, fmt.Errorf("%s is not a merge lane on this installation", name)
}

// concerns reports whether a note is the person's: for them or their team,
// or filed by them.
func concerns(who identity.Caller, forWhom, byPerson string) bool {
	return strings.EqualFold(forWhom, who.Email) || (who.Team != "" && forWhom == "team:"+who.Team) ||
		(byPerson != "" && strings.EqualFold(byPerson, who.Email))
}

func (s *server) notesOf(who identity.Caller) (map[string]any, error) {
	st, err := s.store.Read()
	if err != nil {
		return nil, err
	}
	notes := []state.Note{}
	for _, n := range st.Notes {
		if concerns(who, n.For, n.By.Person) {
			notes = append(notes, n)
		}
	}
	return map[string]any{"person": who.Email, keyNotes: notes}, nil
}

func (s *server) mailboxOf(ctx context.Context, who identity.Caller) (map[string]any, error) {
	n, err := s.mail.Pending(ctx, who.Email, s.now())
	if err != nil {
		return nil, err
	}
	return map[string]any{keyMailbox: mailbox.Key(who.Email), "pending": n, "cap": mailbox.Cap}, nil
}
