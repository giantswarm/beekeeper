package cmd

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"unicode/utf8"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/state/kube"
)

// conversationText is the longest message klaus-gateway posts into a
// conversation.
const conversationText = 12000

// conversation is klaus-gateway's POST /conversations body: a direct message
// to the person whose thread is bound to the agent, a reply in which calls
// Reply.Tool through muster as the person.
type conversation struct {
	Person string         `json:"person"`
	From   string         `json:"from"`
	Text   string         `json:"text"`
	Reply  toolInvocation `json:"reply"`
}

// open starts a conversation and returns the gateway's id of it.
func (g *gateway) open(ctx context.Context, c conversation) (string, error) {
	var receipt struct {
		ID string `json:"id"`
	}
	if err := g.do(ctx, "/conversations", c, &receipt); err != nil {
		return "", err
	}
	if receipt.ID == "" {
		return "", errors.New("klaus-gateway returned no conversation id")
	}
	return receipt.ID, nil
}

// say posts text into the conversation id.
func (g *gateway) say(ctx context.Context, id, text string) error {
	return g.do(ctx, "/conversations/"+url.PathEscape(id)+"/messages", map[string]string{"text": text}, nil)
}

// toolConverse posts the calling agent's message to its person: into the
// agent's conversation, or a new one when it has none or the gateway no
// longer holds it.
func toolConverse(c *call, req mcp.CallToolRequest) (any, error) {
	text, err := required(req, paramText, "the message")
	if err != nil {
		return nil, err
	}
	if n := utf8.RuneCountInString(text); n > conversationText {
		return nil, usageErr("text: %d characters, at most %d", n, conversationText)
	}
	if c.s.gw == nil {
		return nil, refused("no klaus-gateway is configured (serve.gateway): there is no Slack to converse in")
	}
	agent, err := c.ownAgent()
	if err != nil {
		return nil, err
	}
	c.concern = kube.RosterObject(agent.Party)
	address := agentAddress(*agent)
	id, opened := agent.Conversation, false
	if id != "" {
		err = c.s.gw.say(c.ctx, id, text)
		var ge *gatewayError
		if errors.As(err, &ge) && ge.status == http.StatusNotFound {
			id, err = "", nil // quiet too long: a new thread
		}
	}
	if err == nil && id == "" {
		id, err = c.s.gw.open(c.ctx, conversation{Person: c.who.Email, From: agent.Name + " on " + agent.Host, Text: text,
			Reply: toolInvocation{Tool: c.s.cfg.Serve.Gateway.SendTool, Arguments: map[string]any{"to": address}}})
		opened = err == nil
	}
	var ge *gatewayError
	if errors.As(err, &ge) && ge.status/100 == 4 {
		return nil, refused("%s: not posted: %s", address, ge.reason)
	}
	if err != nil {
		return nil, err
	}
	if opened {
		err = c.store.Update(func(st *state.State) ([]state.Event, error) {
			for i := range st.Agents {
				if agentAddress(st.Agents[i]) == address && !st.Agents[i].Done {
					st.Agents[i].Conversation = id
					return []state.Event{event(c.me, "agents.converse", "%s opened conversation %s with %s", address, id, c.who.Email)}, nil
				}
			}
			return nil, refused("%s left the roster", address)
		})
		if err != nil {
			return nil, err
		}
	}
	verb := "posted to"
	if opened {
		verb = "opened"
	}
	_, err = fmt.Fprintf(c.out, "converse: %s conversation %s with %s\n", verb, id, c.who.Email)
	return map[string]any{"conversation": id, "opened": opened}, err
}

// ownAgent is the calling agent's roster entry: registered, running, and
// the caller's own.
func (c *call) ownAgent() (*state.Agent, error) {
	if c.me.Host == "" || c.me.Name == c.who.Email {
		return nil, usageErr("agent and host are required: the calling agent, as agents_register put it on the roster")
	}
	st, err := c.app.store.Read()
	if err != nil {
		return nil, err
	}
	for i, a := range st.Agents {
		if strings.EqualFold(a.Name, c.me.Name) && a.Host == c.me.Host && !a.Done {
			if !strings.EqualFold(a.Person, c.who.Email) {
				return nil, refused("%s is %s's agent: only its person converses as it", agentAddress(a), a.Person)
			}
			return &st.Agents[i], nil
		}
	}
	return nil, refused("%s%s/%s is not on the roster: agents_register first", localPrefix, c.me.Host, c.me.Name)
}
