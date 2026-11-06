package guard

import "testing"

const (
	reasonSyntax  = "restricted SELECT syntax"
	reasonOptions = "other options"
	listedQuery   = "select * from conversations limit 5"
	protectedName = "protected-lab"
)

func readOnlyExecHook(queries ...string) Hook {
	h := kubeHook(machineKC)
	h.Production = "production"
	h.ReadOnlyExecQueries = queries
	return h
}

func TestKubeReadOnlyExec(t *testing.T) {
	h := readOnlyExecHook(listedQuery, "select count(*) from public.conversations;", "select id, content from conversations")
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
		{"psql -X -c 'delete from conversations'", reasonSyntax},
		{"psql -X -c 'select * from other'", "not in kube.readOnlyExecQueries"},
		{"psql -X -c 'select * from conversations; delete from conversations'", reasonSyntax},
		{"psql -X -c 'select dangerous() from conversations'", reasonSyntax},
		{"psql -X -c 'select * into copy from conversations'", reasonSyntax},
		{"psql -X -c 'select * from conversations for update'", reasonSyntax},
		{"psql -X -c 'select * from conversations limit 5' -c 'delete from conversations'", reasonOptions},
		{"psql -X -f /tmp/query", reasonOptions},
		{"psql -X -d 'dbname=app' -c 'select * from conversations limit 5'", "literal names"},
		{"psql -X -c 'select * from conversations limit 5' -o '|sh'", reasonOptions},
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
	wantRefused(t, h, prefix+"psql -X -c 'select dangerous() from conversations'", reasonSyntax)
}

// A terminal gives psql a pager and a pager a shell escape, so the exec stays
// non-interactive however the flags are spelled.
func TestKubeReadOnlyExecNoTerminal(t *testing.T) {
	h := readOnlyExecHook(listedQuery)
	query := " pod/db -- psql -X -c '" + listedQuery + "'"
	for _, flag := range []string{"-i", "-t", "-it", "-ti", "--stdin", "--tty", "--stdin=true", "--tty=true"} {
		wantRefused(t, h, "kubectl --context production exec "+flag+query, "non-interactive")
	}
	wantPassed(t, h, "kubectl --context production exec -n app -c postgres"+query)
}

// kube.protectedContexts share the write gate, and with it the allowlist:
// only a listed query passes there, every other exec stays refused.
func TestKubeReadOnlyExecProtectedContext(t *testing.T) {
	h := readOnlyExecHook(listedQuery)
	h.Production = ""
	h.ProtectedContexts = []string{protectedName}
	prefix := "kubectl --context " + protectedName + " exec pod/db -- "
	listed := prefix + "psql -X -c '" + listedQuery + "'"
	wantPassed(t, h, listed)
	wantRefused(t, h, prefix+"psql -X -c 'select * from other'", "not in kube.readOnlyExecQueries")
	wantRefused(t, h, prefix+"sh", "only direct psql")
	h.ReadOnlyExecQueries = nil
	wantRefused(t, h, listed, "writes to "+protectedName)
}
