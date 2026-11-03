package config

import "testing"

func TestWorkerGitHubConfigRequiresScopedAppAuthority(t *testing.T) {
	if err := (*WorkerGitHub)(nil).Validate(); err != nil {
		t.Fatal(err)
	}
	valid := func() *WorkerGitHub {
		return &WorkerGitHub{ClientID: "Iv23liFixtureClientID", RefreshToken: "op://Shared/worker-app/refresh-token"}
	}
	if err := valid().Validate(); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*WorkerGitHub){
		func(w *WorkerGitHub) { w.ClientID = "" },
		func(w *WorkerGitHub) { w.ClientID = "invalid client id" },
		func(w *WorkerGitHub) { w.RefreshToken = "raw-token-material" },
		func(w *WorkerGitHub) { w.RefreshToken = "op://Shared/worker-app/refresh-token\ninjected" },
	} {
		w := valid()
		change(w)
		if w.Validate() == nil {
			t.Fatal("incomplete App authority accepted")
		}
	}
}
