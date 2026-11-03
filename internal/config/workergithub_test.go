package config

import "testing"

const workerGitHubOwner = "example"

func TestWorkerGitHubConfigRequiresScopedAppAuthority(t *testing.T) {
	if err := (*WorkerGitHub)(nil).Validate(); err != nil {
		t.Fatal(err)
	}
	valid := func() *WorkerGitHub {
		return &WorkerGitHub{AppID: 42, PrivateKey: "op://Shared/worker-app/private-key", Installations: map[string]int64{workerGitHubOwner: 42}}
	}
	if err := valid().Validate(); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*WorkerGitHub){func(w *WorkerGitHub) { w.AppID = 0 }, func(w *WorkerGitHub) { w.PrivateKey = "raw-key-material" }, func(w *WorkerGitHub) { w.Installations = nil }, func(w *WorkerGitHub) { w.Installations = map[string]int64{workerGitHubOwner: 0} }, func(w *WorkerGitHub) { w.Installations = map[string]int64{"Example": 42} }} {
		w := valid()
		change(w)
		if w.Validate() == nil {
			t.Fatal("incomplete App authority accepted")
		}
	}
}
