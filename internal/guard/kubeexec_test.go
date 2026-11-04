package guard

import "testing"

func TestKubeReadOnlyExec(t *testing.T) {
	h := kubeHook(machineKC)
	h.Production = "production"
	h.ReadOnlyExecQueries = []string{"select * from conversations limit 5", "select count(*) from public.conversations;", "select id, content from conversations"}
	prefix := "kubectl --context production exec -n app pod/db -- "
	for _, cmd := range []string{
		"psql -X -c 'select * from conversations limit 5'",
		"psql -X -U reader -d app -c 'select count(*) from public.conversations;'",
		"psql -X -c 'select id, content from conversations'",
	} {
		wantPassed(t, h, prefix+cmd)
	}
	for _, tt := range []struct{ cmd, reason string }{
		{"sh -c 'psql -c select'", "only direct psql"},
		{"psql -c 'select * from conversations limit 5'", "psql -X is required"},
		{"psql -X -c 'delete from conversations'", "restricted SELECT syntax"},
		{"psql -X -c 'select * from other'", "not in kube.readOnlyExecQueries"},
		{"psql -X -c 'select * from conversations; delete from conversations'", "restricted SELECT syntax"},
		{"psql -X -c 'select dangerous() from conversations'", "restricted SELECT syntax"},
		{"psql -X -c 'select * into copy from conversations'", "restricted SELECT syntax"},
		{"psql -X -c 'select * from conversations for update'", "restricted SELECT syntax"},
		{"psql -X -c 'select * from conversations limit 5' -c 'delete from conversations'", "other options"},
		{"psql -X -f /tmp/query", "other options"},
		{"psql -X -d 'dbname=app' -c 'select * from conversations limit 5'", "literal names"},
		{"psql -X -c 'select * from conversations limit 5' -o '|sh'", "other options"},
	} {
		wantRefused(t, h, prefix+tt.cmd, tt.reason)
	}
	for _, flag := range []string{"--dry-run=client", "--local"} {
		wantRefused(t, h, "kubectl --context production exec "+flag+" pod/db -- sh", "only direct psql")
	}
	wantRefused(t, h, "kubectl exec pod/db -- psql -X -c 'select * from conversations limit 5'", contextMsg)
	h.ReadOnlyExecQueries = nil
	wantRefused(t, h, prefix+"psql -X -c 'select * from conversations limit 5'", "not in kube.readOnlyExecQueries")
	// A configured unsafe query still cannot pass.
	h.ReadOnlyExecQueries = []string{"select dangerous() from conversations"}
	wantRefused(t, h, prefix+"psql -X -c 'select dangerous() from conversations'", "restricted SELECT syntax")
}
