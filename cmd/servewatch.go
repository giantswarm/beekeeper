package cmd

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/rest"
	toolscache "k8s.io/client-go/tools/cache"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/giantswarm/beekeeper/internal/identity"
	"github.com/giantswarm/beekeeper/internal/mailbox"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/state/kube"
	"github.com/giantswarm/beekeeper/pkg/apis/beekeeper/v1alpha1"
)

// expireEvery is how often the unacked messages past their deadline are
// taken out.
const expireEvery = 30 * time.Second

// watch keeps the subscribers current until ctx ends: it watches the
// beekeeper resources and Events of the cluster rc names and the mailboxes'
// channel, and expires the messages past their deadline. It returns once
// the watches are in place.
func (s *server) watch(ctx context.Context, rc *rest.Config) error {
	scheme, err := kube.Scheme()
	if err != nil {
		return err
	}
	c, err := cache.New(rc, cache.Options{Scheme: scheme, ByObject: map[client.Object]cache.ByObject{
		&corev1.Event{}: {Label: labels.SelectorFromSet(kube.EventLabels())},
	}})
	if err != nil {
		return err
	}
	// on calls changed with the object before (nil when added) and after
	// each change; a deleted object comes as both.
	on := func(obj client.Object, changed func(old, obj any)) error {
		inf, err := c.GetInformer(ctx, obj)
		if err != nil {
			return err
		}
		_, err = inf.AddEventHandler(toolscache.ResourceEventHandlerFuncs{
			AddFunc:    func(obj any) { changed(nil, obj) },
			UpdateFunc: changed,
			DeleteFunc: func(obj any) {
				if t, ok := obj.(toolscache.DeletedFinalStateUnknown); ok {
					obj = t.Obj
				}
				changed(obj, obj)
			},
		})
		return err
	}
	err = errors.Join(
		on(&v1alpha1.Environment{}, func(old, obj any) {
			if e, ok := obj.(*v1alpha1.Environment); ok {
				s.hub.updated(resourceEnvironments + e.Name)
				s.holderChanged(old, e)
			}
		}),
		on(&v1alpha1.MergeLane{}, func(_, obj any) {
			if l, ok := obj.(*v1alpha1.MergeLane); ok {
				s.hub.updated(resourceLanes + l.Name)
			}
		}),
		on(&v1alpha1.Note{}, func(_, obj any) {
			if n, ok := obj.(*v1alpha1.Note); ok {
				s.hub.updatedWhere(func(who identity.Caller, uri string) bool {
					return strings.HasPrefix(uri, resourceNotes) && concerns(who, n.Spec.For, n.Status.By.Person)
				})
			}
		}),
		on(&v1alpha1.RosterEntry{}, func(_, obj any) {
			if r, ok := obj.(*v1alpha1.RosterEntry); ok {
				s.rosterChanged(kube.PartyOf(r.Spec.Party))
			}
		}),
		on(&corev1.Event{}, func(_, obj any) {
			if e, ok := obj.(*corev1.Event); ok && !strings.HasPrefix(e.Reason, "serve.") {
				s.eventRecorded(e)
			}
		}),
	)
	if err != nil {
		return err
	}
	go func() {
		if err := c.Start(ctx); err != nil {
			s.log.Error("watch", "detail", err.Error())
		}
	}()
	if !c.WaitForCacheSync(ctx) {
		return errors.New("watch: the caches did not sync")
	}
	listening := make(chan struct{})
	go s.mail.Listen(ctx, s.mailboxChanged, func() {
		select {
		case listening <- struct{}{}:
		default:
		}
	}, func(err error) { s.log.Warn("mailbox listen", "detail", err.Error()) })
	select {
	case <-listening:
	case <-ctx.Done():
		return ctx.Err()
	}
	go s.expireLoop(ctx)
	go s.defaultLoop(ctx)
	return nil
}

// rosterChanged notifies the roster's subscribers who read the agent p.
func (s *server) rosterChanged(p state.Party) {
	envs, err := s.store.Environments()
	if err != nil {
		s.log.Error("notify", "uri", resourceRoster, "detail", err.Error())
		return
	}
	s.hub.updatedWhere(func(who identity.Caller, uri string) bool {
		return uri == resourceRoster && scopeOf(who, envs).reads(p)
	})
}

// holderChanged notifies the roster's subscribers of the old and the new
// holder's team: a lease taken or freed shows a local agent to its team or
// hides it again.
func (s *server) holderChanged(old any, e *v1alpha1.Environment) {
	teams := map[string]bool{}
	for _, x := range []any{old, e} {
		if env, ok := x.(*v1alpha1.Environment); ok && env.Status.Holder != nil && env.Status.Holder.Party.Team != "" {
			teams[env.Status.Holder.Party.Team] = true
		}
	}
	if len(teams) == 0 {
		return
	}
	s.hub.updatedWhere(func(who identity.Caller, uri string) bool { return uri == resourceRoster && teams[who.Team] })
}

// eventRecorded notifies the feed's subscribers who read the event.
func (s *server) eventRecorded(e *corev1.Event) {
	if err := s.notifyEvent(e); err != nil {
		s.log.Error("notify", "uri", resourceFeed, "detail", err.Error())
	}
}

func (s *server) notifyEvent(e *corev1.Event) error {
	ev, err := kube.FeedEvent(e)
	if err != nil {
		return err
	}
	st, err := s.store.Read()
	if err != nil {
		return err
	}
	envs, err := s.store.Environments()
	if err != nil {
		return err
	}
	s.hub.updatedWhere(func(who identity.Caller, uri string) bool {
		return uri == resourceFeed && scopeOf(who, envs).readsEvent(ev, st.Agents)
	})
	return nil
}

// mailboxChanged notifies the streams subscribed to mb's mailbox.
func (s *server) mailboxChanged(mb string) {
	s.hub.updatedWhere(func(_ identity.Caller, uri string) bool {
		p, ok := strings.CutPrefix(uri, resourceMailbox)
		return ok && mailbox.Key(p) == mb
	})
}

func (s *server) expireLoop(ctx context.Context) {
	t := time.NewTicker(expireEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := s.expire(ctx); err != nil {
				s.log.Error("expire", "detail", err.Error())
			}
		}
	}
}

// expire takes the messages past their deadline out and records each on
// the feed, as its sender's: message.expired.
func (s *server) expire(ctx context.Context) error {
	exp, err := s.mail.Expire(ctx, s.now())
	if err != nil {
		return err
	}
	var errs []error
	for _, e := range exp {
		by := state.Party{Name: e.Sender, Person: e.Sender, Team: e.SenderTeam}
		ev := state.Event{At: s.now().UTC(), By: by, Verb: "message.expired",
			Detail: fmt.Sprintf("%s to %s expired unacked at %s", e.MessageID, e.Mailbox, e.Deadline.Format(time.RFC3339))}
		if err := s.store.Audit(nil, e.SenderTeam, ev); err != nil {
			errs = append(errs, err)
		}
		s.log.Info("expire", "sender", e.Sender, "mailbox", e.Mailbox, "messageId", e.MessageID)
	}
	return errors.Join(errs...)
}
