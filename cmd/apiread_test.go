package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	authentication "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
	clientapi "k8s.io/client-go/tools/clientcmd/api"

	"github.com/giantswarm/beekeeper/internal/config"
)

func TestAPIReadRedactsConsumerOutput(t *testing.T) {
	dir := t.TempDir()
	// The consumer deliberately echoes the credential on both streams and
	// asserts that the credential was not put in the process arguments.
	script := `#!/bin/sh
case "$*" in *private-value*) exit 9;; esac
printf '%s' "$BEEKEEPER_API_READ_TOKEN"
printf '%s' "$BEEKEEPER_API_READ_TOKEN" >&2
`
	if err := os.WriteFile(filepath.Join(dir, "grpcurl"), []byte(script), 0700); err != nil { //nolint:gosec // an executable fixture
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	var out bytes.Buffer
	err := runReadRPC(context.Background(), "<endpoint>", "Reader/List", "{}", "private-value", &out)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "private-value") || out.String() != "[REDACTED][REDACTED]" {
		t.Fatalf("unsafe output: %q", out.String())
	}
}

func TestAPIReadDiscardsFailedConsumerOutput(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "grpcurl"), []byte("#!/bin/sh\nprintf '%s' \"$BEEKEEPER_API_READ_TOKEN\"\nexit 1\n"), 0700); err != nil { //nolint:gosec // an executable fixture
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	var out bytes.Buffer
	err := runReadRPC(context.Background(), "<endpoint>", "Reader/List", "{}", "private-value", &out)
	if err == nil || out.Len() != 0 || strings.Contains(err.Error(), "private-value") {
		t.Fatal("unsafe failure output")
	}
}

func TestAPIReadCommand(t *testing.T) {
	minted := false
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/apis/rbac.authorization.k8s.io/v1/clusterrolebindings":
			ioString := `{"apiVersion":"rbac.authorization.k8s.io/v1","kind":"ClusterRoleBindingList","items":[]}`
			_, _ = w.Write([]byte(ioString))
		case "/apis/rbac.authorization.k8s.io/v1/rolebindings":
			_, _ = w.Write([]byte(`{"apiVersion":"rbac.authorization.k8s.io/v1","kind":"RoleBindingList","items":[]}`))
		case "/api/v1/namespaces/example-namespace/serviceaccounts/reader/token":
			minted = true
			var req authentication.TokenRequest
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Error(err)
			}
			if err := runtime.DecodeInto(scheme.Codecs.UniversalDeserializer(), body, &req); err != nil {
				t.Error(err)
			}

			if req.Spec.ExpirationSeconds == nil || *req.Spec.ExpirationSeconds != 600 || len(req.Spec.Audiences) != 1 || req.Spec.Audiences[0] != "example-audience" {
				t.Error("incorrect token request")
			}
			_ = json.NewEncoder(w).Encode(authentication.TokenRequest{TypeMeta: metav1.TypeMeta{APIVersion: "authentication.k8s.io/v1", Kind: "TokenRequest"}, Status: authentication.TokenRequestStatus{Token: "private-value", ExpirationTimestamp: metav1.NewTime(time.Now().Add(599 * time.Second))}})
		default:
			t.Errorf("unexpected request: %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	dir := t.TempDir()
	path := filepath.Join(dir, "kubeconfig")
	kc := clientapi.Config{Clusters: map[string]*clientapi.Cluster{"example-cluster": {Server: server.URL, CertificateAuthorityData: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})}}, Contexts: map[string]*clientapi.Context{"example-context": {Cluster: "example-cluster"}}}
	if err := clientcmd.WriteToFile(kc, path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "grpcurl"), []byte("#!/bin/sh\nprintf '%s' \"$BEEKEEPER_API_READ_TOKEN\"\n"), 0700); err != nil { //nolint:gosec // an executable fixture
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	var out bytes.Buffer
	a := &app{cfg: &config.Config{Kube: config.Kube{Kubeconfig: []string{path}, APIReads: map[string]config.APIRead{"example-api": {Context: "example-context", Namespace: "example-namespace", ServiceAccount: "reader", Audience: "example-audience", Endpoint: "<endpoint>", Methods: []string{"Reader/List"}}}}}, out: &out}
	for _, method := range []string{"Reader/Delete", "Reader/List"} {
		command := a.apiReadCmd()
		command.SetArgs([]string{"example-api", method})
		err := command.Execute()
		if method == "Reader/Delete" {
			if err == nil || minted {
				t.Fatal("unconfigured method reached issuer")
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if !minted || out.String() != "[REDACTED]" {
		t.Fatal("reader command did not keep token private")
	}
}

func TestAPIReadOutputBound(t *testing.T) {
	var output limitedRPCOutput
	_, err := io.Copy(&output, strings.NewReader(strings.Repeat("x", (16<<20)+1)))
	if err == nil || len(output.String()) > 16<<20 {
		t.Fatal("RPC output limit bypassed")
	}
}
