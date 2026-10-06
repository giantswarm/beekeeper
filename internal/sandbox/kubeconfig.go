package sandbox

import (
	"fmt"
	"net/url"

	"k8s.io/client-go/tools/clientcmd"

	"github.com/giantswarm/beekeeper/internal/config"
)

// SOCKSProxy is the SOCKS5 proxy URL of the sandbox the calling process
// runs in. A kind lab's API server is on loopback, which every HTTP client
// exempts from the proxy environment, so the lab's kubeconfig names the
// sandbox's SOCKS proxy, which ends at beekeeper's egress proxy: an opaque
// tunnel, held to the allow list (127.0.0.1:<port>).
func SOCKSProxy(getenv func(string) string) (string, error) {
	for _, name := range []string{"ALL_PROXY", "all_proxy", "FTP_PROXY", "ftp_proxy"} {
		u, err := url.Parse(getenv(name))
		if err != nil || u.Host == "" || (u.Scheme != "socks5" && u.Scheme != "socks5h") {
			continue
		}
		// client-go dials socks5 only; the lab's server is an IP literal,
		// so where the name resolves makes no difference
		u.Scheme = "socks5"
		return u.String(), nil
	}
	return "", fmt.Errorf("the sandbox sets no SOCKS5 proxy (ALL_PROXY, FTP_PROXY)")
}

// ProxyKubeconfig sets proxy on every cluster of the kubeconfig raw whose
// server is on loopback, and reports whether that changed it.
func ProxyKubeconfig(raw []byte, proxy string) ([]byte, bool, error) {
	cfg, err := clientcmd.Load(raw)
	if err != nil {
		return nil, false, err
	}
	changed := false
	for _, c := range cfg.Clusters {
		u, err := url.Parse(c.Server)
		if err != nil || !config.Loopback(u.Hostname()) || c.ProxyURL == proxy {
			continue
		}
		c.ProxyURL, changed = proxy, true
	}
	if !changed {
		return raw, false, nil
	}
	out, err := clientcmd.Write(*cfg)
	return out, true, err
}
