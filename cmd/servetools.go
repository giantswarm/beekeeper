package cmd

import (
	"cmp"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/state/kube"
	"github.com/giantswarm/beekeeper/pkg/apis/beekeeper/v1alpha1"
)

// The tools' parameters that more than one tool takes or the tests name.
const (
	paramEnvironment = "environment"
	paramRepo        = "repo"
	paramReason      = "reason"
	paramChoice      = "choice"
	paramVia         = "via"
	paramAgent       = "agent"
	paramHost        = "host"
	paramPurpose     = "purpose"
	paramTarget      = "target"
	paramNote        = "note"
	paramFor         = "for"
	paramKind        = "kind"
	paramText        = "text"
	paramMessage     = "message"
	keyMailbox       = "mailbox"
	keyNotes         = "notes"
	lanesName        = "lanes"
)

// The options every tool takes: who the calling agent is and where it runs.
func callerOptions() []mcp.ToolOption {
	return []mcp.ToolOption{
		mcp.WithString(paramAgent, mcp.Description("the calling agent's name (default: the person's email)")),
		mcp.WithString(paramHost, mcp.Description("the machine or installation the agent runs on")),
	}
}

func newTool(name, desc string, readOnly bool, opts ...mcp.ToolOption) mcp.Tool {
	opts = append([]mcp.ToolOption{mcp.WithDescription(desc), mcp.WithReadOnlyHintAnnotation(readOnly)}, opts...)
	return mcp.NewTool(name, append(opts, callerOptions()...)...)
}

func (s *server) tools() []serveTool {
	return []serveTool{
		{newTool("lease_list", "The installations: who holds each, since when and what for, and which are free.", true), toolLeaseList},
		{newTool("lease_claim", "Claim an installation; refused while another holds it or it upgrades.", false,
			mcp.WithString(paramEnvironment, mcp.Required(), mcp.Description("the Environment, an installation's name")),
			mcp.WithString(paramPurpose, mcp.Required(), mcp.Description("what the installation is for"))), toolLeaseClaim},
		{newTool("lease_release", "Release an installation: your own, or as the holder's team's supervisor role.", false,
			mcp.WithString(paramEnvironment, mcp.Required(), mcp.Description("the Environment"))), toolLeaseRelease},
		{newTool("hold_list", "The holds: merges into a repository or lane, or all GitHub work, held until lifted.", true), toolHoldList},
		{newTool("hold_set", "Hold a target: owner/repo, lane:<name>, merges or github.", false,
			mcp.WithString(paramTarget, mcp.Required(), mcp.Description("owner/repo, lane:<name>, merges or github")),
			mcp.WithString(paramReason, mcp.Required(), mcp.Description("why")),
			mcp.WithString("until", mcp.Description("when it ends by itself: a time (15:30) or a duration (2h)")),
			mcp.WithString("except", mcp.Description("the one owner/repo or owner/repo#n the hold lets through"))), toolHoldSet},
		{newTool("hold_lift", "Lift a hold: your own, or as its setter's team's supervisor role.", false,
			mcp.WithString(paramTarget, mcp.Required(), mcp.Description("the held target"))), toolHoldLift},
		{newTool("lanes", "The merge lanes: the running merge, the settling one and the queue.", true), toolLanes},
		{newTool("lane_queue", "Queue a pull request's merge at the end of its lane.", false, prOptions()...), toolLaneQueue},
		{newTool("lane_settle", "Register a merge run outside the gate as its lane's head, running or merged.", false,
			append(prOptions(), mcp.WithBoolean("merged", mcp.Description("the pull request is merged: the lane settles")))...), toolLaneSettle},
		{newTool("lane_turn", "Whether a queued merge is its lane's next, and what is ahead of it.", true, prOptions()...), toolLaneTurn},
		{newTool("note_add", "File a note: a decision for a person or a team, put to them in Slack, or a memo.", false,
			mcp.WithString(paramText, mcp.Required(), mcp.Description("the question (one line, at most 150 characters) or the memo")),
			mcp.WithString(paramFor, mcp.Description("who decides: a person's name or email, or team:<name>")),
			mcp.WithString(paramKind, mcp.Description("decision, memo or login")),
			mcp.WithString("due", mcp.Description("when it is due: a time (22:55) or a duration (3h)")),
			mcp.WithString("default", mcp.Description("the action if nobody answers by the due time")),
			mcp.WithString("status_quo", mcp.Description("what is true now")),
			mcp.WithString("why", mcp.Description("why it needs the person")),
			mcp.WithArray("options", mcp.WithStringItems(), mcp.Description(`"<choice>: <consequence>" each, at most 10, labels at most 75 characters`)),
			mcp.WithNumber("recommend", mcp.Description("the option recommended, 1-based")),
			mcp.WithString("checked", mcp.Description("where a merged, green, released, rolled or closed claim was checked")),
			mcp.WithArray("refs", mcp.WithStringItems(), mcp.Description("the issues and pull requests it asks about, owner/repo#n")),
			mcp.WithBoolean("pin", mcp.Description("a standing instruction"))), toolNoteAdd},
		{newTool("note_list", "The open notes, decisions apart from memos.", true), toolNoteList},
		{newTool("note_answer", "Answer a decision, word for word, and close it: as the person it is for, or a member of the team it is for.", false,
			mcp.WithNumber(paramNote, mcp.Required(), mcp.Description("the note's number")),
			mcp.WithNumber(paramChoice, mcp.Description("the option chosen, 1-based")),
			mcp.WithString(paramText, mcp.Description("the answer in the person's own words")),
			mcp.WithString(paramVia, mcp.Description("how the answer came: cli (the default) or slack"))), toolNoteAnswer},
		{newTool("note_done", "Close a note without an answer: its decision is withdrawn. Its filer's, or the filer's team's supervisor role's.", false,
			mcp.WithNumber(paramNote, mcp.Required(), mcp.Description("the note's number"))), toolNoteDone},
		{newTool("agents_register", "Register the calling agent on its team's roster, idle unless it holds an open task.", false), toolAgentsRegister},
		{newTool("list_agents", "The agent roster.", true,
			mcp.WithString("scope", mcp.Description("all (default) or team: only the caller's team"))), toolListAgents},
		{newTool("snapshot", "Everything at once: leases, holds, lanes, notes and the roster.", true), toolSnapshot},
	}
}

func prOptions() []mcp.ToolOption {
	return []mcp.ToolOption{
		mcp.WithString(paramRepo, mcp.Required(), mcp.Description("the repository, owner/repo")),
		mcp.WithNumber("pr", mcp.Required(), mcp.Description("the pull request's number")),
	}
}

// heldLease is an Environment's holder.
type heldLease struct {
	Environment string      `json:"environment"`
	Holder      state.Party `json:"holder"`
	Purpose     string      `json:"purpose"`
	Since       time.Time   `json:"since"`
}

type leaseListView struct {
	Held []heldLease `json:"held"`
	Free []string    `json:"free"`
}

func toolLeaseList(c *call, _ mcp.CallToolRequest) (any, error) { return c.leaseList() }

func (c *call) leaseList() (*leaseListView, error) {
	envs, err := c.s.store.Environments()
	if err != nil {
		return nil, err
	}
	v := &leaseListView{Held: []heldLease{}, Free: []string{}}
	for _, e := range envs {
		if h := e.Status.Holder; h != nil {
			v.Held = append(v.Held, heldLease{Environment: e.Name, Holder: kube.PartyOf(h.Party), Purpose: h.Purpose, Since: h.Since.Time})
		} else {
			v.Free = append(v.Free, e.Name)
		}
	}
	a := c.app
	if len(v.Held) == 0 {
		_, _ = fmt.Fprintln(a.out, "no installation is held")
	} else {
		w := a.table()
		_, _ = fmt.Fprintln(w, "ENVIRONMENT\tHOLDER\tSINCE\tPURPOSE")
		for _, h := range v.Held {
			_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", h.Environment, withOwner(truncate(partyName(h.Holder), 60), state.Party{Team: h.Holder.Team, Host: h.Holder.Host}), clock(a.now, h.Since), truncate(h.Purpose, 60))
		}
		_ = w.Flush()
	}
	if len(v.Free) > 0 {
		_, _ = fmt.Fprintln(a.out, "free:", strings.Join(v.Free, ", "))
	}
	return v, nil
}

// environment is the Environment name, or a refusal naming the known ones.
func (c *call) environment(name string) (*v1alpha1.Environment, error) {
	envs, err := c.s.store.Environments()
	if err != nil {
		return nil, err
	}
	names := make([]string, len(envs))
	for i := range envs {
		if envs[i].Name == name {
			return &envs[i], nil
		}
		names[i] = envs[i].Name
	}
	return nil, usageErr("%s is not an Environment here (known: %s)", name, strings.Join(names, ", "))
}

func (c *call) heldBy(env string, h v1alpha1.Holder) error {
	return refused("%s is held by %q since %s: %s", env, partyName(kube.PartyOf(h.Party)), clock(c.app.now, h.Since.Time), h.Purpose)
}

func toolLeaseClaim(c *call, req mcp.CallToolRequest) (any, error) {
	name, err := required(req, paramEnvironment, "the installation to claim")
	if err != nil {
		return nil, err
	}
	c.concern = kube.EnvironmentObject(name)
	purpose, err := required(req, paramPurpose, "say what the installation is for")
	if err != nil {
		return nil, err
	}
	env, err := c.environment(name)
	if err != nil {
		return nil, err
	}
	if u := env.Status.Upgrade; u != nil {
		return nil, refused("%s upgrades to %s since %s: claims wait until the upgrade ends", name, u.To, clock(c.app.now, u.Since.Time))
	}
	if h := env.Status.Holder; h != nil {
		if kube.PartyOf(h.Party).Is(c.me) {
			_, err := fmt.Fprintf(c.out, "%s is already yours (since %s)\n", name, clock(c.app.now, h.Since.Time))
			return heldLease{Environment: name, Holder: c.me, Purpose: h.Purpose, Since: h.Since.Time}, err
		}
		return nil, c.heldBy(name, *h)
	}
	h := v1alpha1.Holder{Party: kube.APIParty(c.me), Purpose: purpose, Since: metav1.NewTime(c.app.now.UTC().Truncate(time.Second))}
	if err := c.s.store.Claim(name, h); err != nil {
		if held := (*kube.HeldError)(nil); errors.As(err, &held) {
			return nil, c.heldBy(name, held.Holder)
		}
		return nil, err
	}
	c.store.wrote = true
	_, err = fmt.Fprintf(c.out, "claimed %s\n", name)
	return heldLease{Environment: name, Holder: c.me, Purpose: purpose, Since: h.Since.Time}, err
}

func toolLeaseRelease(c *call, req mcp.CallToolRequest) (any, error) {
	name, err := required(req, paramEnvironment, "the installation to release")
	if err != nil {
		return nil, err
	}
	c.concern = kube.EnvironmentObject(name)
	env, err := c.environment(name)
	if err != nil {
		return nil, err
	}
	h := env.Status.Holder
	if h == nil {
		_, err := fmt.Fprintf(c.out, "%s was free\n", name)
		return map[string]any{"environment": name, "released": false}, err
	}
	holder := kube.PartyOf(h.Party)
	if err := c.may(name+"'s lease", holder); err != nil {
		return nil, err
	}
	if err := c.s.store.Free(name, holder, c.me, c.app.now.UTC()); err != nil {
		if held := (*kube.HeldError)(nil); errors.As(err, &held) {
			return nil, c.heldBy(name, held.Holder)
		}
		return nil, err
	}
	c.store.wrote = true
	msg := "released " + name
	if holder.Person != c.me.Person {
		msg += fmt.Sprintf(" (held by %q: %s)", partyName(holder), h.Purpose)
	}
	_, err = fmt.Fprintln(c.out, msg)
	return map[string]any{"environment": name, "released": true, "holder": holder}, err
}

func toolHoldList(c *call, _ mcp.CallToolRequest) (any, error) {
	holds, err := c.holdList()
	return map[string]any{"holds": holds}, err
}

func (c *call) holdList() ([]state.Hold, error) {
	if err := c.verb(c.app.holdCmd()); err != nil {
		return nil, err
	}
	st, err := c.app.store.Read()
	if err != nil {
		return nil, err
	}
	return append(append([]state.Hold{}, c.app.activeHolds(st)...), liftedHolds(st, c.app.now)...), nil
}

// pruneHolds takes the expired holds away, one per update: the store writes
// one object at a time, and the verbs drop an expired hold beside the one
// they change.
func (c *call) pruneHolds() error {
	st, err := c.app.store.Read()
	if err != nil {
		return err
	}
	for _, h := range st.Holds {
		if !h.Expired(c.app.now) {
			continue
		}
		err := c.s.store.Update(func(st *state.State) ([]state.Event, error) {
			st.Holds = slices.DeleteFunc(st.Holds, func(x state.Hold) bool { return x.Target == h.Target && x.Expired(c.app.now) })
			return nil, nil
		})
		if err != nil {
			return err
		}
	}
	return nil
}

func (c *call) hold(target string) (*state.Hold, error) {
	st, err := c.app.store.Read()
	if err != nil {
		return nil, err
	}
	for _, h := range st.Holds {
		if h.Target == target {
			return &h, nil
		}
	}
	return nil, nil
}

func toolHoldSet(c *call, req mcp.CallToolRequest) (any, error) {
	target, err := required(req, paramTarget, "owner/repo, lane:<name>, merges or github")
	if err != nil {
		return nil, err
	}
	c.concern = kube.HoldObject(target)
	args := []string{"set", target, "--reason", req.GetString(paramReason, "")}
	if u := req.GetString("until", ""); u != "" {
		args = append(args, "--until", u)
	}
	if e := req.GetString("except", ""); e != "" {
		args = append(args, "--except", e)
	}
	if err := c.lanes(); err != nil {
		return nil, err
	}
	if err := c.pruneHolds(); err != nil {
		return nil, err
	}
	if cur, err := c.hold(target); err != nil {
		return nil, err
	} else if cur != nil && cur.Active(c.app.now) {
		if err := c.may("the hold on "+target, cur.By); err != nil {
			return nil, err
		}
	}
	if err := c.verb(c.app.holdCmd(), args...); err != nil {
		return nil, err
	}
	return c.hold(target)
}

func toolHoldLift(c *call, req mcp.CallToolRequest) (any, error) {
	target, err := required(req, paramTarget, "the held target")
	if err != nil {
		return nil, err
	}
	c.concern = kube.HoldObject(target)
	if err := c.pruneHolds(); err != nil {
		return nil, err
	}
	cur, err := c.hold(target)
	if err != nil {
		return nil, err
	}
	if cur != nil {
		if err := c.may("the hold on "+target, cur.By); err != nil {
			return nil, err
		}
	}
	if err := c.verb(c.app.holdCmd(), "lift", target); err != nil {
		return nil, err
	}
	return map[string]any{"target": target, "lifted": cur != nil}, nil
}

// centralLane is a MergeLane: its repositories, its running and settling
// merges and its queue.
type centralLane struct {
	Name         string        `json:"name"`
	Repositories []string      `json:"repositories,omitempty"`
	Running      string        `json:"running,omitempty"`
	Settling     string        `json:"settling,omitempty"`
	Queue        []state.Merge `json:"queue"`
}

func toolLanes(c *call, _ mcp.CallToolRequest) (any, error) {
	lanes, err := c.laneList()
	return map[string]any{lanesName: lanes}, err
}

func (c *call) laneList() ([]centralLane, error) {
	ls, err := c.s.store.Lanes()
	if err != nil {
		return nil, err
	}
	st, err := c.app.store.Read()
	if err != nil {
		return nil, err
	}
	views := make([]centralLane, len(ls))
	for i, l := range ls {
		v := centralLane{Name: l.Name, Repositories: l.Spec.Repositories, Queue: []state.Merge{}}
		for _, m := range st.Merges {
			if m.Lane != l.Name {
				continue
			}
			v.Queue = append(v.Queue, m)
			switch m.Phase {
			case state.Running:
				v.Running = m.Key()
			case state.Settling:
				v.Settling = m.Key()
			}
		}
		views[i] = v
	}
	if len(views) == 0 {
		_, _ = fmt.Fprintln(c.out, "no merge lanes")
		return views, nil
	}
	w := c.app.table()
	_, _ = fmt.Fprintln(w, "LANE\tRUNNING\tSETTLING\tWAITING")
	for _, v := range views {
		var waiting []string
		for _, m := range v.Queue {
			if m.Phase == state.Waiting {
				waiting = append(waiting, fmt.Sprintf("%s (%s)", m.Key(), partyName(m.By)))
			}
		}
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", v.Name, dash(v.Running), dash(v.Settling), dash(strings.Join(waiting, ", ")))
	}
	_ = w.Flush()
	return views, nil
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// laneOf is the MergeLane a repository's merges queue in.
func (c *call) laneOf(repo string) (string, error) {
	if err := c.lanes(); err != nil {
		return "", err
	}
	l := c.app.cfg.LaneOf(repo)
	if _, ok := c.app.cfg.LaneNamed(l.Name); !ok {
		return "", refused("no MergeLane carries %s", repo)
	}
	c.concern = kube.LaneObject(l.Name)
	return l.Name, nil
}

// laneTurn is where a merge stands in its lane.
type laneTurn struct {
	Lane     string   `json:"lane"`
	Merge    string   `json:"merge"`
	Phase    string   `json:"phase"`
	Position int      `json:"position"`
	Turn     bool     `json:"turn"`
	Ahead    []string `json:"ahead"`
}

func turnOf(st *state.State, lane, repo string, pr int) (laneTurn, bool) {
	t := laneTurn{Lane: lane, Merge: fmt.Sprintf("%s#%d", repo, pr), Ahead: []string{}}
	busy := false
	for _, m := range st.Merges {
		if m.Lane != lane {
			continue
		}
		if strings.EqualFold(m.Repo, repo) && m.PR == pr {
			t.Phase, t.Position = m.Phase, len(t.Ahead)+1
			t.Turn = m.Phase == state.Waiting && len(t.Ahead) == 0 && !busy
			return t, true
		}
		if m.Phase != state.Waiting {
			busy = true
		}
		t.Ahead = append(t.Ahead, m.Key())
	}
	return t, false
}

func toolLaneQueue(c *call, req mcp.CallToolRequest) (any, error) {
	repo, pr, err := prArg(req)
	if err != nil {
		return nil, err
	}
	lane, err := c.laneOf(repo)
	if err != nil {
		return nil, err
	}
	var t laneTurn
	queued := false
	err = c.app.store.Update(func(st *state.State) ([]state.Event, error) {
		var ok bool
		if t, ok = turnOf(st, lane, repo, pr); ok {
			return nil, nil
		}
		st.Merges = append(st.Merges, state.Merge{Repo: repo, PR: pr, Lane: lane, By: c.me, Phase: state.Waiting, Joined: c.app.now.UTC()})
		t, _ = turnOf(st, lane, repo, pr)
		queued = true
		return []state.Event{event(c.me, "merge.queue", "%s#%d in lane %s, number %d", repo, pr, lane, t.Position)}, nil
	})
	if err != nil {
		return nil, err
	}
	if queued {
		_, err = fmt.Fprintf(c.out, "queued %s in lane %s, number %d\n", t.Merge, lane, t.Position)
	} else {
		_, err = fmt.Fprintf(c.out, "%s is already number %d in lane %s (%s)\n", t.Merge, t.Position, lane, t.Phase)
	}
	return t, err
}

func toolLaneTurn(c *call, req mcp.CallToolRequest) (any, error) {
	repo, pr, err := prArg(req)
	if err != nil {
		return nil, err
	}
	lane, err := c.laneOf(repo)
	if err != nil {
		return nil, err
	}
	st, err := c.app.store.Read()
	if err != nil {
		return nil, err
	}
	t, ok := turnOf(st, lane, repo, pr)
	switch {
	case !ok:
		return nil, refused("%s is not queued in lane %s: lane_queue first", t.Merge, lane)
	case t.Turn:
		_, err = fmt.Fprintf(c.out, "your turn: %s merges next in lane %s\n", t.Merge, lane)
	case t.Phase != state.Waiting:
		_, err = fmt.Fprintf(c.out, "%s is %s in lane %s\n", t.Merge, t.Phase, lane)
	case len(t.Ahead) == 0:
		_, err = fmt.Fprintf(c.out, "%s is next in lane %s once its running or settling merge frees it\n", t.Merge, lane)
	default:
		_, err = fmt.Fprintf(c.out, "%s is number %d in lane %s, behind %s\n", t.Merge, t.Position, lane, strings.Join(t.Ahead, ", "))
	}
	return t, err
}

func toolLaneSettle(c *call, req mcp.CallToolRequest) (any, error) {
	repo, pr, err := prArg(req)
	if err != nil {
		return nil, err
	}
	lane, err := c.laneOf(repo)
	if err != nil {
		return nil, err
	}
	merged := req.GetBool("merged", false)
	key := fmt.Sprintf("%s#%d", repo, pr)
	var t laneTurn
	err = c.app.store.Update(func(st *state.State) ([]state.Event, error) {
		i := -1
		for j, m := range st.Merges {
			switch {
			case m.Lane != lane:
			case strings.EqualFold(m.Repo, repo) && m.PR == pr:
				i = j
			case m.Phase != state.Waiting:
				return nil, refused("lane %s is busy: %s is %s", lane, m.Key(), m.Phase)
			}
		}
		m := state.Merge{Repo: repo, PR: pr, Lane: lane, By: c.me, Joined: c.app.now.UTC()}
		if i >= 0 {
			m = st.Merges[i]
			st.Merges = slices.Delete(st.Merges, i, i+1)
		}
		m.Phase, m.Started = state.Running, cmp.Or(m.Started, c.app.now.UTC())
		if merged {
			m.Phase, m.Finished = state.Settling, c.app.now.UTC()
		}
		// The lane's head: before every other merge of the lane.
		at := slices.IndexFunc(st.Merges, func(x state.Merge) bool { return x.Lane == lane })
		if at < 0 {
			at = len(st.Merges)
		}
		st.Merges = slices.Insert(st.Merges, at, m)
		t, _ = turnOf(st, lane, repo, pr)
		return []state.Event{event(c.me, "merge.settle", "%s outside the gate in lane %s, %s", key, lane, m.Phase)}, nil
	})
	if err != nil {
		return nil, err
	}
	if merged {
		_, err = fmt.Fprintf(c.out, "%s merged outside the gate: lane %s settles until its release rolled\n", key, lane)
	} else {
		_, err = fmt.Fprintf(c.out, "%s heads lane %s outside the gate until it merges: the lane's other merges wait\n", key, lane)
	}
	return t, err
}

func toolNoteAdd(c *call, req mcp.CallToolRequest) (any, error) {
	text, err := required(req, "text", "the question or the memo")
	if err != nil {
		return nil, err
	}
	args := []string{"add", text}
	for _, f := range [][2]string{{"for", "for"}, {"kind", "kind"}, {"due", "due"}, {"default", "default"}, {"status_quo", "status-quo"}, {"why", "why"}, {"checked", "checked"}} {
		if v := req.GetString(f[0], ""); v != "" {
			args = append(args, "--"+f[1], v)
		}
	}
	for _, o := range req.GetStringSlice("options", nil) {
		args = append(args, "--option", o)
	}
	for _, r := range req.GetStringSlice("refs", nil) {
		args = append(args, "--ref", r)
	}
	if req.GetBool("pin", false) {
		args = append(args, "--pin")
	}
	if r := req.GetInt("recommend", 0); r != 0 {
		args = append(args, "--recommend", strconv.Itoa(r))
	}
	if err := c.verb(c.app.noteCmd(), args...); err != nil {
		return nil, err
	}
	var id int
	if _, err := fmt.Sscanf(c.out.String(), "note #%d", &id); err != nil {
		return nil, fmt.Errorf("the note's number in %q: %w", c.out.String(), err)
	}
	c.concern = kube.NoteObject(c.me.Team, id)
	n, err := c.note(id)
	if err != nil || n == nil || n.Posted != "" {
		return n, err // a folded note was posted with its first filing
	}
	if err := c.s.postDecision(c.ctx, c.me, n); err != nil {
		return nil, err
	}
	return n, nil
}

func (c *call) note(id int) (*state.Note, error) {
	st, err := c.app.store.Read()
	if err != nil {
		return nil, err
	}
	for _, n := range st.Notes {
		if n.ID == id {
			return &n, nil
		}
	}
	return nil, nil
}

func toolNoteList(c *call, _ mcp.CallToolRequest) (any, error) {
	notes, err := c.noteList()
	return map[string]any{keyNotes: notes}, err
}

func (c *call) noteList() ([]state.Note, error) {
	if err := c.verb(c.app.noteCmd(), "list"); err != nil {
		return nil, err
	}
	st, err := c.app.store.Read()
	if err != nil {
		return nil, err
	}
	notes := slices.Clone(st.Notes)
	for i := range notes {
		notes[i].Kind = noteKind(c.app.cfg.Guide.Person, &notes[i])
	}
	return append([]state.Note{}, notes...), nil
}

func toolNoteAnswer(c *call, req mcp.CallToolRequest) (any, error) {
	id := req.GetInt(paramNote, 0)
	if id <= 0 {
		return nil, usageErr("note is required: the note's number")
	}
	choice := req.GetInt(paramChoice, 0)
	text := strings.TrimSpace(req.GetString(paramText, ""))
	n, err := c.note(id)
	if err != nil {
		return nil, err
	}
	if n != nil {
		c.concern = kube.NoteObject(n.By.Team, id)
		if !decidesNote(c.s.cfg.Serve, n, c.who) {
			return nil, refused("note #%d is for %s: only its addressee answers it", id, cmp.Or(n.For, "nobody named"))
		}
	}
	args := []string{"answer", strconv.Itoa(id), "--via", cmp.Or(req.GetString(paramVia, ""), viaCLI)}
	if choice != 0 {
		args = append(args, "--choice", strconv.Itoa(choice))
	}
	if text != "" {
		args = append(args, text)
	}
	if err := c.verb(c.app.noteCmd(), args...); err != nil {
		return nil, err
	}
	answer, _ := answerText(n, choice, text) // the verb took it
	c.s.closeDecision(c.ctx, n, outcomeAnswered, answer)
	return map[string]any{"note": id, "answer": answer, "answeredBy": c.who.Email}, nil
}

func toolNoteDone(c *call, req mcp.CallToolRequest) (any, error) {
	id := req.GetInt(paramNote, 0)
	if id <= 0 {
		return nil, usageErr("note is required: the note's number")
	}
	n, err := c.note(id)
	if err != nil {
		return nil, err
	}
	if n == nil {
		return nil, refused("note #%d is not open", id)
	}
	c.concern = kube.NoteObject(n.By.Team, id)
	if err := c.may(fmt.Sprintf("note #%d", id), n.By); err != nil {
		return nil, err
	}
	if err := c.verb(c.app.noteCmd(), "done", strconv.Itoa(id)); err != nil {
		return nil, err
	}
	c.s.closeDecision(c.ctx, n, outcomeWithdrawn, "")
	_, err = fmt.Fprintf(c.out, "note #%d done\n", id)
	return map[string]any{"note": id}, err
}

func toolAgentsRegister(c *call, req mcp.CallToolRequest) (any, error) {
	if _, err := required(req, paramAgent, "the name to register under"); err != nil {
		return nil, err
	}
	if _, err := required(req, paramHost, "the machine or installation the agent runs on"); err != nil {
		return nil, err
	}
	c.concern = kube.RosterObject(c.me)
	task := ""
	err := c.app.store.Update(func(st *state.State) ([]state.Event, error) {
		for i, x := range st.Agents {
			if !strings.EqualFold(x.Name, c.me.Name) || x.Host != c.me.Host {
				continue
			}
			// An address, local:<host>/<name>, names one agent.
			if x.Person != c.me.Person {
				return nil, refused("%s on %s is %s's agent: register under another name", x.Name, x.Host, cmp.Or(x.Person, x.Team))
			}
			// Registered again: the call's audit Event records it, since
			// within the same second nothing the resource stores changes.
			task = x.Task
			st.Agents[i].Party, st.Agents[i].Registered = c.me, c.app.now.UTC()
			return nil, nil
		}
		st.Agents = append(st.Agents, state.Agent{Party: c.me, Registered: c.app.now.UTC(), IdleSince: c.app.now.UTC()})
		return []state.Event{event(c.me, "agents.register", "%s on %s", c.me.Name, c.me.Host)}, nil
	})
	if err != nil {
		return nil, err
	}
	if task != "" {
		_, err = fmt.Fprintf(c.out, "register: %s busy with %q: work it, then report it idle\n", c.me.Name, task)
	} else {
		_, err = fmt.Fprintf(c.out, "register: %s idle, ready for a task\n", c.me.Name)
	}
	return map[string]any{"agent": c.me, "task": task}, err
}

func toolListAgents(c *call, req mcp.CallToolRequest) (any, error) {
	scope := req.GetString("scope", "all")
	if scope != "all" && scope != "team" {
		return nil, usageErr("scope %q: all or team", scope)
	}
	agents, err := c.agentList(scope == "team")
	return map[string]any{agentsName: agents}, err
}

func (c *call) agentList(team bool) ([]addressedAgent, error) {
	st, err := c.app.store.Read()
	if err != nil {
		return nil, err
	}
	agents := []addressedAgent{}
	for _, ag := range st.Agents {
		if !team || ag.Team == c.me.Team {
			agents = append(agents, addressed(ag))
		}
	}
	if len(agents) == 0 {
		_, _ = fmt.Fprintln(c.out, "no agent is registered")
		return agents, nil
	}
	w := c.app.table()
	_, _ = fmt.Fprintln(w, "ADDRESS\tPERSON\tTEAM\tSTATE\tTASK")
	for _, ag := range agents {
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", ag.Address, dash(ag.Person), dash(ag.Team), ag.State, dash(truncate(ag.Task, 60)))
	}
	_ = w.Flush()
	return agents, nil
}

// snapshotView is everything the central state holds.
type snapshotView struct {
	Leases *leaseListView   `json:"leases"`
	Holds  []state.Hold     `json:"holds"`
	Lanes  []centralLane    `json:"lanes"`
	Notes  []state.Note     `json:"notes"`
	Agents []addressedAgent `json:"agents"`
}

func toolSnapshot(c *call, _ mcp.CallToolRequest) (any, error) {
	var v snapshotView
	var err error
	section := func(title string, fill func() error) {
		if err != nil {
			return
		}
		_, _ = fmt.Fprintf(c.out, "== %s\n", title)
		err = fill()
		_, _ = fmt.Fprintln(c.out)
	}
	section("Leases", func() (e error) { v.Leases, e = c.leaseList(); return })
	section("Holds", func() (e error) { v.Holds, e = c.holdList(); return })
	section("Lanes", func() (e error) { v.Lanes, e = c.laneList(); return })
	section("Notes", func() (e error) { v.Notes, e = c.noteList(); return })
	section("Agents", func() (e error) { v.Agents, e = c.agentList(false); return })
	return v, err
}
