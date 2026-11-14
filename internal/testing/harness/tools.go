package harness

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	dto "github.com/prometheus/client_model/go"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/giantswarm/beekeeper/api/v1alpha1"
	"github.com/giantswarm/beekeeper/internal/history"
	"github.com/giantswarm/beekeeper/internal/receiver"
	fakegithub "github.com/giantswarm/beekeeper/internal/testing/fakes/github"
	fakejev "github.com/giantswarm/beekeeper/internal/testing/fakes/jev"
)

// The step tools of contract §4.2.

// Arguments several step tools take, which the loader checks too.
const (
	argItem      = "item"
	argChange    = "change"
	argPoint     = "point"
	argKind      = "kind"
	argName      = "name"
	argRef       = "ref"
	argType      = "type"
	argSource    = "source"
	argSinceStep = "since_step"
	argTeams     = "teams"
	argPeople    = "people"
)

// resultCount is the count the read tools answer with.
const resultCount = "count"

// toolClockAdvance moves the fake clock forward.
const toolClockAdvance = "clock_advance"

// jevTools are dispatched to the fake JEV.
var jevTools = map[string][]string{
	fakejev.ToolSet:   {argPoint, argItem, "person", "answer", "confidence", "error"},
	fakejev.ToolCalls: {argPoint, argSinceStep},
}

func init() {
	// The fake GitHub lists the arguments it understands, so the loader checks them against it.
	for name, args := range fakegithub.Tools {
		name := name
		Register(name, Tool{Args: args, Run: func(ctx context.Context, w *World, a map[string]any) (any, error) {
			if w.GitHub == nil {
				return nil, fmt.Errorf("no fake GitHub")
			}
			return w.GitHub.Do(ctx, name, a)
		}})
	}
	for name, args := range jevTools {
		name := name
		Register(name, Tool{Args: args, Run: func(ctx context.Context, w *World, a map[string]any) (any, error) {
			if w.JEV == nil {
				return nil, fmt.Errorf("no fake JEV")
			}
			return w.JEV.Do(ctx, name, a)
		}})
	}

	Register("webhook_post", Tool{Args: []string{"event", "action", "body", "signature", "delivery_id", "size_bytes", "method", "path"}, Run: webhookPost})
	Register(toolClockAdvance, Tool{Args: []string{"by"}, Run: func(ctx context.Context, w *World, a map[string]any) (any, error) {
		d, err := time.ParseDuration(fmt.Sprint(a["by"]))
		if err != nil {
			return nil, err
		}
		if err := w.AdvanceClock(ctx, d); err != nil {
			return nil, err
		}
		return map[string]any{"now": w.Clock.Now()}, nil
	}})
	Register("settle", Tool{Args: []string{}, Run: func(ctx context.Context, w *World, _ map[string]any) (any, error) {
		if err := w.Settle(ctx); err != nil {
			return nil, err
		}
		return map[string]any{"settled": true}, nil
	}})
	Register("config_apply", Tool{Args: []string{argTeams, argPeople, "installation"}, Run: configApply})
	Register("engine_fault", Tool{Args: []string{argKind}, Run: engineFault})
	Register("postgres_fault", Tool{Args: []string{argKind}, Run: postgresFault})

	Register("get_item", Tool{Args: []string{argRef}, Run: func(ctx context.Context, w *World, a map[string]any) (any, error) {
		ref := str(a, argRef)
		return getObject(ctx, w, &v1alpha1.Item{}, v1alpha1.ItemName(ref), "item "+ref)
	}})
	Register("get_change", Tool{Args: []string{argRef}, Run: func(ctx context.Context, w *World, a map[string]any) (any, error) {
		ref := str(a, argRef)
		return getObject(ctx, w, &v1alpha1.Change{}, v1alpha1.ChangeName(ref), "change "+ref)
	}})
	Register("get_team", Tool{Args: []string{argName}, Run: func(ctx context.Context, w *World, a map[string]any) (any, error) {
		return getObject(ctx, w, &v1alpha1.Team{}, str(a, argName), "team "+str(a, argName))
	}})
	Register("get_person", Tool{Args: []string{"login"}, Run: func(ctx context.Context, w *World, a map[string]any) (any, error) {
		return getObject(ctx, w, &v1alpha1.Person{}, v1alpha1.PersonName(str(a, "login")), "person "+str(a, "login"))
	}})
	Register("get_installation", Tool{Args: []string{}, Run: func(ctx context.Context, w *World, _ map[string]any) (any, error) {
		org, _ := w.Config.Installation["org"].(string)
		return getObject(ctx, w, &v1alpha1.Installation{}, org, "installation "+org)
	}})
	Register("get_budget", Tool{Args: []string{}, Run: func(ctx context.Context, w *World, _ map[string]any) (any, error) {
		return getObject(ctx, w, &v1alpha1.Budget{}, v1alpha1.BudgetName, "budget "+v1alpha1.BudgetName)
	}})
	Register("list_items", Tool{Args: []string{"team", "phase", "eligible", "class"}, Run: listItems})
	Register("events", Tool{Args: []string{argItem, argChange, argType, argSource, argSinceStep}, Run: events})
	Register("decisions", Tool{Args: []string{argPoint, argItem, argSinceStep}, Run: decisions})
	Register("metric", Tool{Args: []string{argName, "labels"}, Run: func(_ context.Context, w *World, a map[string]any) (any, error) {
		labels, _ := a["labels"].(map[string]any)
		v, n, err := w.metric(str(a, argName), labels)
		if err != nil {
			return nil, err
		}
		return map[string]any{"value": v, "series": n}, nil
	}})
	Register("reconciles", Tool{Args: []string{argItem, argChange, "replica", argSinceStep}, Run: reconciles})
	Register("object_writes", Tool{Args: []string{"replica", argSinceStep}, Run: objectWrites})
	Register("replay", Tool{Args: []string{}, Run: replay})
}

func str(a map[string]any, k string) string {
	if v, ok := a[k]; ok && v != nil {
		return fmt.Sprint(v)
	}
	return ""
}

// getObject reads an object from the API server as JSON, without its managed fields.
func getObject(ctx context.Context, w *World, obj client.Object, name, what string) (any, error) {
	if w.Kube == nil {
		return nil, fmt.Errorf("no API server")
	}
	err := w.Kube.Get(ctx, client.ObjectKey{Namespace: w.Namespace, Name: name}, obj)
	if apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("%s: %w", what, errNotFound)
	}
	if err != nil {
		return nil, err
	}
	doc, err := Normalize(obj)
	if err != nil {
		return nil, err
	}
	if m, ok := doc.(map[string]any); ok {
		if md, ok := m["metadata"].(map[string]any); ok {
			delete(md, "managedFields")
		}
	}
	return doc, nil
}

// itemRef is an Item's issue ref.
func itemRef(it *v1alpha1.Item) string {
	if it.Status.Issue != nil && it.Status.Issue.Repo != "" {
		return fmt.Sprintf("%s#%d", it.Status.Issue.Repo, it.Status.Issue.Number)
	}
	return it.Name
}

func listItems(ctx context.Context, w *World, a map[string]any) (any, error) {
	if w.Kube == nil {
		return nil, fmt.Errorf("no API server")
	}
	var list v1alpha1.ItemList
	if err := w.Kube.List(ctx, &list, client.InNamespace(w.Namespace)); err != nil {
		return nil, err
	}
	team := str(a, "team")
	option := ""
	if team != "" {
		teams, err := w.Config.TeamConfigs()
		if err != nil {
			return nil, err
		}
		for _, t := range teams {
			if t.Name == team {
				option = t.BoardOption
			}
		}
	}
	refs := []string{}
	for i := range list.Items {
		it := &list.Items[i]
		st := it.Status
		if team != "" {
			if st.InScope != nil && !*st.InScope {
				continue
			}
			byLabel := it.Labels[v1alpha1.LabelTeam] == team
			byBoard := option != "" && st.Board != nil && st.Board.Team == option
			if !byLabel && !byBoard {
				continue
			}
		}
		if p := str(a, "phase"); p != "" && st.Phase != p {
			continue
		}
		if c := str(a, "class"); c != "" && st.Class != c {
			continue
		}
		if e, ok := a["eligible"]; ok {
			want := fmt.Sprint(e) == "true"
			got := st.Eligible != nil && st.Eligible.Value
			if want != got {
				continue
			}
		}
		refs = append(refs, itemRef(it))
	}
	sort.Strings(refs)
	return map[string]any{resultCount: len(refs), "items": refs}, nil
}

func events(ctx context.Context, w *World, a map[string]any) (any, error) {
	if w.History == nil {
		return nil, fmt.Errorf("no history")
	}
	m, err := w.markOf(a)
	if err != nil {
		return nil, err
	}
	f := history.EventFilter{Item: str(a, argItem), Change: str(a, argChange), Type: str(a, argType), Source: str(a, argSource)}
	if m != nil {
		f.AfterID = m.events
	}
	evs, err := w.History.Events(ctx, f)
	if err != nil {
		return nil, err
	}
	rows := make([]any, 0, len(evs))
	for _, e := range evs {
		row := map[string]any{"type": e.Type, "source": e.Source, "at": e.At.UTC(), "data": e.Data}
		if e.Item != "" {
			row["item"] = e.Item
		}
		if e.Change != "" {
			row["change"] = e.Change
		}
		rows = append(rows, row)
	}
	return map[string]any{resultCount: len(rows), "rows": rows}, nil
}

func decisions(ctx context.Context, w *World, a map[string]any) (any, error) {
	if w.History == nil {
		return nil, fmt.Errorf("no history")
	}
	m, err := w.markOf(a)
	if err != nil {
		return nil, err
	}
	f := history.DecisionFilter{Point: str(a, argPoint), Subject: str(a, argItem)}
	if m != nil {
		f.AfterID = m.decisions
	}
	ds, err := w.History.Decisions(ctx, f)
	if err != nil {
		return nil, err
	}
	rows := make([]any, 0, len(ds))
	for _, d := range ds {
		row := map[string]any{
			"point": d.Point, "subject": d.Subject, "other": d.Other, "rule": d.Rule, "jev": d.JEV,
			"jevConfidence": d.JEVConfidence, "jevError": d.JEVError, "chosen": d.Chosen, "mode": d.Mode,
			"outcome": d.Outcome, "inputsHash": d.InputsHash, "at": d.At.UTC(),
		}
		if strings.Contains(d.Subject, "#") {
			row["item"] = d.Subject
		}
		rows = append(rows, row)
	}
	return map[string]any{resultCount: len(rows), "rows": rows}, nil
}

// sample is one metric series.
type sample struct {
	labels map[string]string
	value  float64
}

// series returns the series of name across the running replicas and the receiver that match labels.
func (w *World) series(name string, labels map[string]any) ([]sample, error) {
	var out []sample
	var errs []string
	for _, g := range w.gatherers() {
		fams, err := g.Gather()
		if err != nil {
			errs = append(errs, err.Error())
		}
		for _, f := range fams {
			if f.GetName() != name {
				continue
			}
			for _, m := range f.GetMetric() {
				ls := map[string]string{}
				for _, lp := range m.GetLabel() {
					ls[lp.GetName()] = lp.GetValue()
				}
				if !labelsMatch(ls, labels) {
					continue
				}
				out = append(out, sample{labels: ls, value: metricValue(f.GetType(), m)})
			}
		}
	}
	if len(errs) > 0 {
		return out, fmt.Errorf("gathering metrics: %s", strings.Join(errs, "; "))
	}
	return out, nil
}

// metric sums the series of name that match labels.
func (w *World) metric(name string, labels map[string]any) (float64, int, error) {
	ss, err := w.series(name, labels)
	if err != nil {
		return 0, 0, err
	}
	var sum float64
	for _, s := range ss {
		sum += s.value
	}
	return sum, len(ss), nil
}

func labelsMatch(have map[string]string, want map[string]any) bool {
	for k, v := range want {
		if !scalarEqual(scalarString(v), have[k]) {
			return false
		}
	}
	return true
}

func metricValue(t dto.MetricType, m *dto.Metric) float64 {
	switch t {
	case dto.MetricType_COUNTER:
		return m.GetCounter().GetValue()
	case dto.MetricType_GAUGE:
		return m.GetGauge().GetValue()
	case dto.MetricType_HISTOGRAM:
		return float64(m.GetHistogram().GetSampleCount())
	case dto.MetricType_SUMMARY:
		return float64(m.GetSummary().GetSampleCount())
	default:
		return m.GetUntyped().GetValue()
	}
}

func (w *World) replicasOf(a map[string]any) ([]*Replica, error) {
	if name := str(a, "replica"); name != "" {
		r, err := w.Replica(name)
		if err != nil {
			return nil, err
		}
		return []*Replica{r}, nil
	}
	return w.Replicas, nil
}

func reconciles(_ context.Context, w *World, a map[string]any) (any, error) {
	m, err := w.markOf(a)
	if err != nil {
		return nil, err
	}
	rs, err := w.replicasOf(a)
	if err != nil {
		return nil, err
	}
	k := statKey{}
	switch {
	case str(a, argItem) != "":
		k = statKey{"Item", str(a, argItem)}
	case str(a, argChange) != "":
		k = statKey{"Change", str(a, argChange)}
	}
	n := 0
	for _, r := range rs {
		n += r.Reconciles(k)
		if m != nil {
			n -= m.reconciles[r.Name][k]
		}
	}
	return map[string]any{resultCount: n}, nil
}

func objectWrites(_ context.Context, w *World, a map[string]any) (any, error) {
	m, err := w.markOf(a)
	if err != nil {
		return nil, err
	}
	rs, err := w.replicasOf(a)
	if err != nil {
		return nil, err
	}
	n := 0
	for _, r := range rs {
		n += r.Writes()
		if m != nil {
			n -= m.writes[r.Name]
		}
	}
	return map[string]any{resultCount: n}, nil
}

func replay(ctx context.Context, w *World, _ map[string]any) (any, error) {
	r := w.Leader()
	if r == nil {
		for _, x := range w.Replicas {
			if x.Running() {
				r = x
				break
			}
		}
	}
	if r == nil {
		return nil, fmt.Errorf("no running replica")
	}
	rp, ok := r.Engine().(Replayer)
	if !ok {
		return nil, fmt.Errorf("%s can't replay", r.Name)
	}
	d, mm, err := rp.Replay(ctx)
	if err != nil {
		return nil, err
	}
	return map[string]any{"decisions": d, "mismatches": mm}, nil
}

func engineFault(ctx context.Context, w *World, a map[string]any) (any, error) {
	kind := str(a, argKind)
	switch kind {
	case "kill_leader":
		r := w.Leader()
		if r == nil {
			return nil, fmt.Errorf("no replica leads")
		}
		if err := r.Stop(w.tracked); err != nil {
			return nil, err
		}
		return map[string]any{"killed": r.Name}, nil
	case "restart":
		for i := len(w.Replicas) - 1; i >= 0; i-- {
			if err := w.Replicas[i].Stop(w.tracked); err != nil {
				return nil, err
			}
		}
		if w.startReplicas == nil {
			return nil, fmt.Errorf("replicas can't be restarted")
		}
		if err := w.startReplicas(ctx); err != nil {
			return nil, err
		}
		return map[string]any{"restarted": len(w.Replicas)}, nil
	default:
		for _, r := range w.Replicas {
			if !r.Faults.Set(kind) {
				return nil, fmt.Errorf("unknown engine fault %q", kind)
			}
		}
		return map[string]any{"fault": kind}, nil
	}
}

func postgresFault(_ context.Context, w *World, a map[string]any) (any, error) {
	if w.DB == nil {
		return nil, fmt.Errorf("no database")
	}
	switch str(a, argKind) {
	case "down":
		if err := w.DB.Down(); err != nil {
			return nil, err
		}
		w.setDBDown(true)
	case "up":
		if err := w.DB.Up(); err != nil {
			return nil, err
		}
		w.setDBDown(false)
	default:
		return nil, fmt.Errorf("unknown postgres fault %q", str(a, argKind))
	}
	return map[string]any{"postgres": str(a, argKind)}, nil
}

// webhookPost sends a raw request to the receiver (contract §4.2): POST to the receiver's path by
// default, the action placed in the body, a generated delivery id, and size_bytes padding the body
// (with JSON whitespace) inside the signature.
func webhookPost(ctx context.Context, w *World, a map[string]any) (any, error) {
	if w.Receiver.URL == "" {
		return nil, fmt.Errorf("no receiver")
	}
	body := map[string]any{}
	if b, ok := a["body"].(map[string]any); ok {
		body = b
	} else if a["body"] != nil {
		return nil, fmt.Errorf("body must be a map")
	}
	if act := str(a, "action"); act != "" {
		body["action"] = act
	}
	raw, err := marshal(body)
	if err != nil {
		return nil, err
	}
	if sz, ok := a["size_bytes"]; ok {
		n, isNum := toFloat(sz)
		if !isNum || int(n) < len(raw) {
			return nil, fmt.Errorf("size_bytes %v is smaller than the body (%d bytes)", sz, len(raw))
		}
		raw = append(raw, bytes.Repeat([]byte{' '}, int(n)-len(raw))...)
	}
	method := str(a, "method")
	if method == "" {
		method = http.MethodPost
	}
	path := str(a, "path")
	if path == "" {
		path = w.Receiver.Path
	}
	id := str(a, "delivery_id")
	if id == "" {
		w.mu.Lock()
		w.deliveries++
		id = fmt.Sprintf("00000000-harness-%08d", w.deliveries)
		w.mu.Unlock()
	}
	req, err := http.NewRequestWithContext(ctx, method, w.Receiver.URL+path, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "GitHub-Hookshot/harness")
	req.Header.Set("X-GitHub-Delivery", id)
	if ev := str(a, "event"); ev != "" {
		req.Header.Set("X-GitHub-Event", ev)
	}
	switch sig := str(a, "signature"); sig {
	case "", "valid":
		req.Header.Set("X-Hub-Signature-256", receiver.Sign(w.Receiver.Secret, raw))
	case "invalid":
		req.Header.Set("X-Hub-Signature-256", receiver.Sign([]byte("not-the-secret"), raw))
	case "missing":
	default:
		return nil, fmt.Errorf("unknown signature %q", sig)
	}
	hc := w.Receiver.Client
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }() // drained below; closing can't lose anything
	_, _ = io.Copy(io.Discard, resp.Body)
	return map[string]any{"status_code": resp.StatusCode, "delivery_id": id}, nil
}
