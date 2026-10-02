package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"

	"github.com/giantswarm/beekeeper/internal/mailbox"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/state/kube"
	"github.com/giantswarm/beekeeper/pkg/project"
)

// The messaging tools: a person's mailbox for the local agents the person
// federates, and kagent sessions reached through muster.
func (s *server) messageTools() []serveTool {
	return []serveTool{
		{newTool("send_message", "Send an A2A message to an agent: local:<machine>/<name>, one of your own local agents, into your mailbox; kagent:<installation>/<namespace>/<session> through muster as you.", false,
			mcp.WithString("to", mcp.Required(), mcp.Description("the address: local:<machine>/<name> or kagent:<installation>/<namespace>/<session>")),
			mcp.WithObject(paramMessage, mcp.Required(), mcp.Description("the A2A Message: messageId, role and parts")),
			mcp.WithString("deadline", mcp.Description("when an unacked local: message expires: an RFC 3339 time or a duration (2h); default 24h"))), toolSendMessage},
		{newTool("receive_messages", "The oldest unacked messages of your mailbox, in order; ack them with ack_messages.", true,
			mcp.WithString(paramFor, mcp.Description("the mailbox: your email (default)")),
			mcp.WithNumber("limit", mcp.Description(fmt.Sprintf("at most this many (default and most: %d)", mailbox.Cap)))), toolReceiveMessages},
		{newTool("ack_messages", "Ack received messages of your mailbox by their ids: they are not delivered again.", false,
			mcp.WithString(paramFor, mcp.Description("the mailbox: your email (default)")),
			mcp.WithArray("ids", mcp.Required(), mcp.WithNumberItems(), mcp.Description("the ids receive_messages returned"))), toolAckMessages},
		{newTool("converse", "Write to your person in Slack as the calling agent, in its conversation thread (the first message opens it); the person's replies there arrive in your mailbox from source slack.", false,
			mcp.WithString(paramText, mcp.Required(), mcp.Description(fmt.Sprintf("the message, Slack markdown of at most %d characters", conversationText)))), toolConverse},
	}
}

// ownMailbox is the mailbox a call names in for: only the caller's own.
func (c *call) ownMailbox(req mcp.CallToolRequest) (string, error) {
	mb := mailbox.Key(req.GetString(paramFor, c.who.Email))
	if mb != mailbox.Key(c.who.Email) {
		return "", refused("the mailbox of %s is that person's: you read and ack only your own (%s)", mb, mailbox.Key(c.who.Email))
	}
	return mb, nil
}

// a2aMessage is what send_message reads of an A2A Message; the rest
// travels as it came.
type a2aMessage struct {
	MessageID string `json:"messageId"`
	Role      string `json:"role"`
	Parts     []struct {
		Kind string `json:"kind"`
		Text string `json:"text"`
	} `json:"parts"`
	// Metadata.Source is where the person wrote it: slack for a reply in a
	// conversation's thread.
	Metadata struct {
		Source string `json:"source"`
	} `json:"metadata"`
}

// text is the message's text parts, one per line.
func (m a2aMessage) text() string {
	var parts []string
	for _, p := range m.Parts {
		if p.Text != "" {
			parts = append(parts, p.Text)
		}
	}
	return strings.Join(parts, "\n")
}

func toolSendMessage(c *call, req mcp.CallToolRequest) (any, error) {
	to, err := required(req, "to", "the address, local:<machine>/<name> or kagent:<installation>/<namespace>/<session>")
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(req.GetArguments()[paramMessage])
	if err != nil {
		return nil, err
	}
	var msg a2aMessage
	if err := json.Unmarshal(raw, &msg); err != nil || string(raw) == "null" {
		return nil, usageErr("message is required: an A2A Message object")
	}
	if strings.TrimSpace(msg.MessageID) == "" || len(msg.Parts) == 0 {
		return nil, usageErr("message: an A2A Message has a messageId and parts")
	}
	switch {
	case strings.HasPrefix(to, "local:"):
		return c.sendLocal(req, to, msg, raw)
	case strings.HasPrefix(to, "kagent:"):
		return c.sendKagent(to, msg)
	}
	return nil, usageErr("to %q: local:<machine>/<name> or kagent:<installation>/<namespace>/<session>", to)
}

// sendLocal queues the message in the mailbox of the agent's person, who
// alone messages their local agents.
func (c *call) sendLocal(req mcp.CallToolRequest, to string, msg a2aMessage, raw json.RawMessage) (any, error) {
	sc, err := c.s.scope(c.who)
	if err != nil {
		return nil, err
	}
	st, err := c.app.store.Read()
	if err != nil {
		return nil, err
	}
	var agent *state.Agent
	for i, a := range st.Agents {
		if strings.EqualFold(agentAddress(a), to) && !a.Done {
			agent = &st.Agents[i]
			break
		}
	}
	if agent == nil || agent.Person == "" || !sc.reads(agent.Party) {
		return nil, refused("%s is not running: it is not on the roster", to)
	}
	if !sc.own(agent.Party) {
		return nil, refused("%s is %s's local agent: only its person messages it", to, agent.Person)
	}
	c.concern = kube.RosterObject(agent.Party)
	deadline, err := deadlineArg(c.app.now, req.GetString("deadline", ""))
	if err != nil {
		return nil, err
	}
	from := map[string]string{"person": c.who.Email, "agent": c.me.Name, "host": c.me.Host}
	if msg.Metadata.Source != "" {
		from["source"] = msg.Metadata.Source
	}
	env, err := json.Marshal(map[string]any{"to": agentAddress(*agent), "from": from, paramMessage: raw})
	if err != nil {
		return nil, err
	}
	sent, err := c.s.mail.Send(c.ctx, mailbox.Message{Mailbox: agent.Person, Sender: c.who.Email, SenderTeam: c.who.Team, ID: msg.MessageID, Envelope: env, Deadline: deadline}, c.app.now)
	if errors.Is(err, mailbox.ErrFull) {
		return nil, refused("%s: %s", to, err)
	}
	if err != nil {
		return nil, err
	}
	if sent.Duplicate {
		_, err = fmt.Fprintf(c.out, "sent: %s to %s was sent before (delivery %d); not queued again\n", msg.MessageID, to, sent.Seq)
	} else {
		_, err = fmt.Fprintf(c.out, "sent: %s to %s, delivery %d in %s's mailbox\n", msg.MessageID, to, sent.Seq, mailbox.Key(agent.Person))
	}
	return map[string]any{"to": to, "messageId": msg.MessageID, "id": sent.Seq, "duplicate": sent.Duplicate}, err
}

// deadlineArg is a send's deadline: an RFC 3339 time or a duration from
// now; empty, none of its own.
func deadlineArg(now time.Time, s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return time.Time{}, usageErr("deadline %q: an RFC 3339 time or a positive duration (2h)", s)
	}
	return now.Add(d), nil
}

// sendKagent invokes the session through muster with the caller's token:
// kagent authorizes the person, beekeeper only routes.
func (c *call) sendKagent(to string, msg a2aMessage) (any, error) {
	rest := strings.TrimPrefix(to, "kagent:")
	parts := strings.Split(rest, "/")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return nil, usageErr("to %q: kagent:<installation>/<namespace>/<session>", to)
	}
	installation, session := parts[0], parts[2]
	tool := c.s.cfg.Serve.Kagent[installation]
	if tool == "" || c.s.cfg.Serve.Muster == "" {
		return nil, refused("%s: installation %s is not one serve.kagent routes to", to, installation)
	}
	text := msg.text()
	if text == "" {
		return nil, usageErr("message: a kagent session takes text parts")
	}
	token, _ := c.ctx.Value(tokenKey{}).(string)
	out, err := callMuster(c.ctx, c.s.cfg.Serve.Muster, token, tool, map[string]any{
		"agent_instance_id": session, paramMessage: text, "message_id": msg.MessageID,
	})
	if err != nil {
		return nil, fmt.Errorf("%s: %w", to, err)
	}
	_, err = fmt.Fprintf(c.out, "sent: %s to %s\n%s\n", msg.MessageID, to, out)
	return map[string]any{"to": to, "messageId": msg.MessageID, "result": out}, err
}

// callMuster calls tool through muster's call_tool as the token's person
// and returns the tool's text.
func callMuster(ctx context.Context, url, token, tool string, args map[string]any) (string, error) {
	mc, err := client.NewStreamableHttpClient(url, transport.WithHTTPHeaders(map[string]string{"Authorization": "Bearer " + token}))
	if err != nil {
		return "", err
	}
	defer func() { _ = mc.Close() }()
	if err := mc.Start(ctx); err != nil {
		return "", err
	}
	init := mcp.InitializeRequest{}
	init.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	init.Params.ClientInfo = mcp.Implementation{Name: project.Name, Version: project.Version()}
	if _, err := mc.Initialize(ctx, init); err != nil {
		return "", fmt.Errorf("muster: %w", err)
	}
	req := mcp.CallToolRequest{}
	req.Params.Name = "call_tool"
	req.Params.Arguments = map[string]any{"name": tool, "arguments": args}
	res, err := mc.CallTool(ctx, req)
	if err != nil {
		return "", fmt.Errorf("muster: %s: %w", tool, err)
	}
	var texts []string
	for _, ct := range res.Content {
		if t, ok := ct.(mcp.TextContent); ok {
			texts = append(texts, t.Text)
		}
	}
	text := strings.Join(texts, "\n")
	if res.IsError {
		return "", fmt.Errorf("muster: %s: %s", tool, text)
	}
	return text, nil
}

func toolReceiveMessages(c *call, req mcp.CallToolRequest) (any, error) {
	mb, err := c.ownMailbox(req)
	if err != nil {
		return nil, err
	}
	c.concern = nil
	ds, err := c.s.mail.Receive(c.ctx, mb, req.GetInt("limit", 0), c.app.now)
	if err != nil {
		return nil, err
	}
	if len(ds) == 0 {
		_, _ = fmt.Fprintln(c.out, "no message waits")
	}
	for _, d := range ds {
		_, _ = fmt.Fprintf(c.out, "%d %s from %s: %s, expires %s\n", d.Seq, d.Kind, d.Sender, d.MessageID, d.Deadline.Format(time.RFC3339))
	}
	return map[string]any{keyMailbox: mb, "messages": ds}, nil
}

func toolAckMessages(c *call, req mcp.CallToolRequest) (any, error) {
	mb, err := c.ownMailbox(req)
	if err != nil {
		return nil, err
	}
	var seqs []int64
	for _, v := range req.GetIntSlice("ids", nil) {
		seqs = append(seqs, int64(v))
	}
	if len(seqs) == 0 {
		return nil, usageErr("ids is required: the ids receive_messages returned")
	}
	n, err := c.s.mail.Ack(c.ctx, mb, seqs, c.app.now)
	if err != nil {
		return nil, err
	}
	_, err = fmt.Fprintf(c.out, "acked %d of %d\n", n, len(seqs))
	return map[string]any{keyMailbox: mb, "acked": n}, err
}
