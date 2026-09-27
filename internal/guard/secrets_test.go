package guard

import (
	"strings"
	"testing"
)

func TestHookRefusesSecretReads(t *testing.T) {
	for _, c := range []struct {
		cmd, safe string
	}{
		{"kubectl get secret app -n ns -o yaml", "jq '.data|keys'"},
		{"kubectl get secrets -A -o json", "jq '.data|keys'"},
		{"kubectl -n x get secret/app -oyaml", "jq '.data|keys'"},
		{"kubectl --context a get secrets.v1 app --output=json", "jq '.data|keys'"},
		{"kubectl get cm,secret -n x -o yaml", "jq '.data|keys'"},
		{"kubectl get secret app -o jsonpath='{.data.clientSecret}' | base64 -d", "sha256sum"},
		{"kubectl get secret app -o jsonpath='{.data}'", "jq '.data|keys'"},
		{"kubectl get secret app -o jsonpath='{.items[*]}'", "jq '.data|keys'"},
		{`kubectl get secret app -o go-template='{{.data.token | base64decode}}'`, "jq '.data|keys'"},
		{`kubectl get secret app -o go-template='{{range $k, $v := .data}}{{$v}}{{end}}'`, "jq '.data|keys'"},
		{`kubectl get secret app -o go-template='{{range $k, $v := .data}}{{$k}}={{$v}}{{end}}'`, "jq '.data|keys'"},
		{"kubectl get secret tls -o jsonpath='{.data.tls\\.key}' | base64 -d | openssl rsa -text", "sha256sum"},
		{"kubectl get secret app -o custom-columns=T:.data.token", "jq '.data|keys'"},
		{"kubectl get secret app -o json | jq .data", "jq '.data|keys'"},
		{"kubectl get secret app -o json | jq -r '.data.token' | base64 -d", "sha256sum"},
		{"kubectl get secret app -o json | jq '.data | to_entries'", "jq '.data|keys'"},
		{"kubectl get secret app -o json | jq '.data|keys, .data.token'", "jq '.data|keys'"},
		{"kubectl get secret app -o json | jq '.'", "jq '.data|keys'"},
		{"kubectl get secret app -o yaml | grep token", "jq '.data|keys'"},
		{"kubectl get secret app -o yaml | tee /dev/stderr | sha256sum", "jq '.data|keys'"},
		{"kubectl get secrets -o name | xargs kubectl get -o yaml", "jq '.data|keys'"},
		{"kubectl view-secret app token", "jq '.data|keys'"},
		{"kubectl-view_secret app -a", "jq '.data|keys'"},
		{"cd x && kubectl get secret app -o yaml; echo done", "jq '.data|keys'"},
		{`echo "$(kubectl get secret app -o yaml)"`, "jq '.data|keys'"},
		{`curl -H "Authorization: Bearer $(kubectl get secret t -o jsonpath='{.data.t}' | base64 -d)" x`, "sha256sum"},
		{"diff <(kubectl get secret a -o yaml) <(kubectl get secret b -o yaml)", "jq '.data|keys'"},
		{"bash -c 'kubectl get secret app -o yaml'", "jq '.data|keys'"},
		{`zsh -lc "cd x; kubectl get secret app -o json | jq .data"`, "jq '.data|keys'"},
		{"/home/u/.go/bin/beekeeper run -- zsh -c 'kubectl get secret app -o yaml'", "jq '.data|keys'"},
		{"ssh host 'kubectl get secret app -o yaml'", "jq '.data|keys'"},
		{"bash <<'EOF'\nkubectl get secret app -o yaml\nEOF", "jq '.data|keys'"},
		{"timeout 30 ~/bin/kubectl get secret app -o yaml", "jq '.data|keys'"},
		{"sops -d secrets.enc.yaml", "sops -d --output"},
		{"sops --decrypt --extract '[\"data\"]' x.yaml", "sops -d --output"},
		{"sops decrypt x.yaml | grep password", "sops -d --output"},
		{"sops -d x.yaml | yq .data", "sops -d --output"},
		{"op read op://vault/item/password", "op read --out-file"},
		{"op item get app --fields password --reveal", "op read --out-file"},
		{"op item get app --format json", "op read --out-file"},
		{"op document get kubeconfig", "op read --out-file"},
		{"op inject -i tpl.yaml", "op read --out-file"},
		{"op run --no-masking -- env", "op read --out-file"},
		{"vault kv get secret/app", "vault kv get -field="},
		{"vault kv get -field=password secret/app", "vault kv get -field="},
		{"vault read -format=json secret/app | jq .data", "vault kv get -field="},
		{"yq '.data.token' secret.yaml | base64 -d", "base64 -d | sha256sum"},
		{"jq -r '.data.token' s.json | base64 --decode", "base64 -d | sha256sum"},
	} {
		d := decide(t, hook(), "/", c.cmd, nil)
		if d == nil || d.PermissionDecision != decisionDeny {
			t.Errorf("%q: want a refusal, got %+v", c.cmd, d)
			continue
		}
		if !strings.Contains(d.Reason, c.safe) || !strings.HasPrefix(d.Reason, "Refused: `") {
			t.Errorf("%q: the refusal does not name the safe form %q:\n%s", c.cmd, c.safe, d.Reason)
		}
	}
}

func TestHookAllowsSafeSecretReads(t *testing.T) {
	for _, cmd := range []string{
		"kubectl get secrets -n ns",
		"kubectl get secrets -A -o name",
		"kubectl get secret app -o wide",
		"kubectl describe secret app",
		"kubectl get secret app -o json | jq '.data|keys'",
		"kubectl get secret app -o json | jq -r '.data | keys[]'",
		"kubectl get secret app -o jsonpath='{.data}' | jq keys",
		"kubectl get secrets -A -o json | jq '.items[] | {n: .metadata.name, k: (.data|keys)}'",
		"kubectl get secrets -o json | jq -r '.items[].metadata.name'",
		"kubectl get secret app -o yaml | yq '.metadata'",
		"kubectl get secret app -o jsonpath='{.metadata.name}'",
		`kubectl get secret app -o go-template='{{range $k, $v := .data}}{{$k}}{{"\n"}}{{end}}'`,
		`kubectl get secret app -o go-template='{{len .data}} entries'`,
		"kubectl get secret tls -o jsonpath='{.data.tls\\.crt}' | base64 -d | openssl x509 -noout -subject -enddate",
		"op read op://v/i/agekey | age-keygen -y",
		`op item get x --format json | jq -r '.fields[] | "\(.label) len=\(.value|length)"'`,
		"kubectl get secrets -o jsonpath='{range .items[*]}{.metadata.name}{\"\\n\"}{end}'",
		"kubectl get secret app -o custom-columns=NAME:.metadata.name,TYPE:.type",
		"kubectl get secret app -o jsonpath='{.data.clientSecret}' | base64 -d | sha256sum",
		"kubectl get secret app -o jsonpath='{.data.token}' | base64 -d | wc -c",
		"kubectl get secret app -o yaml | grep -c clientSecret",
		"kubectl get secret app -o yaml > ~/.local/state/x/app.yaml",
		"kubectl get secret app -o yaml >| \"$OUT\" 2>/dev/null",
		"kubectl get secret app -n a -o json | jq 'del(.metadata.uid)' > s.json",
		"kubectl get secret app -n a -o yaml | kubectl apply -n b -f -",
		"TOKEN=$(kubectl get secret t -o jsonpath='{.data.t}' | base64 -d)",
		"export TOKEN=\"$(kubectl get secret t -o jsonpath='{.data.t}' | base64 -d)\"; curl -sf -o /dev/null x",
		"kubectl create secret generic x --from-literal=t=$(op read op://v/i/t) --dry-run=client -o yaml | kubectl apply -f -",
		"kubectl get configmap x -o yaml",
		"kubectl get pods -o yaml",
		"kubectl get -f deploy.yaml -o yaml",
		"sops -e -i x.yaml",
		"sops x.yaml",
		"sops -d --output plain.yaml x.enc.yaml",
		"sops -d x.enc.yaml | kubectl apply -f -",
		"sops -d x.enc.yaml | yq '.stringData | keys'",
		"sops -d x.enc.yaml | sha256sum",
		"op read --out-file ~/.kube/c op://v/i/kubeconfig",
		"op read op://v/i/token | docker login registry.example -u x --password-stdin",
		"op read op://v/i/token | gh secret set TOKEN --repo o/r",
		"op item get app --fields password",
		"op item list --vault x",
		"op run --env-file .env -- make test",
		"vault kv get -field=password secret/app > pw.txt",
		"vault kv get -format=json secret/app | jq '.data.data|keys'",
		"vault kv metadata get secret/app",
		"echo aGk= | base64 -d",
		"git commit -m 'guard: refuse kubectl get secret -o yaml and sops -d'",
		"gh issue create --repo o/r --title x --body \"kubectl get secret x -o yaml prints values\"",
		"cat <<'EOF' >| body.md\nkubectl get secret x -o yaml\nsops -d x\nEOF",
		"grep -rn 'kubectl get secret' docs/ # kubectl get secret x -o yaml",
		"rg -n \"op read\" .",
	} {
		if d := decide(t, hook(), "/", cmd, nil); d != nil && d.PermissionDecision == decisionDeny {
			t.Errorf("%q is refused:\n%s", cmd, d.Reason)
		}
	}
}

func TestHookRefusesSecretReadsThroughKubectlWrappers(t *testing.T) {
	for _, cmd := range []string{
		`kc(){ timeout 60 kubectl --context a "$@"; }; kc -n x get secret app -o yaml`,
		`function kc { kubectl --context a "$@"; }; kc get secret app -o json | jq .data`,
		`K="kubectl --context a"; timeout 60 $K -n x get secret app -o yaml`,
		`KC=(kubectl --context a); "${KC[@]}" get secret app -o jsonpath='{.data.t}'`,
		`kg() { /usr/bin/kubectl "$@"; }; kg get secrets -o json`,
	} {
		d := decide(t, hook(), "/", cmd, nil)
		if d == nil || d.PermissionDecision != decisionDeny {
			t.Errorf("%q: want a refusal, got %+v", cmd, d)
		}
	}
	d := decide(t, hook(), "/", `kc(){ kubectl --context a "$@"; }; kc get secret app -o json | jq '.data|keys'`, nil)
	if d != nil && d.PermissionDecision == decisionDeny {
		t.Errorf("the keys form through a wrapper is refused: %s", d.Reason)
	}
}
