package cmd

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"

	"github.com/giantswarm/beekeeper/internal/lease"
	"github.com/giantswarm/beekeeper/internal/sandbox"
	"github.com/giantswarm/beekeeper/internal/secret/secrettest"
)

func runLease(a *app, args ...string) (string, error) {
	a.out = &bytes.Buffer{}
	c := a.leaseCmd()
	c.PersistentPreRunE = nil
	usageArgs(c)
	c.SetArgs(args)
	c.SetOut(a.out)
	c.SetErr(a.out)
	c.SilenceUsage, c.SilenceErrors = true, true
	err := c.ExecuteContext(context.Background())
	return a.out.(*bytes.Buffer).String(), err
}

func TestLeaseKubeconfigOnlyForTheHolder(t *testing.T) {
	a, _, _ := secretApp(t)
	a.cfg.LeaseDir = t.TempDir()
	a.cfg.Resources = []string{labOne, labTwo, browser}
	a.cfg.Labs = map[string]string{labOne: labCluster, labTwo: labTwo}
	dir := lease.Dir(a.cfg.LeaseDir)
	if _, err := dir.Claim(labOne, lease.Holder{Env: labOne, Name: a.as}); err != nil {
		t.Fatal(err)
	}
	if _, err := dir.Claim(labTwo, lease.Holder{Env: labTwo, Name: "another session"}); err != nil {
		t.Fatal(err)
	}
	path := labKubeconfig(a.cfg.LeaseDir, labOne)
	out, err := runLease(a, "kubeconfig", labOne)
	if err != nil || out != "export KUBECONFIG="+path+"\n" {
		t.Fatalf("the holder's kubeconfig: %q, %v", out, err)
	}
	raw, err := os.ReadFile(path) //nolint:gosec // the test's lease
	if st, _ := os.Stat(path); err != nil || string(raw) != secrettest.Kubeconfig(labCluster) || st.Mode().Perm() != 0o600 {
		t.Errorf("kubeconfig %q, %v", raw, err)
	}
	if _, err := runLease(a, "kubeconfig", labTwo); Code(err) != ExitRefused {
		t.Errorf("another holder's lab: exit %d, want refused", Code(err))
	}
	if _, err := os.Stat(labKubeconfig(a.cfg.LeaseDir, labTwo)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("another holder's lab got a kubeconfig: %v", err)
	}
	if _, err := runLease(a, "kubeconfig", browser); Code(err) != ExitUsage {
		t.Errorf("a resource that is no lab: exit %d, want usage", Code(err))
	}
	if err := dir.Release(labOne); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the kubeconfig outlived its lease: %v", err)
	}
	if _, err := runLease(a, "kubeconfig", labOne); Code(err) != ExitRefused {
		t.Errorf("a released lab: exit %d, want refused", Code(err))
	}
}

func TestLeaseKubeconfigInTheSandboxAsksTheBroker(t *testing.T) {
	a, _, _ := secretApp(t)
	a.cfg.LeaseDir = t.TempDir()
	a.cfg.Resources = []string{labOne}
	a.cfg.Labs = map[string]string{labOne: labCluster}
	t.Setenv(sandbox.Env, "1")
	// no broker serves the scratch state directory
	if _, err := runLease(a, "kubeconfig", labOne); Code(err) != ExitRefused {
		t.Errorf("no broker: exit %d, want refused", Code(err))
	}
	if _, err := os.Stat(filepath.Join(a.cfg.LeaseDir, labOne)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the sandbox wrote a lease: %v", err)
	}
}

func TestBrokeredKubeconfigArgv(t *testing.T) {
	if argv, err := brokeredKubeconfigArgv(sandbox.Request{Op: sandbox.OpKubeconfig, Resource: labOne}, true); err != nil || len(argv) != 3 || argv[2] != labOne {
		t.Errorf("%q, %v", argv, err)
	}
	for _, bad := range []string{"", "../state", "-h", "a b", "--as=x"} {
		if _, err := brokeredKubeconfigArgv(sandbox.Request{Op: sandbox.OpKubeconfig, Resource: bad}, true); err == nil {
			t.Errorf("%q: want a refusal", bad)
		}
	}
}

// labTLS is a lab's API server for a test: a TLS server on loopback and a
// kubeconfig of kind's shape naming it with its CA.
func labTLS(t *testing.T) (*httptest.Server, []byte) {
	t.Helper()
	srv := newLabServer(t)
	t.Cleanup(srv.Close)
	return srv, testKubeconfig(t, srv.URL, srv.Certificate(), "")
}

// newLabServer is a TLS server on loopback with a self-signed CA of its
// own, as every kind cluster has: httptest's servers share one.
func newLabServer(t *testing.T) *httptest.Server {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "kubernetes"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, IPAddresses: []net.IP{net.IPv4(127, 0, 0, 1)},
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(http.NotFoundHandler())
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}, MinVersion: tls.VersionTLS12}
	srv.StartTLS()
	return srv
}

func testKubeconfig(t *testing.T, server string, ca *x509.Certificate, proxy string) []byte {
	t.Helper()
	cfg := clientcmdapi.NewConfig()
	cfg.Clusters["kind-"+labCluster] = &clientcmdapi.Cluster{Server: server, ProxyURL: proxy,
		CertificateAuthorityData: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca.Raw})}
	cfg.AuthInfos["kind-"+labCluster] = &clientcmdapi.AuthInfo{}
	cfg.Contexts["kind-"+labCluster] = &clientcmdapi.Context{Cluster: "kind-" + labCluster, AuthInfo: "kind-" + labCluster}
	cfg.CurrentContext = "kind-" + labCluster
	raw, err := clientcmd.Write(*cfg)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// TestLeaseKubeconfigFollowsARecreatedLab recreates nothing: the lease's
// copy carries the CA of a lab's former cluster, the lab's API server now
// answers with another, and kind answers the new cluster's kubeconfig.
func TestLeaseKubeconfigFollowsARecreatedLab(t *testing.T) {
	a, tools, _ := secretApp(t)
	a.cfg.LeaseDir = t.TempDir()
	a.cfg.Resources = []string{labOne}
	a.cfg.Labs = map[string]string{labOne: labCluster}
	former, _ := labTLS(t)
	lab, live := labTLS(t)
	kinds := 0
	secretRun = func(ctx context.Context, dir string, env []string, stdin io.Reader, name string, args ...string) ([]byte, error) {
		if name == "kind" {
			kinds++
			return live, nil
		}
		return tools.Run(ctx, dir, env, stdin, name, args...)
	}
	dir := lease.Dir(a.cfg.LeaseDir)
	if _, err := dir.Claim(labOne, lease.Holder{Env: labOne, Name: a.as}); err != nil {
		t.Fatal(err)
	}
	path := labKubeconfig(a.cfg.LeaseDir, labOne)
	write := func(raw []byte) {
		t.Helper()
		if err := os.WriteFile(path, raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	read := func() []byte {
		t.Helper()
		raw, err := os.ReadFile(path) //nolint:gosec // the test's lease
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	ctx := context.Background()

	write(live)
	if done, err := a.refreshLabKubeconfig(ctx, labOne); done || err != nil || kinds != 0 {
		t.Errorf("a copy that reaches the lab: rewritten %v, %v, kind asked %d times", done, err, kinds)
	}

	write(testKubeconfig(t, lab.URL, former.Certificate(), ""))
	if done, err := a.refreshLabKubeconfig(ctx, labOne); !done || err != nil || !bytes.Equal(read(), live) {
		t.Errorf("a rotated CA: rewritten %v, %v, copy %s", done, err, read())
	}
	if st, _ := os.Stat(path); st.Mode().Perm() != 0o600 {
		t.Errorf("mode %v", st.Mode().Perm())
	}

	const proxy = "socks5://127.0.0.1:1080"
	write(testKubeconfig(t, lab.URL, former.Certificate(), proxy))
	want, _, err := sandbox.ProxyKubeconfig(live, proxy)
	if err != nil {
		t.Fatal(err)
	}
	if done, err := a.refreshLabKubeconfig(ctx, labOne); !done || err != nil || !bytes.Equal(read(), want) {
		t.Errorf("the sandbox proxy is kept: rewritten %v, %v, copy %s", done, err, read())
	}

	gone := newLabServer(t)
	gone.Close()
	write(testKubeconfig(t, gone.URL, lab.Certificate(), ""))
	if done, err := a.refreshLabKubeconfig(ctx, labOne); !done || err != nil || !bytes.Equal(read(), live) {
		t.Errorf("a new port: rewritten %v, %v", done, err)
	}

	write(testKubeconfig(t, lab.URL, former.Certificate(), ""))
	a.freshLabKubeconfigs([]string{labOne})
	if !bytes.Equal(read(), live) {
		t.Errorf("the hook's refresh left the copy %s", read())
	}

	if err := dir.Release(labOne); err != nil {
		t.Fatal(err)
	}
	if done, err := a.refreshLabKubeconfig(ctx, labOne); done || !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a released lease: rewritten %v, %v", done, err)
	}
	if _, err := os.Stat(filepath.Join(a.cfg.LeaseDir, labOne)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a released lease got its kubeconfig back: %v", err)
	}
}

func TestStaleKubeconfigJudgesLoopbackOnly(t *testing.T) {
	lab, live := labTLS(t)
	ctx := context.Background()
	if staleKubeconfig(ctx, live) {
		t.Error("the live kubeconfig is stale")
	}
	if !staleKubeconfig(ctx, testKubeconfig(t, lab.URL, labCA(t), "")) {
		t.Error("another CA is not stale")
	}
	if staleKubeconfig(ctx, testKubeconfig(t, "https://api.example.com:6443", labCA(t), "")) {
		t.Error("a server off loopback was judged")
	}
	if staleKubeconfig(ctx, []byte("not a kubeconfig")) {
		t.Error("garbage is stale")
	}
}

func labCA(t *testing.T) *x509.Certificate {
	t.Helper()
	srv := newLabServer(t)
	defer srv.Close()
	return srv.Certificate()
}
