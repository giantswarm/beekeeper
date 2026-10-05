package sandbox

import (
	"net/url"
	"strings"
	"testing"

	"k8s.io/client-go/tools/clientcmd"
)

func TestSOCKSProxy(t *testing.T) {
	proxy := func(scheme, host string) string {
		return (&url.URL{Scheme: scheme, User: url.UserPassword("srt", "placeholder"), Host: host}).String()
	}
	env := map[string]string{
		"ALL_PROXY": proxy("http", "localhost:3128"),
		"FTP_PROXY": proxy("socks5h", "localhost:1080"),
	}
	got, err := SOCKSProxy(func(k string) string { return env[k] })
	if err != nil || got != proxy("socks5", "localhost:1080") {
		t.Errorf("SOCKSProxy = %q, %v", got, err)
	}
	if _, err := SOCKSProxy(func(string) string { return "" }); err == nil {
		t.Error("no SOCKS proxy in the environment: no error")
	}
	delete(env, "FTP_PROXY")
	env["ftp_proxy"] = "socks5h://localhost:1080"
	if _, err := SOCKSProxy(func(k string) string { return env[k] }); err == nil {
		t.Error("a proxy without credentials is no sandbox proxy")
	}
}

const kindKubeconfig = `apiVersion: v1
kind: Config
clusters:
- name: kind-lab
  cluster:
    server: https://127.0.0.1:6443
    certificate-authority-data: Y2E=
- name: remote
  cluster:
    server: https://api.example.com
contexts:
- name: kind-lab
  context: {cluster: kind-lab, user: kind-lab}
current-context: kind-lab
users:
- name: kind-lab
  user: {token: t}
`

func TestProxyKubeconfig(t *testing.T) {
	proxy := (&url.URL{Scheme: "socks5", User: url.UserPassword("srt", "placeholder"), Host: "localhost:1080"}).String()
	out, changed, err := ProxyKubeconfig([]byte(kindKubeconfig), proxy)
	if err != nil || !changed {
		t.Fatalf("changed %v, %v", changed, err)
	}
	cfg, err := clientcmd.Load(out)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Clusters["kind-lab"].ProxyURL; got != proxy {
		t.Errorf("the lab's proxy-url = %q", got)
	}
	if got := cfg.Clusters["remote"].ProxyURL; got != "" {
		t.Errorf("a remote cluster's proxy-url = %q", got)
	}
	if cfg.CurrentContext != "kind-lab" || cfg.AuthInfos["kind-lab"].Token != "t" {
		t.Errorf("the rest of the kubeconfig changed: %s", out)
	}
	again, changed, err := ProxyKubeconfig(out, proxy)
	if err != nil || changed || string(again) != string(out) {
		t.Errorf("the same proxy again: changed %v, %v", changed, err)
	}
	if _, _, err := ProxyKubeconfig([]byte("clusters: ["), proxy); err == nil || strings.Contains(err.Error(), "placeholder") {
		t.Errorf("a broken kubeconfig: %v", err)
	}
}
