package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/state/kube"
	"github.com/giantswarm/beekeeper/pkg/apis/beekeeper/v1alpha1"
)

var errDefaultEventWrite = errors.New("default event unavailable")

type interruptedDefaults struct {
	client.Client
	failEvent      bool
	conflictSecond bool
	conflicted     bool
}

func (c *interruptedDefaults) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	if _, ok := obj.(*corev1.Event); ok && c.failEvent {
		return errDefaultEventWrite
	}
	return c.Client.Create(ctx, obj, opts...)
}

func (c *interruptedDefaults) Status() client.SubResourceWriter {
	return interruptedDefaultsStatus{SubResourceWriter: c.Client.Status(), c: c}
}

type interruptedDefaultsStatus struct {
	client.SubResourceWriter
	c *interruptedDefaults
}

func (w interruptedDefaultsStatus) Update(ctx context.Context, obj client.Object, opts ...client.SubResourceUpdateOption) error {
	if n, ok := obj.(*v1alpha1.Note); ok && n.Spec.ID == 2 && n.Status.Closure != nil && w.c.conflictSecond && !w.c.conflicted {
		w.c.conflicted = true
		return apierrors.NewConflict(schema.GroupResource{Group: v1alpha1.GroupVersion.Group, Resource: "notes"}, n.Name, errors.New("competing status write"))
	}
	return w.SubResourceWriter.Update(ctx, obj, opts...)
}

func TestDefaultDueRecoversDurableDeliveries(t *testing.T) {
	for _, conflict := range []bool{false, true} {
		t.Run(fmt.Sprintf("conflict=%t", conflict), func(t *testing.T) {
			scheme, err := kube.Scheme()
			if err != nil {
				t.Fatal(err)
			}
			now := time.Now().UTC().Truncate(time.Second)
			count := 1
			if conflict {
				count = 2
			}
			var objects []client.Object
			for id := 1; id <= count; id++ {
				objects = append(objects, &v1alpha1.Note{
					ObjectMeta: metav1.ObjectMeta{Namespace: kube.TeamNamespace(ourTeam), Name: fmt.Sprintf("note-%d", id)},
					Spec:       v1alpha1.NoteSpec{ID: id, Kind: state.NoteDecision, Text: askedQ, Default: ownLane, Due: &metav1.Time{Time: now.Add(-time.Minute)}},
					Status:     v1alpha1.NoteStatus{State: "open", By: v1alpha1.Party{Name: anaAgent, Team: ourTeam}, At: metav1.NewTime(now.Add(-time.Hour)), Posted: fmt.Sprintf("d%d", id)},
				})
			}
			c := &interruptedDefaults{Client: fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&v1alpha1.Note{}).WithObjects(objects...).Build(),
				failEvent: !conflict, conflictSecond: conflict}
			var failed atomic.Bool
			failed.Store(!conflict)
			var delivered atomic.Int64
			gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if failed.Load() {
					http.Error(w, "try again", http.StatusServiceUnavailable)
					return
				}
				delivered.Add(1)
				w.WriteHeader(http.StatusNoContent)
			}))
			t.Cleanup(gw.Close)
			// A synthetic fixture; no configured credential or real gateway.
			token := filepath.Join(t.TempDir(), "synthetic-token")
			if err := os.WriteFile(token, []byte("synthetic"), 0o600); err != nil {
				t.Fatal(err)
			}
			makeServer := func() *server {
				return &server{store: kube.New(c), cfg: &config.Config{}, log: slog.New(slog.NewTextHandler(io.Discard, nil)), now: func() time.Time { return now },
					gw: newGateway(config.Gateway{URL: gw.URL, TokenFile: token})}
			}
			err = makeServer().defaultDue(context.Background(), now)
			if conflict && err != nil {
				t.Fatal(err)
			}
			if !conflict && !errors.Is(err, errDefaultEventWrite) {
				t.Fatalf("partial commit must still report its Event error: %v", err)
			}
			if conflict && (!c.conflicted || delivered.Load() != int64(count)) {
				t.Fatalf("lost the default committed before retry: conflict=%t delivered=%d", c.conflicted, delivered.Load())
			}
			c.failEvent = false
			failed.Store(false)
			// A fresh process finds the unacknowledged closure after the failed
			// Event and gateway writes. A completed batch stays acknowledged.
			if err := makeServer().defaultDue(context.Background(), now); err != nil {
				t.Fatal(err)
			}
			if err := makeServer().defaultDue(context.Background(), now); err != nil {
				t.Fatal(err)
			}
			if delivered.Load() != int64(count) {
				t.Fatalf("missing or repeated delivery after restart: %d, want %d", delivered.Load(), count)
			}
		})
	}
}
