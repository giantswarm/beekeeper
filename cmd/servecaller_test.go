package cmd

import (
	"bytes"
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/identity"
	"github.com/giantswarm/beekeeper/internal/state/kube"
)

const (
	leaseListTool  = "lease_list"
	callerTestHost = "test-machine"
)

func TestAuditUndeclaredCaller(t *testing.T) {
	scheme, err := kube.Scheme()
	if err != nil {
		t.Fatal(err)
	}
	client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: kube.TeamNamespace("test-team")}}).Build()
	var log bytes.Buffer
	s := &server{cfg: &config.Config{}, store: kube.New(client), now: time.Now, log: slog.New(slog.NewJSONHandler(&log, nil))}
	ctx := context.WithValue(t.Context(), callerKey{}, identity.Caller{Email: "caller@example.com", Team: "test-team"})
	var tool serveTool
	for _, candidate := range s.tools() {
		if candidate.tool.Name == leaseListTool {
			tool = candidate
		}
	}
	req := mcp.CallToolRequest{}
	req.Params.Name = tool.tool.Name
	req.Params.Arguments = map[string]any{paramAgent: "test-agent", paramHost: callerTestHost}
	result, err := s.handle(tool)(ctx, req)
	if err != nil || result.IsError {
		t.Fatalf("call: %+v, %v", result, err)
	}
	events, err := s.store.Events(0, nil)
	if err != nil || len(events) != 1 {
		t.Fatalf("audit events: %+v, %v", events, err)
	}
	if got := events[0]; got.By.Name != "test-agent" || got.By.Host != callerTestHost || got.Verb != "serve.lease_list" {
		t.Fatalf("audit lost caller identity: %+v", got)
	}
	if !bytes.Contains(log.Bytes(), []byte(`"agent":"test-agent"`)) {
		t.Fatalf("log lost caller identity: %s", &log)
	}
}
