package cmd

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/go-logr/logr"
	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
	"github.com/spf13/cobra"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlconfig "sigs.k8s.io/controller-runtime/pkg/client/config"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/identity"
	"github.com/giantswarm/beekeeper/internal/mailbox"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/state/kube"
	"github.com/giantswarm/beekeeper/pkg/project"
)

func (a *app) centralCmd() *cobra.Command {
	var addr, kubeContext string
	c := &cobra.Command{
		Use:   "serve",
		Short: "Serve the coordination verbs as MCP tools behind muster: the central instance",
		Long: `serve speaks MCP over streamable HTTP on --http (path /mcp; /healthz for
probes) and offers the coordination verbs as tools over the
beekeeper.giantswarm.io resources of the cluster it runs in: leases, holds,
merge lanes, notes and the agent roster. Each tool returns the text the
command prints and the same as structured content.

Every request carries a Dex ID token (muster forwards the person's): it is
validated against serve.issuer's keys and serve.clientIDs, and the caller is
the token's verified email, a member of serve.organization, of the team its
group in serve.teams names. Releasing another person's lease or lifting
their hold is that person's, or the owning team's supervisor role's
(serve.supervisors). Every call leaves one Kubernetes Event on the resource
it concerns (the caller's team namespace for a list) and one log line.

list_agents, the roster and the feed show a person their own local agents,
their team's local agents that hold a lease (work on a shared
installation) and every remote agent; a send_message to a local agent is
its person's alone (docs/feed.md).

A decision (note_add --kind decision, --for a person of serve.people or an
email, or team:<name> of serve.channels) is put to its addressee as one
Slack message through klaus-gateway (serve.gateway, POST /decisions, with
the projected ServiceAccount token): a direct message, or the team's
channel. A click, the modal or a thread reply calls note_answer through
muster as the person who answered; only the addressee answers (the
person's email, or a member of the team's Dex group in serve.teams; the
first answer closes a team's). A decision klaus-gateway does not take is
withdrawn at once. Its message is closed on every path: answered, withdrawn
(note_done) or at its due time, when serve closes it with its default
(note.defaulted).`,
		Args: cobra.NoArgs,
		PersistentPreRunE: func(*cobra.Command, []string) error {
			return a.loadConfig()
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			rc, err := ctrlconfig.GetConfigWithContext(kubeContext)
			if err != nil {
				return err
			}
			store, err := kube.NewForConfig(rc)
			if err != nil {
				return err
			}
			ids, err := identity.New(ctx, a.cfg.Serve)
			if err != nil {
				return err
			}
			dsn := os.Getenv(databaseEnv)
			if dsn == "" {
				return usageErr("%s is not set: the URL of the beekeeper database the mailboxes live in", databaseEnv)
			}
			mail, err := mailbox.Open(ctx, dsn)
			if err != nil {
				return err
			}
			defer mail.Close()
			log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
			ctrllog.SetLogger(logr.FromSlogHandler(log.Handler()))
			s := newServer(a.cfg, store, mail, ids, log)
			if err := s.watch(ctx, rc); err != nil {
				return err
			}
			srv := &http.Server{Addr: addr, Handler: s.handler(), ReadHeaderTimeout: 10 * time.Second}
			go func() {
				<-ctx.Done()
				sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				_ = srv.Shutdown(sctx)
			}()
			log.Info("serving", "addr", addr, "version", project.Version())
			if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
				return err
			}
			return nil
		},
	}
	c.Flags().StringVar(&addr, "http", ":8080", "the address to serve MCP on")
	c.Flags().StringVar(&kubeContext, "context", "", "the kubeconfig context of the cluster the state lives in (default: in-cluster, else the current one)")
	return c
}

// databaseEnv names the URL of the beekeeper database: the mailboxes.
const databaseEnv = "BEEKEEPER_DATABASE_URL"

// server is beekeeper serve: the MCP tools and resources over the
// Kubernetes store and the mailboxes.
type server struct {
	cfg   *config.Config
	store *kube.Store
	mail  *mailbox.Store
	hub   *hub
	ids   *identity.Verifier
	log   *slog.Logger
	now   func() time.Time
	// gw puts the decisions to their addressee; nil keeps them here.
	gw *gateway
}

func newServer(cfg *config.Config, store *kube.Store, mail *mailbox.Store, ids *identity.Verifier, log *slog.Logger) *server {
	s := &server{cfg: cfg, store: store, mail: mail, hub: newHub(), ids: ids, log: log, now: time.Now, gw: newGateway(cfg.Serve.Gateway)}
	s.hub.dropped = func(who identity.Caller, uri string) {
		log.Warn("notify", "caller", who.Email, "uri", uri, "outcome", "dropped", "detail", "the stream's queue is full")
	}
	return s
}

// callerKey carries the authenticated caller, tokenKey the token it came
// with (forwarded to muster as the caller's).
type (
	callerKey struct{}
	tokenKey  struct{}
)

func (s *server) handler() http.Handler {
	m := mcpserver.NewMCPServer(project.Name, project.Version(), mcpserver.WithToolCapabilities(false),
		mcpserver.WithResourceCapabilities(true, false), mcpserver.WithHooks(s.subscriptionHooks()))
	for _, t := range append(s.tools(), s.messageTools()...) {
		m.AddTool(t.tool, s.handle(t))
	}
	s.addResources(m)
	mux := http.NewServeMux()
	mux.Handle("/mcp", s.authenticate(mcpserver.NewStreamableHTTPServer(m, mcpserver.WithStateLess(true))))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	return mux
}

// authenticate refuses a request without a valid token of a member, before
// MCP sees it, and hands the caller on to the tools.
func (s *server) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		who, err := s.ids.Verify(r.Context(), strings.TrimSpace(raw))
		if err != nil {
			s.log.Info("call", "caller", "", "tool", "", "outcome", "refused", "detail", err.Error())
			w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token"`)
			http.Error(w, err.Error(), http.StatusUnauthorized)
			return
		}
		ctx := context.WithValue(context.WithValue(r.Context(), callerKey{}, who), tokenKey{}, strings.TrimSpace(raw))
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// serveTool is one tool and what it runs: it writes the command's text to
// the call's out and returns the structured content.
type serveTool struct {
	tool mcp.Tool
	run  func(c *call, req mcp.CallToolRequest) (any, error)
}

// call is one tool call: the caller, the verb's app over the store, and the
// record the call concerns.
type call struct {
	s     *server
	ctx   context.Context
	who   identity.Caller
	me    state.Party
	app   *app
	out   *bytes.Buffer
	store *auditStore
	// concern is the record the call's Event goes on; nil: the caller's team
	// namespace.
	concern client.Object
}

// auditStore is the store a call's verb writes through: it notes whether
// the write recorded the call's Event.
type auditStore struct {
	*kube.Store
	wrote bool
}

func (s *auditStore) Update(fn func(*state.State) ([]state.Event, error)) error {
	n := 0
	err := s.Store.Update(func(st *state.State) ([]state.Event, error) {
		evs, err := fn(st)
		n = len(evs)
		return evs, err
	})
	if err == nil && n > 0 {
		s.wrote = true
	}
	return err
}

func (s *server) newCall(ctx context.Context, who identity.Caller, req mcp.CallToolRequest) *call {
	me := state.Party{Name: cmp.Or(strings.TrimSpace(req.GetString(paramAgent, "")), who.Email), Person: who.Email, Team: who.Team, Host: strings.TrimSpace(req.GetString(paramHost, ""))}
	cfg := *s.cfg
	cfg.Identity = config.Identity{Person: me.Person, Team: me.Team, Host: me.Host}
	st := &auditStore{Store: s.store}
	out := &bytes.Buffer{}
	return &call{s: s, ctx: ctx, who: who, me: me, store: st, out: out,
		app: &app{cfg: &cfg, as: me.Name, store: st, now: s.now(), out: out, central: true}}
}

// handle runs a tool for the authenticated caller and audits it: the Event
// the write recorded, else one on the record the call concerns, and one log
// line either way.
func (s *server) handle(t serveTool) mcpserver.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		who, ok := ctx.Value(callerKey{}).(identity.Caller)
		if !ok {
			return mcp.NewToolResultError("no authenticated caller"), nil
		}
		c := s.newCall(ctx, who, req)
		structured, err := t.run(c, req)
		text := strings.TrimRight(c.out.String(), "\n")
		outcome := "ok"
		if err != nil {
			outcome = "error"
			if code := Code(err); code == ExitRefused || code == ExitUsage {
				outcome = "refused"
			}
			text = strings.TrimLeft(text+"\n"+err.Error(), "\n")
		}
		if !c.store.wrote {
			ev := state.Event{At: c.app.now.UTC(), By: c.me, Verb: "serve." + t.tool.Name, Detail: outcome + ": " + firstLine(text)}
			if aerr := s.store.Audit(c.concern, who.Team, ev); aerr != nil {
				text += "\naudit: " + aerr.Error()
				if err == nil {
					outcome, err = "error", aerr
				}
			}
		}
		s.log.Info("call", "caller", who.Email, "team", who.Team, "agent", c.me.Name, "tool", t.tool.Name, "outcome", outcome, "detail", firstLine(text))
		if err != nil {
			res := mcp.NewToolResultError(text)
			res.StructuredContent = map[string]string{"outcome": outcome, "message": text}
			return res, nil
		}
		if structured == nil {
			structured = map[string]string{"message": text}
		}
		return mcp.NewToolResultStructured(structured, text), nil
	}
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}

// verb runs one of the command line's verbs on the call's app, its text
// into the call's out.
func (c *call) verb(cmd *cobra.Command, args ...string) error {
	usageArgs(cmd)
	cmd.SetArgs(args)
	cmd.SetOut(c.out)
	cmd.SetErr(c.out)
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	cmd.SetFlagErrorFunc(func(_ *cobra.Command, err error) error { return usageErr("%s", err) })
	_, err := cmd.ExecuteContextC(c.ctx)
	return err
}

// lanes puts the MergeLanes into the call's configuration, where the verbs
// look a repository's lane up.
func (c *call) lanes() error {
	ls, err := c.s.store.Lanes()
	if err != nil {
		return err
	}
	c.app.cfg.Lanes = make([]config.Lane, len(ls))
	for i, l := range ls {
		c.app.cfg.Lanes[i] = config.Lane{Name: l.Name, Repositories: l.Spec.Repositories, Installation: l.Spec.Installation}
	}
	return nil
}

// may refuses an action on a record of owner's unless the caller is that
// person or holds owner's team's supervisor role.
func (c *call) may(what string, owner state.Party) error {
	if owner.Person != "" && owner.Person == c.who.Email || c.s.ids.Supervises(c.who, owner.Team) {
		return nil
	}
	return refused("%s is %s's: only that person or team %s's supervisor role may", what, cmp.Or(owner.Person, owner.Name), cmp.Or(owner.Team, "(none)"))
}

// partyName is how a party is named to another: <person>/<agent>.
func partyName(p state.Party) string {
	if p.Person == "" || p.Name == p.Person {
		return cmp.Or(p.Name, p.Person)
	}
	return p.Person + "/" + p.Name
}

func required(req mcp.CallToolRequest, key, what string) (string, error) {
	v := strings.TrimSpace(req.GetString(key, ""))
	if v == "" {
		return "", usageErr("%s is required: %s", key, what)
	}
	return v, nil
}

func prArg(req mcp.CallToolRequest) (string, int, error) {
	repo, err := required(req, paramRepo, "the repository, owner/repo")
	if err != nil {
		return "", 0, err
	}
	if !strings.Contains(repo, "/") {
		return "", 0, usageErr("repo %q: owner/repo", repo)
	}
	pr := req.GetInt("pr", 0)
	if pr <= 0 {
		return "", 0, usageErr("pr is required: the pull request's number")
	}
	return repo, pr, nil
}
