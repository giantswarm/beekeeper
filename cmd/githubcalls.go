package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/guard"
	"github.com/giantswarm/beekeeper/internal/state"
)

// verbGitHubCall is the event of one admitted GraphQL invocation.
const verbGitHubCall = "github.call"
const verbGitHubRESTCall = "github.rest.call"

// Only admitted invocations are recorded. PreToolUse cannot prove execution
// or measure points, but retains callers after their processes have exited.
func (a *app) recordGitHubCalls(raw []byte, format string, output []byte) {
	var decision struct {
		Decision string `json:"decision"`
		Specific struct {
			Decision string `json:"permissionDecision"`
		} `json:"hookSpecificOutput"`
	}
	_ = json.Unmarshal(output, &decision)
	if decision.Decision == guardDeny || decision.Specific.Decision == guardDeny || a.cfg == nil {
		return
	}
	session, calls := guard.GitHubCalls(raw, format)
	_, restCalls := guard.GitHubRESTCalls(raw, format)
	if session == "" || len(calls)+len(restCalls) == 0 {
		return
	}
	store, err := state.Open(a.cfg.StateDir)
	if err != nil {
		return
	}
	who := state.Party{Session: session, Name: os.Getenv("BEEKEEPER_AGENT_NAME")}
	var events []state.Event
	for _, call := range calls {
		detail, _ := json.Marshal(call)
		events = append(events, state.Event{At: time.Now(), By: who, Verb: verbGitHubCall, Detail: string(detail)})
	}
	for _, call := range restCalls {
		detail, _ := json.Marshal(call)
		events = append(events, state.Event{At: time.Now(), By: who, Verb: verbGitHubRESTCall, Detail: string(detail)})
	}
	_ = store.RecordWithin(200*time.Millisecond, events...)
}

type githubCaller struct {
	Session string `json:"session"`
	Class   string `json:"class"`
	Calls   int    `json:"calls"`
	Heavy   int    `json:"heavy"`
}

func githubWindowCallers(a *app, g *github.GraphQL) ([]githubCaller, error) {
	if g == nil {
		return nil, nil
	}
	return githubResourceCallers(a, g.Reset, verbGitHubCall)
}

func githubResourceCallers(a *app, reset time.Time, verb string) ([]githubCaller, error) {
	if reset.IsZero() || !reset.After(a.now) {
		reset = a.now
	}
	since := reset.Add(-time.Hour)
	events, err := a.store.Events(0, func(e state.Event) bool {
		return e.Verb == verb && !e.At.Before(since) && !e.At.After(a.now)
	})
	if err != nil {
		return nil, err
	}
	var callers []githubCaller
	for _, e := range events {
		var call guard.GitHubCall
		if json.Unmarshal([]byte(e.Detail), &call) != nil || call.Class == "" {
			continue
		}
		session := e.By.Session
		if session == "" {
			session = e.By.HostSession
		}
		if session == "" {
			session = e.By.Name
		}
		i := slices.IndexFunc(callers, func(c githubCaller) bool { return c.Session == session && c.Class == call.Class })
		if i < 0 {
			callers = append(callers, githubCaller{Session: session, Class: call.Class})
			i = len(callers) - 1
		}
		callers[i].Calls++
		if call.Heavy {
			callers[i].Heavy++
		}
	}
	slices.SortFunc(callers, func(x, y githubCaller) int {
		if x.Heavy != y.Heavy {
			return y.Heavy - x.Heavy
		}
		if x.Calls != y.Calls {
			return y.Calls - x.Calls
		}
		if x.Session != y.Session {
			return strings.Compare(x.Session, y.Session)
		}
		return strings.Compare(x.Class, y.Class)
	})
	return callers, nil
}

func githubCallersText(callers []githubCaller) string {
	var parts []string
	for _, c := range callers {
		parts = append(parts, fmt.Sprintf("%s: %s (%d calls, %d heavy)", c.Session, c.Class, c.Calls, c.Heavy))
	}
	return strings.Join(parts, "; ")
}

// Internal calls survive short-lived gh processes too. The observer receives
// only a source function name, never endpoints, tokens or command arguments.
func (a *app) observeREST() {
	github.RESTObserver = func(class string) {
		detail, _ := json.Marshal(guard.GitHubCall{Class: class})
		who := envParty(os.Getenv)
		if who.Session == "" && who.HostSession == "" {
			who.Name = "beekeeper"
		}
		_ = a.store.RecordWithin(200*time.Millisecond, state.Event{
			At: time.Now(), By: who, Verb: verbGitHubRESTCall, Detail: string(detail),
		})
	}
}
