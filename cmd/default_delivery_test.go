package cmd

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/state/kube"
	"github.com/giantswarm/beekeeper/pkg/apis/beekeeper/v1alpha1"
)

type defaultFailureClient struct {
	client.Client
	eventFailure bool
	conflict     bool
	writes       int
}

func (c *defaultFailureClient) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	if _, ok := obj.(*corev1.Event); ok && c.eventFailure {
		return errors.New("synthetic Event failure")
	}
	return c.Client.Create(ctx, obj, opts...)
}

type defaultStatusWriter struct {
	client.SubResourceWriter
	owner *defaultFailureClient
}

func (c *defaultFailureClient) Status() client.SubResourceWriter {
	return defaultStatusWriter{c.Client.Status(), c}
}
func (w defaultStatusWriter) Update(ctx context.Context, obj client.Object, opts ...client.SubResourceUpdateOption) error {
	if _, ok := obj.(*v1alpha1.Note); ok {
		w.owner.writes++
		if w.owner.conflict && w.owner.writes == 2 {
			return apierrors.NewConflict(schema.GroupResource{Resource: "notes"}, obj.GetName(), errors.New("synthetic conflict"))
		}
	}
	return w.SubResourceWriter.Update(ctx, obj, opts...)
}

func TestDefaultDueRecoversDurableDeliveries(t *testing.T) {
	for _, mode := range []string{"event", "conflict", "gateway"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			now := time.Now().UTC().Truncate(time.Second)
			scheme, err := kube.Scheme()
			if err != nil {
				t.Fatal(err)
			}
			c := &defaultFailureClient{Client: fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&v1alpha1.Note{}).Build(), eventFailure: mode == "event", conflict: mode == "conflict"}
			due := metav1.NewTime(now.Add(-time.Hour))
			for id := 1; id <= 2; id++ {
				n := &v1alpha1.Note{ObjectMeta: metav1.ObjectMeta{Name: kube.NoteObject("test", id).GetName(), Namespace: kube.NoteObject("test", id).GetNamespace()},
					Spec:   v1alpha1.NoteSpec{ID: id, Kind: "decision", Due: &due, Default: "wait"},
					Status: v1alpha1.NoteStatus{State: "open", By: v1alpha1.Party{Team: "test"}, At: metav1.NewTime(now), Posted: "handle-" + strconv.Itoa(id)}}
				if err := c.Client.Create(ctx, n); err != nil {
					t.Fatal(err)
				}
			}
			calls := 0
			delivered := map[string]int{}
			failGateway := mode == "gateway"
			httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if failGateway {
					http.Error(w, "synthetic failure", http.StatusServiceUnavailable)
					return
				}
				delivered[r.URL.Path]++
				w.WriteHeader(http.StatusOK)
			}))
			defer httpServer.Close()
			token := filepath.Join(t.TempDir(), "token")
			if err := os.WriteFile(token, []byte("synthetic-token"), 0600); err != nil {
				t.Fatal(err)
			}
			fresh := func() *server {
				s := newServer(&config.Config{}, kube.New(c), nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
				s.gw = &gateway{http: httpServer.Client()}
				s.gw.cfg.URL = httpServer.URL
				s.gw.cfg.TokenFile = token
				return s
			}
			err = fresh().defaultDue(ctx, now)
			if mode != "conflict" && err == nil {
				t.Fatal("expected injected failure")
			}
			c.eventFailure = false
			failGateway = false
			if err := fresh().defaultDue(ctx, now.Add(48*time.Hour)); err != nil {
				t.Fatal(err)
			}
			pending, err := kube.New(c).PendingDefaults()
			if err != nil || len(pending) != 0 {
				t.Fatalf("pending: %+v, %v", pending, err)
			}
			before := calls
			if err := fresh().defaultDue(ctx, now.Add(48*time.Hour)); err != nil {
				t.Fatal(err)
			}
			if calls != before {
				t.Fatalf("acknowledged deliveries replayed: %d -> %d", before, calls)
			}
			if len(delivered) != 2 {
				t.Fatalf("lost note delivery: %v", delivered)
			}
			if calls < 2 {
				t.Fatalf("lost deliveries: %d", calls)
			}
		})
	}
}
