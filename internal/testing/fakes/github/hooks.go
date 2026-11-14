package fakegithub

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"

	"github.com/giantswarm/beekeeper/internal/forge/github/wire"
)

// Deliver modes of a state-changing tool (contract §4.2).
const (
	deliverDelivered = "delivered"
	deliverLost      = "lost"
	deliverNever     = "never"
	deliverDuplicate = "duplicate"
	deliverDelay     = "delay"
)

// hook is a webhook a state change produces.
type hook struct {
	event  string
	action string
	// repo is the repository the event belongs to; repos outside the installation send nothing.
	// Empty for org-level events (projects_v2_item, installation_repositories).
	repo    string
	repoID  int64
	payload any
}

// logEntry is an entry of the App's delivery log.
type logEntry struct {
	wire.HookDeliveryDetail
}

func (s *Server) envelope(action string, r *repo, sender string) wire.Envelope {
	st := s.st
	if sender == "" {
		sender = ghostLogin
	}
	e := wire.Envelope{
		Action:       action,
		Installation: &wire.Installation{ID: st.installationID},
		Sender:       &wire.User{Login: sender},
	}
	if r == nil || r.owner() == st.org {
		e.Organization = &wire.Organization{Login: st.org}
	}
	if r != nil {
		wr := s.wireRepo(r)
		e.Repository = &wr
	}
	return e
}

func (s *Server) repoHook(event, action string, r *repo, payload any) hook {
	return hook{event: event, action: action, repo: r.fullName, repoID: r.id, payload: payload}
}

// dispatch sends a step's webhooks, in order, by the step's deliver mode (a standing deliveries
// fault overrides it). It is called without s.mu. It returns what happened to each, for the step's
// result.
func (s *Server) dispatch(ctx context.Context, hooks []hook, mode deliverArg) []map[string]any {
	out := []map[string]any{}
	for _, h := range hooks {
		s.mu.Lock()
		if h.repo != "" {
			if r := s.st.repos[h.repo]; r == nil || !r.installed {
				s.mu.Unlock()
				out = append(out, map[string]any{"event": h.event, "action": h.action, argDeliver: "not-installed"})
				continue
			}
		}
		m := mode
		if s.delivery != "" {
			m = deliverArg{mode: s.delivery}
		}
		s.guidSeq++
		guid := fmt.Sprintf("00000000-0000-4000-8000-%012d", s.guidSeq)
		s.mu.Unlock()
		body, err := json.Marshal(h.payload)
		if err != nil {
			body = []byte(`{}`)
		}
		res := map[string]any{"event": h.event, "action": h.action, "guid": guid, argDeliver: m.String()}
		switch m.mode {
		case deliverNever:
		case deliverLost:
			s.logDelivery(h, guid, body, s.hookHeaders(h, guid, body), 0, "failed to connect to host", nil, "")
		case deliverDelay:
			h := h
			s.opts.Clock.AfterFunc(m.delay, func() { s.fire(context.Background(), h, guid, body) })
		case deliverDuplicate:
			res["status_code"] = s.post(ctx, h, guid, body)
			s.post(ctx, h, guid, body)
		default:
			res["status_code"] = s.post(ctx, h, guid, body)
		}
		out = append(out, res)
	}
	return out
}

// fire sends a delayed delivery when its time comes; a deliveries fault standing then applies.
func (s *Server) fire(ctx context.Context, h hook, guid string, body []byte) {
	s.mu.Lock()
	mode := s.delivery
	s.mu.Unlock()
	switch mode {
	case deliverNever:
	case deliverLost:
		s.logDelivery(h, guid, body, s.hookHeaders(h, guid, body), 0, "failed to connect to host", nil, "")
	default:
		s.post(ctx, h, guid, body)
	}
}

func (s *Server) hookHeaders(h hook, guid string, body []byte) map[string]string {
	s.mu.Lock()
	secret, appID := s.st.secret, s.st.appID
	s.mu.Unlock()
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write(body)
	return map[string]string{
		"Accept":                                 "*/*",
		"Content-Type":                           "application/json",
		"User-Agent":                             "GitHub-Hookshot/fake",
		wire.HeaderEvent:                         h.event,
		wire.HeaderDelivery:                      guid,
		wire.HeaderHookID:                        "1",
		"X-GitHub-Hook-Installation-Target-Type": "integration",
		"X-GitHub-Hook-Installation-Target-ID":   strconv.FormatInt(appID, 10),
		wire.HeaderSignature:                     "sha256=" + hex.EncodeToString(mac.Sum(nil)),
	}
}

// post delivers one webhook synchronously and logs it with the receiver's answer.
func (s *Server) post(ctx context.Context, h hook, guid string, body []byte) int {
	headers := s.hookHeaders(h, guid, body)
	url := s.opts.WebhookURL
	if url == "" {
		s.logDelivery(h, guid, body, headers, 0, "no webhook URL", nil, "")
		return 0
	}
	done := s.opts.Tracker.Begin("fake github: webhook delivery")
	defer done()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		s.logDelivery(h, guid, body, headers, 0, err.Error(), nil, "")
		return 0
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := s.hooks.Do(req)
	if err != nil {
		s.logDelivery(h, guid, body, headers, 0, "failed to connect to host", nil, "")
		return 0
	}
	rb, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	_ = resp.Body.Close()
	status := "OK"
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		status = fmt.Sprintf("Invalid HTTP Response: %d", resp.StatusCode)
	}
	rh := map[string]string{}
	for k := range resp.Header {
		rh[k] = resp.Header.Get(k)
	}
	s.logDelivery(h, guid, body, headers, resp.StatusCode, status, rh, string(rb))
	return resp.StatusCode
}

func (s *Server) logDelivery(h hook, guid string, body []byte, headers map[string]string, code int, status string, respHeaders map[string]string, respBody string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.logSeq++
	if respHeaders == nil {
		respHeaders = map[string]string{}
	}
	e := &logEntry{wire.HookDeliveryDetail{
		HookDelivery: wire.HookDelivery{
			ID:             9_000_000_000 + s.logSeq,
			GUID:           guid,
			DeliveredAt:    s.now(),
			Duration:       0.01,
			Status:         status,
			StatusCode:     code,
			Event:          h.event,
			Action:         h.action,
			InstallationID: s.st.installationID,
			RepositoryID:   h.repoID,
		},
		URL:      s.opts.WebhookURL,
		Request:  wire.HookDeliveryRequest{Headers: headers, Payload: json.RawMessage(body)},
		Response: wire.HookDeliveryReply{Headers: respHeaders, Payload: respBody},
	}}
	s.log = append(s.log, e)
}

// Deliveries returns the delivery log, oldest first (for tests).
func (s *Server) Deliveries() []wire.HookDeliveryDetail {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]wire.HookDeliveryDetail, 0, len(s.log))
	for _, e := range s.log {
		out = append(out, e.HookDeliveryDetail)
	}
	return out
}

// ---- payloads ----

func (s *Server) issueHook(action string, is *issue, sender string, mod func(*wire.IssuesEvent)) hook {
	r := s.st.repos[is.repo]
	ev := &wire.IssuesEvent{Envelope: s.envelope(action, r, sender), Issue: s.wireIssue(is)}
	if mod != nil {
		mod(ev)
	}
	return s.repoHook(wire.EventIssues, action, r, ev)
}

func (s *Server) itemHook(action string, it *boardItem, sender string, change *wire.FieldValueChange) hook {
	st := s.st
	item := wire.ProjectsV2Item{
		ID:            it.id,
		NodeID:        it.nodeID,
		ProjectNodeID: st.projectNodeID(),
		ContentNodeID: "I_" + it.issue,
		ContentType:   typenameIssue,
		Creator:       &wire.User{Login: ghostLogin},
		CreatedAt:     it.createdAt,
		UpdatedAt:     it.updatedAt,
	}
	if it.archived {
		item.ArchivedAt = timePtr(it.archAt)
	}
	ev := &wire.ProjectsV2ItemEvent{Envelope: s.envelope(action, nil, sender), ProjectsV2Item: item}
	if change != nil {
		ev.Changes = &wire.ProjectsV2ItemChanges{FieldValue: change}
	}
	return hook{event: wire.EventProjectsV2Item, action: action, payload: ev}
}

func (s *Server) pullHook(action string, p *pull, sender string, mod func(*wire.PullRequestEvent)) hook {
	r := s.st.repos[p.repo]
	ev := &wire.PullRequestEvent{Envelope: s.envelope(action, r, sender), Number: p.number, PullRequest: s.wirePull(p)}
	if mod != nil {
		mod(ev)
	}
	return s.repoHook(wire.EventPullRequest, action, r, ev)
}

func (s *Server) fieldChange(field string, from, to any) *wire.FieldValueChange {
	ftype := "single_select"
	if field == fieldPriority {
		ftype = "number"
	}
	return &wire.FieldValueChange{
		FieldNodeID: s.st.fieldNodeID(field), FieldType: ftype, FieldName: field, ProjectNumber: s.st.project,
		From: from, To: to,
	}
}

func optionChange(field, name string) any {
	if name == "" {
		return nil
	}
	return wire.SelectOptionChange{ID: optionID(field, name), Name: name}
}

func numberChange(p *float64) any {
	if p == nil {
		return nil
	}
	return *p
}
