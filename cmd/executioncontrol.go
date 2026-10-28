package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/giantswarm/beekeeper/internal/execution"
	"github.com/giantswarm/beekeeper/internal/post"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/spf13/cobra"
)

type executionAuthority struct {
	Stopped    bool `json:"stopped"`
	GitHubHeld bool `json:"githubHeld"`
}
type executionNote struct {
	Draft noteDraft  `json:"draft"`
	Note  state.Note `json:"note"`
}
type executionPark struct {
	On    string `json:"on"`
	Waits string `json:"waits"`
}
type executionReport struct {
	Finished bool     `json:"finished"`
	Text     string   `json:"text"`
	Problems []string `json:"problems"`
}
type executionEvent struct {
	Text string `json:"text"`
}

func (a *app) remoteControl(ctx context.Context, op string, data any) error {
	s, j, err := a.executionCaller()
	if err != nil {
		return err
	}
	result, err := s.SendControl(ctx, j, op, data, a.executionRequestID, nil)
	if err != nil {
		return fmt.Errorf("coordinator delivery unconfirmed or refused; retry the same request, no host-local state was changed: %w", err)
	}
	_, err = fmt.Fprintln(a.out, result.Output)
	return err
}

func (a *app) executionCoordinatorCmd() *cobra.Command {
	var protocol int
	var host string
	c := &cobra.Command{Use: "coordinator-rpc", Hidden: true, Args: cobra.NoArgs, PersistentPreRunE: func(*cobra.Command, []string) error { a.hook = true; return a.load() }, RunE: func(cmd *cobra.Command, _ []string) error {
		if protocol != execution.Protocol || !execution.ValidIdentity(host) || a.managedExecutionID() != "" {
			return refused("invalid coordinator RPC identity")
		}
		return a.serveExecutionCoordinator(cmd.Context(), host, cmd.InOrStdin(), cmd.OutOrStdout())
	}}
	c.Flags().IntVar(&protocol, "protocol", 0, "execution protocol")
	c.Flags().StringVar(&host, "host", "", "authenticated host route")
	return c
}

func (a *app) serveExecutionCoordinator(ctx context.Context, host string, input io.Reader, output io.Writer) error {
	b, err := io.ReadAll(io.LimitReader(input, execution.MaxWire+1))
	if err != nil {
		return err
	}
	var req execution.CallbackRequest
	reply := execution.WireResponse{Protocol: execution.Protocol}
	if execution.Decode(b, &req) != nil {
		reply.Error = "invalid callback"
		return json.NewEncoder(output).Encode(reply)
	}
	reply.Ref, reply.RequestID = req.Ref, req.RequestID
	err = a.store.Update(func(st *state.State) ([]state.Event, error) {
		r, i := executionRecord(st, req.Ref)
		if r == nil || req.Protocol != execution.Protocol || req.RequestID == "" || req.Ref.Validate() != nil || string(req.Ref.Host) != host || !execution.VerifyCallback(req, r.Request.ControlKey) {
			return nil, errors.New("unauthenticated execution callback")
		}
		h, ok := a.cfg.Agents.Hosts[r.Selector]
		if !ok || h.Identity != req.Ref.Host {
			return nil, errors.New("execution host is no longer configured")
		}
		var value any
		var events []state.Event
		switch req.Operation {
		case "activate":
			var request execution.StartRequest
			if execution.Decode(req.Payload, &request) != nil || request.Digest() != r.RequestDigest || r.StopRequest != nil || r.Terminal() || st.Stop != nil || r.Phase != "activating" {
				return nil, errors.New("live coordinator activation refused")
			}
			value = true
		case "guard":
			authority := executionAuthority{Stopped: st.Stop != nil || r.StopRequest != nil || r.Terminal()}
			for _, hold := range st.Holds {
				if hold.Target == holdGitHub && hold.Active(time.Now()) {
					authority.GitHubHeld = true
				}
			}
			value = authority
		case "status":
			// Only public roster fields cross this boundary; journals contain a
			// component credential and are never a status response.
			type row struct {
				Name      string         `json:"name"`
				Task      string         `json:"task"`
				Host      string         `json:"host"`
				Role      string         `json:"role,omitempty"`
				Execution *execution.Ref `json:"execution,omitempty"`
			}
			rows := []row{}
			for _, ag := range st.Agents {
				role := ""
				if st.Supervisor != nil && st.Supervisor.Holder != nil && st.Supervisor.Holder.Is(ag.Party) {
					role = "supervisor"
				}
				rows = append(rows, row{ag.Name, ag.Task, ag.Host, role, ag.Execution})
			}
			value = rows
		case "control":
			var control execution.ControlRequest
			if execution.Decode(req.Payload, &control) != nil || control.Ref != req.Ref || !execution.ValidIdentity(control.ID) || control.Sequence == 0 {
				return nil, errors.New("invalid worker control")
			}
			digest := control.ContentDigest()
			if old, ok := r.ControlResults[control.ID]; ok {
				if old.Digest != digest || old.Sequence != control.Sequence {
					return nil, errors.New("control idempotency conflict")
				}
				value = old
				break
			}
			if control.Sequence != r.ControlSequence+1 {
				return nil, errors.New("control sequence gap or replay")
			}
			result := execution.ControlResult{ID: control.ID, Sequence: control.Sequence, Digest: digest}
			result.Output, events, err = a.applyExecutionControl(st, st.Starts[i].Party, control)
			if err != nil {
				result.Error = err.Error()
				events = nil
				err = nil
			}
			if r.ControlResults == nil {
				r.ControlResults = map[string]execution.ControlResult{}
			}
			r.ControlResults[control.ID] = result
			r.ControlSequence = control.Sequence
			value = result
		default:
			return nil, errors.New("unsupported coordinator operation")
		}
		reply.Payload, err = json.Marshal(value)
		return events, err
	})
	if err != nil {
		reply.Error = err.Error()
	}
	return json.NewEncoder(output).Encode(reply)
}

func (a *app) applyExecutionControl(st *state.State, me state.Party, c execution.ControlRequest) (string, []state.Event, error) {
	i := slices.IndexFunc(st.Agents, func(ag state.Agent) bool { return ag.Is(me) })
	if i < 0 {
		return "", nil, refused("worker registration is missing")
	}
	ag := &st.Agents[i]
	switch c.Operation {
	case "note":
		var v executionNote
		if err := execution.Decode(c.Data, &v); err != nil {
			return "", nil, err
		}
		d, n := v.Draft, v.Note
		if n.Kind != noteDecision || n.For == "" || n.Until != "" || n.Held != nil || len(n.AlsoHeld) > 0 || len(n.Refs) == 0 {
			return "", nil, refused("remote notes support ordinary decisions with a thread only")
		}
		// Revalidate against coordinator policy, never the host's guide policy.
		d.Question = n.Question
		d.StatusQuo = n.StatusQuo
		d.Default = n.Default
		d.Options = n.Options
		d.Recommend = n.Recommend
		d.Kind = n.Kind
		missing := append(d.unanswered(), d.unrenderable(n.For)...)
		if guides(a.cfg.Guide.Person, &n) {
			missing = append(missing, d.missing()...)
		}
		if n.Due.IsZero() {
			missing = append(missing, "due time")
		}
		for _, ref := range n.Refs {
			if _, ok := parseRef(ref); !ok {
				missing = append(missing, "valid thread")
			}
		}
		if len(missing) > 0 {
			return "", nil, refused("note refused: %s", strings.Join(missing, "; "))
		}
		// Plan admission needs external checks; do not silently skip that gate.
		if len(a.cfg.Plans.Repositories) > 0 && len(pullURL.FindAllString(d.subject(), -1)) > 0 {
			return "", nil, refused("plan-linked decisions require the coordinating component directly")
		}
		n.By, n.At = me, a.now.UTC()
		n.ID = st.NextNote + 1
		n.Text = d.text()
		st.NextNote++
		st.Notes = append(st.Notes, n)
		return fmt.Sprintf("note #%d for %s", n.ID, n.For), []state.Event{event(me, "note.added", "#%d for %s: %s", n.ID, n.For, n.Text)}, nil
	case "park":
		var v executionPark
		if err := execution.Decode(c.Data, &v); err != nil {
			return "", nil, err
		}
		if ag.Task == "" || ag.Done || strings.TrimSpace(v.Waits) == "" {
			return "", nil, refused("park requires an unfinished task and wait reason")
		}
		if id, ok := parkNote(v.On); ok {
			if !slices.ContainsFunc(st.Notes, func(n state.Note) bool { return n.ID == id }) {
				return "", nil, refused("note is not open")
			}
		} else if target, ok := parkHold(v.On); ok {
			if !holdStands(st, target, a.now) {
				return "", nil, refused("hold is not active")
			}
		} else if v.On != "" {
			if _, ok := parseRef(v.On); !ok {
				return "", nil, refused("unsupported remote park target")
			}
		}
		ag.Park = &state.Park{By: me, At: a.now.UTC(), On: v.On, Waits: oneLine(v.Waits)}
		return "park recorded on coordinator; end your turn now", []state.Event{event(me, parkEvent, "%s: %s", ag.Name, parkedText(ag.Park, a.now))}, nil
	case "report":
		var v executionReport
		if err := execution.Decode(c.Data, &v); err != nil {
			return "", nil, err
		}
		problems, err := problemsFound(v.Problems, v.Finished)
		if err != nil {
			return "", nil, err
		}
		v.Text = strings.TrimSpace(v.Text)
		if v.Finished && v.Text == "" {
			return "", nil, refused("final report is required")
		}
		if m := post.Unlinked(strings.Join(append([]string{v.Text}, problems...), "\n"), agentHere(st, ag)); len(m) > 0 {
			return "", nil, refused("report refused: %s", strings.Join(m, "; "))
		}
		reportIdle(ag, a.now)
		endNotes(st, me, a.now)
		ag.Done = v.Finished
		verb := "agents.idle"
		if v.Finished {
			verb = "agents.done"
			clearRecords(st, me)
		}
		events := []state.Event{event(me, verb, "%s done: %s", ag.Name, ag.LastTask)}
		st.WorkerReports = append(st.WorkerReports, state.WorkerReport{By: me, At: a.now.UTC(), Task: ag.LastTask, Text: v.Text, Problems: problems})
		if v.Text != "" {
			events = append(events, event(me, "agents.report", "%s: %s", ag.Name, v.Text))
		}
		for _, p := range problems {
			events = append(events, event(me, "agents.problem", "%s: %s", ag.Name, p))
		}
		return "report acknowledged by coordinator", events, nil
	case "handover", "event":
		var v executionEvent
		if err := execution.Decode(c.Data, &v); err != nil {
			return "", nil, err
		}
		if strings.TrimSpace(v.Text) == "" {
			return "", nil, refused("text is required")
		}
		if m := post.Unlinked(v.Text, agentHere(st, ag)); len(m) > 0 {
			return "", nil, refused("full issue and pull request URLs are required")
		}
		verb := "log.added"
		if c.Operation == "handover" {
			verb = "agents.note"
		}
		return "recorded by coordinator", []state.Event{event(me, verb, "%s", v.Text)}, nil
	default:
		return "", nil, refused("unsupported worker control")
	}
}
