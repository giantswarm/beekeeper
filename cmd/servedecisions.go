package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/identity"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/state/kube"
)

// gatewayTimeout bounds one request to klaus-gateway.
const gatewayTimeout = 15 * time.Second

// serveParty is beekeeper serve acting on its own: a decision's default, a
// failed close.
var serveParty = state.Party{Name: "beekeeper serve"}

// The outcomes a decision's message closes with.
const (
	outcomeAnswered  = "answered"
	outcomeDefaulted = "defaulted"
	outcomeWithdrawn = "withdrawn"
)

// decision is klaus-gateway's POST /decisions body: one Slack message to a
// person (a direct message) or to a team's channel, answered by a click,
// the modal or a thread reply, each of which calls Answer.Tool through
// muster as the person who answered.
type decision struct {
	Person    string           `json:"person,omitempty"`
	Team      string           `json:"team,omitempty"`
	Channel   string           `json:"channel,omitempty"`
	Note      string           `json:"note"`
	Question  string           `json:"question"`
	StatusQuo string           `json:"statusQuo"`
	Options   []decisionOption `json:"options,omitempty"`
	Recommend int              `json:"recommend,omitempty"`
	Due       time.Time        `json:"due"`
	Default   string           `json:"default"`
	AskedBy   string           `json:"askedBy"`
	Answer    toolInvocation   `json:"answer"`
}

type decisionOption struct {
	Label       string `json:"label"`
	Consequence string `json:"consequence,omitempty"`
}

type toolInvocation struct {
	Tool      string         `json:"tool"`
	Arguments map[string]any `json:"arguments,omitempty"`
}

// gateway is the klaus-gateway the decisions go through.
type gateway struct {
	cfg  config.Gateway
	http *http.Client
}

func newGateway(cfg config.Gateway) *gateway {
	if cfg.URL == "" {
		return nil
	}
	return &gateway{cfg: cfg, http: &http.Client{Timeout: gatewayTimeout}}
}

// post puts d to its addressee and returns the gateway's handle on it.
func (g *gateway) post(ctx context.Context, d decision) (string, error) {
	var receipt struct {
		ID string `json:"id"`
	}
	if err := g.do(ctx, "/decisions", d, &receipt); err != nil {
		return "", err
	}
	if receipt.ID == "" {
		return "", errors.New("klaus-gateway returned no decision id")
	}
	return receipt.ID, nil
}

// close rewrites the decision's message to its outcome.
func (g *gateway) close(ctx context.Context, id, outcome, text string) error {
	body := map[string]string{"outcome": outcome, "text": truncate(text, 3000)}
	return g.do(ctx, "/decisions/"+url.PathEscape(id)+"/close", body, nil)
}

// do posts body as JSON with the ServiceAccount token and decodes the
// answer into out; a status other than 2xx is an error carrying the
// gateway's reason.
func (g *gateway) do(ctx context.Context, path string, body, out any) error {
	token, err := os.ReadFile(g.cfg.TokenFile) //nolint:gosec // the configured token file
	if err != nil {
		return fmt.Errorf("klaus-gateway token: %w", err)
	}
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(g.cfg.URL, "/")+path, bytes.NewReader(b)) //nolint:gosec // the configured klaus-gateway
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))
	resp, err := g.http.Do(req) //nolint:gosec // the configured klaus-gateway
	if err != nil {
		return fmt.Errorf("klaus-gateway %s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode/100 != 2 {
		return &gatewayError{status: resp.StatusCode, reason: strings.TrimSpace(string(raw))}
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(raw, out)
}

// gatewayError is klaus-gateway's refusal or failure.
type gatewayError struct {
	status int
	reason string
}

func (e *gatewayError) Error() string {
	return fmt.Sprintf("klaus-gateway: %d %s: %s", e.status, http.StatusText(e.status), e.reason)
}

// addressee is the email a person's decision goes to, or the team and the
// Slack channel of a team's: people and channels of the configuration.
func addressee(cfg config.Serve, forWho string) (email, team, channel string, err error) {
	if t, ok := strings.CutPrefix(forWho, teamPrefix); ok {
		if channel = cfg.Channels[t]; channel == "" {
			return "", "", "", refused("team %s has no Slack channel to decide in (serve.channels)", t)
		}
		return "", t, channel, nil
	}
	if email = personEmail(cfg, forWho); email == "" {
		return "", "", "", refused("%s is nobody's name in serve.people, nor an email", forWho)
	}
	return email, "", "", nil
}

// personEmail is the email of the person named name: its serve.people
// entry, or name itself when it is an email.
func personEmail(cfg config.Serve, name string) string {
	for k, v := range cfg.People {
		if strings.EqualFold(k, name) {
			return v
		}
	}
	if strings.Contains(name, "@") {
		return name
	}
	return ""
}

// decidesNote reports whether who is n's addressee: the person it names, or a
// member of the Dex group of the team it names.
func decidesNote(cfg config.Serve, n *state.Note, who identity.Caller) bool {
	if t, ok := strings.CutPrefix(n.For, teamPrefix); ok {
		for group, team := range cfg.Teams {
			if team == t && who.In(group) {
				return true
			}
		}
		return false
	}
	email := personEmail(cfg, n.For)
	return email != "" && strings.EqualFold(email, who.Email)
}

// decisionOf is n's message: its addressee, its parts and the tool an
// answer calls.
func (s *server) decisionOf(n *state.Note) (decision, error) {
	email, team, channel, err := addressee(s.cfg.Serve, n.For)
	if err != nil {
		return decision{}, err
	}
	d := decision{Person: email, Team: team, Channel: channel, Note: fmt.Sprintf("note #%d", n.ID),
		Question: n.Question, StatusQuo: n.StatusQuo, Recommend: n.Recommend, Due: n.Due, Default: n.Default,
		AskedBy: partyName(n.By) + " on " + n.By.Host,
		Answer:  toolInvocation{Tool: s.cfg.Serve.Gateway.AnswerTool, Arguments: map[string]any{paramNote: n.ID, paramVia: viaSlack}}}
	if n.By.Host == "" {
		d.AskedBy = partyName(n.By)
	}
	for i := range n.Options {
		label, cons, _ := n.Option(i + 1)
		d.Options = append(d.Options, decisionOption{Label: label, Consequence: cons})
	}
	return d, nil
}

// postDecision puts a decision just filed to its addressee and keeps the
// gateway's handle on the note. A decision the gateway does not take is
// withdrawn again: it would wait on nobody.
func (s *server) postDecision(ctx context.Context, by state.Party, n *state.Note) error {
	if s.gw == nil || n.Kind != noteDecision {
		return nil
	}
	d, err := s.decisionOf(n)
	if err == nil {
		var id string
		if id, err = s.gw.post(ctx, d); err == nil {
			n.Posted = id
			// The call's note.add Event stands for the posting too.
			return s.store.Update(func(st *state.State) ([]state.Event, error) {
				for i := range st.Notes {
					if st.Notes[i].ID == n.ID {
						st.Notes[i].Posted = id
					}
				}
				return nil, nil
			})
		}
	}
	werr := s.store.Update(func(st *state.State) ([]state.Event, error) {
		for i := range st.Notes {
			if st.Notes[i].ID == n.ID {
				st.Notes = append(st.Notes[:i], st.Notes[i+1:]...)
				return []state.Event{event(by, noteDone, "#%d withdrawn, not delivered: %v", n.ID, err)}, nil
			}
		}
		return nil, nil
	})
	var ge *gatewayError
	if errors.As(err, &ge) && ge.status/100 == 4 {
		err = refused("decision #%d not delivered, withdrawn: %s", n.ID, ge.reason)
	}
	return errors.Join(err, werr)
}

// closeDecision rewrites a closed decision's message, by whichever path it
// closed; the failure is logged and audited, the note stays closed.
func (s *server) closeDecision(ctx context.Context, n *state.Note, outcome, text string) {
	if s.gw == nil || n.Posted == "" {
		return
	}
	err := s.gw.close(ctx, n.Posted, outcome, text)
	if err == nil {
		return // the call's line and Event, or the default's, say it
	}
	s.log.Error("decision close", "note", n.ID, "decision", n.Posted, "outcome", outcome, "detail", err.Error())
	ev := event(serveParty, "note.close-failed", "#%d decision %s (%s): %v", n.ID, n.Posted, outcome, err)
	_ = s.store.Audit(kube.NoteObject(n.By.Team, n.ID), n.By.Team, ev)
}

// defaultLoop closes the decisions due unanswered with their default, every
// expireEvery, until ctx ends.
func (s *server) defaultLoop(ctx context.Context) {
	t := time.NewTicker(expireEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := s.defaultDue(ctx, s.now()); err != nil {
				s.log.Error("default", "detail", err.Error())
			}
		}
	}
}

// defaultDue closes the decisions due now: note.defaulted on each, its
// message rewritten.
func (s *server) defaultDue(ctx context.Context, now time.Time) error {
	var due []state.Note
	err := s.store.Update(func(st *state.State) ([]state.Event, error) {
		_, evs, notes := closeDefaulted(st, s.cfg.Guide.Person, serveParty, now)
		due = notes
		return evs, nil
	})
	if err != nil {
		return err
	}
	for i := range due {
		s.log.Info("default", "note", due[i].ID, "for", due[i].For, "default", due[i].Default)
		s.closeDecision(ctx, &due[i], outcomeDefaulted, due[i].Default)
	}
	return nil
}
