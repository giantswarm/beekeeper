package cmd

import (
	"bytes"
	"cmp"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/lease"
	"github.com/giantswarm/beekeeper/internal/sandbox"
)

func (a *app) leaseKubeconfigCmd() *cobra.Command {
	var refresh bool
	c := &cobra.Command{
		Use:   "kubeconfig <lab>",
		Short: "Write the kubeconfig of a kind lab whose lease you hold into its lease, and print its path",
		Long: `kubeconfig writes kind's kubeconfig of the lab's cluster into the lease
(<leaseDir>/<lab>/kubeconfig, mode 0600) and prints its path; release frees
the lease and the kubeconfig with it. Only the session that holds the lab's
lease gets one. A lab claim writes it already when the cluster runs; this
writes it once a lab started after its claim does. Before every command of
a session that holds a lab lease, beekeeper's hook checks the copy against
the lab's API server and rewrites it from kind when it no longer reaches
it: a lab recreated under the lease (agentlab down and up) has a new CA.
The copy stays in the lease rather than pointing at kind's own kubeconfig,
so that a released lease reaches the cluster no more.

In the agent sandbox, which closes the container runtime's socket kind
needs, the host's broker writes it for the session, and the session points
it at its sandbox's SOCKS proxy: the API server is on loopback, which the
sandbox reaches only through the proxy and only on the ports
sandbox.domains lists (127.0.0.1:<port>). beekeeper's hook refreshes the
proxy's URL before every command of a sandboxed session that holds a lab
lease (--refresh).`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			sandboxed := inSandbox()
			if refresh {
				if !sandboxed {
					return nil
				}
				var errs []error
				for _, res := range args {
					if err := a.checkResource(res); err != nil {
						errs = append(errs, err)
						continue
					}
					if err := proxyLabKubeconfig(labKubeconfig(a.cfg.LeaseDir, res)); err != nil && !errors.Is(err, fs.ErrNotExist) {
						errs = append(errs, err)
					}
				}
				return errors.Join(errs...)
			}
			if len(args) != 1 {
				return usageErr("kubeconfig takes one lab")
			}
			res := args[0]
			if err := a.checkResource(res); err != nil {
				return err
			}
			line, err := a.labKubeconfigLine(cmd.Context(), res)
			if err != nil {
				return err
			}
			_, err = fmt.Fprintln(a.out, line)
			return err
		},
	}
	c.Flags().BoolVar(&refresh, "refresh", false, "in the sandbox, point the labs' existing kubeconfigs at this process's sandbox proxy; quiet, a missing one skipped")
	_ = c.Flags().MarkHidden("refresh")
	return c
}

// brokeredKubeconfig has the host's broker write the kubeconfig of the lab
// res for a sandboxed session, points it at the session's sandbox proxy,
// and returns the broker's export line.
func (a *app) brokeredKubeconfig(res string) (string, error) {
	dir := sandbox.SpoolDir(a.cfg.StateDir)
	if !(sandbox.Capper{Dir: dir}).Available() {
		return "", refused("no sandbox broker answers in %s: beekeeper-sandbox.service on the host runs this for the sandbox (beekeeper install)", dir)
	}
	r, err := sandbox.Call(dir, sandbox.Request{Op: sandbox.OpKubeconfig, Resource: res}, brokeredCallTimeout)
	if err != nil {
		return "", refused("%v", err)
	}
	if r.Code != 0 {
		return "", &exitError{code: r.Code, msg: strings.TrimPrefix(strings.TrimSpace(r.Err), "beekeeper: ")}
	}
	if err := proxyLabKubeconfig(labKubeconfig(a.cfg.LeaseDir, res)); err != nil {
		return "", err
	}
	return strings.TrimSpace(r.Out), nil
}

// proxyLabKubeconfig points the lab kubeconfig at path at the SOCKS proxy
// of the sandbox the process runs in, rewriting it only when that changes.
// path is a lease's kubeconfig, its resource checked by the caller.
func proxyLabKubeconfig(path string) error {
	raw, err := os.ReadFile(path) //nolint:gosec // a lease's kubeconfig (labKubeconfig of a checked resource)
	if err != nil {
		return err
	}
	proxy, err := sandbox.SOCKSProxy(os.Getenv)
	if err != nil {
		return err
	}
	out, changed, err := sandbox.ProxyKubeconfig(raw, proxy)
	if err != nil || !changed {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, out, 0o600); err != nil { //nolint:gosec // beside the checked lease's kubeconfig
		return err
	}
	return os.Rename(tmp, path)
}

// labKubeconfig is where a lab lease's kubeconfig lies: in the lease, so
// that it goes with it.
func labKubeconfig(leaseDir, res string) string { return filepath.Join(leaseDir, res, "kubeconfig") }

// writeLabKubeconfig writes the kubeconfig of the lab res, whose lease the
// caller holds, and returns its path.
func (a *app) writeLabKubeconfig(ctx context.Context, res string) (string, error) {
	cl := a.cfg.LabCluster(res)
	if cl == "" {
		return "", usageErr("%s is not a lab: a kubeconfig is written for a kind lab's lease only", res)
	}
	if err := a.holdsLease(res, res); err != nil {
		return "", err
	}
	kc, err := secretRun(ctx, "", nil, nil, "kind", "get", "kubeconfig", "--name", cl)
	if err != nil {
		return "", fmt.Errorf("kind cluster %s: %w", cl, err)
	}
	path := labKubeconfig(a.cfg.LeaseDir, res)
	tmp := path + ".tmp"
	// a lease released meanwhile took its directory: the write fails
	if err := os.WriteFile(tmp, kc, 0o600); err != nil {
		return "", err
	}
	return path, os.Rename(tmp, path)
}

// claimKubeconfig is the line a lab claim adds: the kubeconfig's path, or
// why there is none yet.
func (a *app) claimKubeconfig(ctx context.Context, res string) string {
	line, err := a.labKubeconfigLine(ctx, res)
	if err != nil {
		return fmt.Sprintf("\nno kubeconfig yet (%v): beekeeper lease kubeconfig %s once the lab runs", err, res)
	}
	return "\n" + line
}

// labKubeconfigLine writes the kubeconfig of the lab res, whose lease the
// caller holds, through the broker in the sandbox, and returns its export
// line.
func (a *app) labKubeconfigLine(ctx context.Context, res string) (string, error) {
	if inSandbox() {
		return a.brokeredKubeconfig(res)
	}
	path, err := a.writeLabKubeconfig(ctx, res)
	return "export KUBECONFIG=" + path, err
}

// holdsLease refuses, for what, unless the caller holds the lease of res.
func (a *app) holdsLease(res, what string) error {
	me, err := a.caller()
	if err != nil {
		return err
	}
	h, err := lease.Dir(a.cfg.LeaseDir).Get(res)
	if err != nil {
		return err
	}
	if h == nil || !h.Party().Is(me) {
		holder := "nobody"
		if h != nil {
			holder = fmt.Sprintf("%q", cmp.Or(h.Name, h.Holder))
		}
		kind := "lease"
		if a.cfg.LabCluster(res) != "" {
			kind = "lab lease"
		}
		return refused("%s: the %s %s is held by %s, not by you: claim it first (beekeeper lease claim %s)", what, kind, res, holder, res)
	}
	return nil
}

// labProbeTimeout bounds the dial of a lab's API server that checks the
// kubeconfig in its lease; labRefreshTimeout bounds kind's answer when it
// no longer reaches it. Both run in the hook, before a command.
const (
	labProbeTimeout   = time.Second
	labRefreshTimeout = 5 * time.Second
)

// freshLabKubeconfigs brings the kubeconfigs in the lab leases labs, which
// the caller holds, up to date with their clusters; quiet, what fails is
// left as it is.
func (a *app) freshLabKubeconfigs(labs []string) {
	for _, res := range labs {
		_, _ = a.refreshLabKubeconfig(context.Background(), res)
	}
}

// refreshLabKubeconfig rewrites the kubeconfig in the lease of the lab res
// from kind when it no longer reaches the lab's API server, the lab
// recreated with a new CA or port, and reports whether it did. The sandbox
// proxy the copy names is kept. Only an existing copy is replaced: a
// released lease, which took its kubeconfig with it, gets none back, and a
// cluster kind does not answer for leaves the copy as it is.
func (a *app) refreshLabKubeconfig(ctx context.Context, res string) (bool, error) {
	cl := a.cfg.LabCluster(res)
	if cl == "" {
		return false, nil
	}
	path := labKubeconfig(a.cfg.LeaseDir, res)
	raw, err := os.ReadFile(path) //nolint:gosec // a lease's kubeconfig (labKubeconfig of a configured lab)
	if err != nil || !staleKubeconfig(ctx, raw) {
		return false, err
	}
	ctx, cancel := context.WithTimeout(ctx, labRefreshTimeout)
	defer cancel()
	kc, err := secretRun(ctx, "", nil, nil, "kind", "get", "kubeconfig", "--name", cl)
	if err != nil {
		return false, fmt.Errorf("kind cluster %s: %w", cl, err)
	}
	if proxy := kubeconfigProxy(raw); proxy != "" {
		if kc, _, err = sandbox.ProxyKubeconfig(kc, proxy); err != nil {
			return false, err
		}
	}
	if bytes.Equal(kc, raw) {
		return false, nil
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, kc, 0o600); err != nil { //nolint:gosec // beside the lease's kubeconfig
		return false, err
	}
	return true, os.Rename(tmp, path)
}

// staleKubeconfig reports whether the kubeconfig raw no longer reaches a
// loopback API server it names: the server does not answer, or its
// certificate does not chain to the CA the kubeconfig carries. A server
// off loopback, or one without CA data, is not judged.
func staleKubeconfig(ctx context.Context, raw []byte) bool {
	cfg, err := clientcmd.Load(raw)
	if err != nil {
		return false
	}
	for _, c := range cfg.Clusters {
		u, err := url.Parse(c.Server)
		if err != nil || !config.Loopback(u.Hostname()) || len(c.CertificateAuthorityData) == 0 {
			continue
		}
		roots := x509.NewCertPool()
		if !roots.AppendCertsFromPEM(c.CertificateAuthorityData) || !trustedServer(ctx, u.Host, roots) {
			return true
		}
	}
	return false
}

// trustedServer reports whether the TLS server at addr answers with a
// certificate that chains to roots; the name is not checked, the CA is
// what a recreated lab changes.
func trustedServer(ctx context.Context, addr string, roots *x509.CertPool) bool {
	d := tls.Dialer{NetDialer: &net.Dialer{Timeout: labProbeTimeout}, Config: &tls.Config{
		InsecureSkipVerify: true, //nolint:gosec // the chain is verified against roots below
		MinVersion:         tls.VersionTLS12,
	}}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return false
	}
	defer func() { _ = conn.Close() }()
	certs := conn.(*tls.Conn).ConnectionState().PeerCertificates
	if len(certs) == 0 {
		return false
	}
	inter := x509.NewCertPool()
	for _, c := range certs[1:] {
		inter.AddCert(c)
	}
	_, err = certs[0].Verify(x509.VerifyOptions{Roots: roots, Intermediates: inter})
	return err == nil
}

// kubeconfigProxy is the proxy the kubeconfig raw names for a cluster, ""
// without one.
func kubeconfigProxy(raw []byte) string {
	cfg, err := clientcmd.Load(raw)
	if err != nil {
		return ""
	}
	for _, c := range cfg.Clusters {
		if c.ProxyURL != "" {
			return c.ProxyURL
		}
	}
	return ""
}
