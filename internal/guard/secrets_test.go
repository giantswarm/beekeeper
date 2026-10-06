package guard

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The safe forms a refusal names.
const (
	keysForm    = "jq '.data|keys'"
	lengthForm  = "base64 -d | wc -c"
	sopsForm    = "sops runs only in beekeeper"
	opForm      = "op runs only in beekeeper"
	cryptForm   = "Decryption runs only in beekeeper"
	vaultForm   = "vault kv metadata get"
	renderForm  = "yq 'del(.data, .stringData)'"
	fileForm    = "yq '.data|keys' <file>"
	keyringForm = "The keyring's entries are the person's"
)

// The forms a refusal never offers: plaintext files and unkeyed hashes.
var unsafeForms = []string{"sha256sum", "--out-file", "--output <", "> <file>", "> file", "md5sum", "openssl dgst"}

// secretCorpus: every command and the safe form its refusal names, "" for a
// command that passes.
var secretCorpus = []struct{ cmd, safe string }{
	// kubectl get of Secrets that prints their data
	{"kubectl get secret app -n ns -o yaml", keysForm},
	{"kubectl get secrets -A -o json", keysForm},
	{"kubectl -n x get secret/app -oyaml", keysForm},
	{"kubectl --context a get secrets.v1 app --output=json", keysForm},
	{"kubectl get cm,secret -n x -o yaml", keysForm},
	{"kubectl get secret app -o jsonpath='{.data.clientSecret}' | base64 -d", keysForm},
	{"kubectl get secret app -o jsonpath='{.data}'", keysForm},
	{"kubectl get secret app -o jsonpath='{.items[*]}'", keysForm},
	{`kubectl get secret app -o go-template='{{.data.token | base64decode}}'`, keysForm},
	{`kubectl get secret app -o go-template='{{range $k, $v := .data}}{{$v}}{{end}}'`, keysForm},
	{`kubectl get secret app -o go-template='{{range $k, $v := .data}}{{$k}}={{$v}}{{end}}'`, keysForm},
	{"kubectl get secret tls -o jsonpath='{.data.tls\\.key}' | base64 -d | openssl rsa -text", keysForm},
	{"kubectl get secret app -o custom-columns=T:.data.token", keysForm},
	{"kubectl get secret app -o json | jq .data", keysForm},
	{"kubectl get secret app -o json | jq -r '.data.token' | base64 -d", keysForm},
	{"kubectl get secret app -o json | jq '.data | to_entries'", keysForm},
	{"kubectl get secret app -o json | jq '.data|keys, .data.token'", keysForm},
	{"kubectl get secret app -o json | jq '.'", keysForm},
	{"kubectl get secret app -o json | jq '(.data // {})'", keysForm},
	{"kubectl get secret app -o json | jq '.metadata.name, .[]'", keysForm},
	{`kubectl get secret app -o json | jq '.metadata.name, .["data"]'`, keysForm},
	{"kubectl get secret app -o yaml | grep token", keysForm},
	{"kubectl get secret app -o yaml | tee /dev/stderr | sha256sum", keysForm},
	{"kubectl get secrets -o name | xargs kubectl get -o yaml", keysForm},
	{"kubectl view-secret app token", keysForm},
	{"kubectl-view_secret app -a", keysForm},
	{"cd x && kubectl get secret app -o yaml; echo done", keysForm},
	{`echo "$(kubectl get secret app -o yaml)"`, keysForm},
	{`curl -H "Authorization: Bearer $(kubectl get secret t -o jsonpath='{.data.t}' | base64 -d)" x`, keysForm},
	{"diff <(kubectl get secret a -o yaml) <(kubectl get secret b -o yaml)", keysForm},
	{"bash -c 'kubectl get secret app -o yaml'", keysForm},
	{`zsh -lc "cd x; kubectl get secret app -o json | jq .data"`, keysForm},
	{"/home/u/.go/bin/beekeeper run -- zsh -c 'kubectl get secret app -o yaml'", keysForm},
	{"ssh host 'kubectl get secret app -o yaml'", keysForm},
	{"bash <<'EOF'\nkubectl get secret app -o yaml\nEOF", keysForm},
	{"timeout 30 ~/bin/kubectl get secret app -o yaml", keysForm},
	{`kc(){ timeout 60 kubectl --context a "$@"; }; kc -n x get secret app -o yaml`, keysForm},
	{`function kc { kubectl --context a "$@"; }; kc get secret app -o json | jq .data`, keysForm},
	{`K="kubectl --context a"; timeout 60 $K -n x get secret app -o yaml`, keysForm},
	{`KC=(kubectl --context a); "${KC[@]}" get secret app -o jsonpath='{.data.t}'`, keysForm},
	{`kg() { /usr/bin/kubectl "$@"; }; kg get secrets -o json`, keysForm},
	// to a file, a variable, a tee, a hash, a count or the clipboard
	{"kubectl get secret app -o yaml > ~/.local/state/x/app.yaml", keysForm},
	{"kubectl get secret app -o yaml >| \"$OUT\" 2>/dev/null", keysForm},
	{"kubectl get secret app -n a -o json | jq 'del(.metadata.uid)' > s.json", keysForm},
	{"kubectl get secret app -o yaml | tee app.yaml | yq '.data|keys'", keysForm},
	{"TOKEN=$(kubectl get secret t -o jsonpath='{.data.t}' | base64 -d)", keysForm},
	{"export TOKEN=\"$(kubectl get secret t -o jsonpath='{.data.t}' | base64 -d)\"; curl -sf -o /dev/null x", keysForm},
	{"kubectl get secret app -o jsonpath='{.data.clientSecret}' | base64 -d | sha256sum", keysForm},
	{"kubectl get secret app -o jsonpath='{.data.t}' | base64 -d | openssl dgst -sha256", keysForm},
	{"kubectl get secret app -o yaml | grep -c clientSecret", keysForm},
	{"kubectl get secret app -o jsonpath='{.data.t}' | base64 -d | wl-copy", keysForm},
	// metadata that carries the whole object (last-applied-configuration)
	{"kubectl get secret app -o yaml | yq '.metadata'", keysForm},
	{"kubectl get secret app -o jsonpath='{.metadata.annotations}'", keysForm},
	{`kubectl get secret app -o go-template='{{index .metadata.annotations "kubectl.kubernetes.io/last-applied-configuration"}}'`, keysForm},
	// other kubectl shapes that print values
	{"kubectl get --raw /api/v1/namespaces/x/secrets/app", keysForm},
	{"kubectl edit secret app", keysForm},
	{"KUBE_EDITOR=cat kubectl edit secret/app -n x", keysForm},
	{"kubectl create secret generic x --from-literal=a=b --dry-run=client -o yaml", keysForm},
	{"kubectl apply -f s.yaml secret/app -o yaml", keysForm},
	{"kubectl get secret app -o yaml | kubectl --context b apply -f - -o yaml", keysForm},
	{"kubectl get secret app -o yaml | kubectl --context b get -f -", keysForm},
	// sops and op, in every form
	{"sops -d secrets.enc.yaml", sopsForm},
	{"sops --decrypt --extract '[\"data\"]' x.yaml", sopsForm},
	{"sops decrypt x.yaml | grep password", sopsForm},
	{"sops -d x.yaml | yq .data", sopsForm},
	{"sops -d --output plain.yaml x.enc.yaml", sopsForm},
	{"sops -d x.enc.yaml | kubectl apply -f -", sopsForm},
	{"sops -d x.enc.yaml | yq '.stringData | keys'", sopsForm},
	{"sops -d x.enc.yaml | sha256sum", sopsForm},
	{"sops -d x.enc.yaml > x.yaml", sopsForm},
	{"sops -e -i x.yaml", sopsForm},
	{"sops x.yaml", sopsForm},
	{"timeout 30 sops -d x.yaml", sopsForm},
	{"sudo -u root sops -d x.yaml", sopsForm},
	{"env SOPS_AGE_KEY_FILE=k sops -d x.yaml", sopsForm},
	{"SOPS_AGE_KEY_FILE=k /usr/bin/sops -d x.yaml", sopsForm},
	{"xargs -n 1 sops -d < files", sopsForm},
	{"beekeeper run -- sops -d x.yaml", sopsForm},
	{"bash -c 'sops -d x.yaml'", sopsForm},
	{"helm secrets template x ./chart -f secrets.yaml", sopsForm},
	{"op read op://vault/item/password", opForm},
	{"op read --out-file ~/.kube/c op://v/i/kubeconfig", opForm},
	{"op read op://v/i/token | sha256sum", opForm},
	{"op read op://v/i/token | docker login registry.example -u x --password-stdin", opForm},
	{"op read op://v/i/token | gh secret set TOKEN --repo o/r", opForm},
	{"op read op://v/i/agekey | age-keygen -y", opForm},
	{"op document get kubeconfig", opForm},
	{"op inject -i tpl.yaml", opForm},
	{"op run --no-masking -- env", opForm},
	{"op run -- sops -d x.yaml", opForm},
	{"op run -- op read op://v/i/f", opForm},
	{"op run -- kubectl get secret app -o yaml", opForm},
	{"op run -- make test", opForm},
	{"op run --env-file .env -- make test", opForm},
	{"op run -- kubectl get secret app -o json | jq '.data|keys'", opForm},
	{"K=op://Employee/x/credential op run -- sh -c 'printenv K | kubectl --context kind-agentlab apply -f -'", opForm},
	{"secret-tool lookup service x account y", keyringForm},
	{"secret-tool search --all service x", keyringForm},
	{"security find-generic-password -s x -w", keyringForm},
	{"bash -c op\\ whoami", opForm},
	{"zsh -ic 'op whoami'", opForm},
	{"eval op whoami", opForm},
	{`eval "$(op signin)"`, "signs in by itself"},
	{"f(){ op item get x; }; f", opForm},
	{"function f { op item get x; }; f", opForm},
	{"if true; then op item get x; fi", opForm},
	{"{ op item get x; }", opForm},
	{"(op item get x)", opForm},
	{"op item list --vault x", opForm},
	{"op item get app", opForm},
	{"op item get app --fields label=password --reveal", opForm},
	{"op item get app --format json | jq '.fields|length'", opForm},
	{"op item get app --vault v > out.json", opForm},
	{"op --account a item get app", opForm},
	{"X=$(op item get app --fields token)", opForm},
	{"/usr/local/bin/op item get app", opForm},
	{`"op" 'item' get app`, opForm},
	{"xargs op item get <<< app", opForm},
	{"kubectl create secret generic x --from-literal=t=$(op read op://v/i/t) --dry-run=client -o yaml | kubectl apply -f -", opForm},
	// decryption
	{"age -d -i key.txt x.age", cryptForm},
	{"gpg --decrypt x.gpg > x", cryptForm},
	// vault: an allow list
	{"vault kv get secret/app", vaultForm},
	{"vault kv get -field=password secret/app", vaultForm},
	{"vault kv get -field=password secret/app > pw.txt", vaultForm},
	{"vault read -format=json secret/app | jq .data", vaultForm},
	{"vault token create", vaultForm},
	{"vault login -method=oidc", vaultForm},
	{"vault kv put secret/app a=b", vaultForm},
	// base64 of a secret's data
	{"yq '.data.token' secret.yaml | base64 -d", lengthForm},
	{"jq -r '.data.token' s.json | base64 --decode", lengthForm},
	{"yq '.data.token' secret.yaml | base64 -d | sha256sum", lengthForm},
	// renders fed with secret values, unless their Secret data is blanked
	{"helm template app ./chart -f values.yaml -f secrets.yaml", renderForm},
	{"helm template app ./chart --values=secrets.dec.yaml > out.yaml", renderForm},
	{"helm install app ./chart -f secret-values.yaml --dry-run", renderForm},
	{"helm template app ./chart --set-file key=token.txt", renderForm},
	{"helm template app ./chart -f secrets.yaml | yq 'del(.data)'", renderForm},
	{"helm template app ./chart -f secrets.yaml | sha256sum", renderForm},
	{"kustomize build --enable-alpha-plugins overlays/x", renderForm},
	{"kubectl kustomize --enable-alpha-plugins overlays/x | grep password", renderForm},
	{"diff <(helm template a ./c -f secrets.yaml) <(helm template b ./c -f secrets.yaml)", renderForm},
	// hashes and diffs of secret files
	{"sha256sum secrets.yaml", fileForm},
	{"md5sum ~/.kube/kubeconfig", fileForm},
	{"openssl dgst -sha256 password.txt", fileForm},
	{"diff secrets.yaml secrets.old.yaml", fileForm},
	{"cmp token.txt token.new", fileForm},
	{"git diff --no-index a/secrets.yaml b/secrets.yaml", fileForm},
	// shapes no rule names: refused by default
	{"kubectl get secret app -o yaml | awk '{print}'", keysForm},
	{"kubectl get secret app -o json | python3 -c 'import sys; print(sys.stdin.read())'", keysForm},
	{"kubectl get secret app -o yaml | base32", keysForm},
	{"kubectl get secret app -o custom-columns=X:.status", keysForm},
	{"op whoami", opForm},
	{"vault write auth/approle/login role_id=x", vaultForm},

	// passes: key names, metadata, lengths, quiet consumers, blanked renders
	{"kubectl get secrets -n ns", ""},
	{"kubectl get secrets -A -o name", ""},
	{"kubectl get secret app -o wide", ""},
	{"kubectl describe secret app", ""},
	{"kubectl get secret app -o json | jq '.data|keys'", ""},
	{"kubectl get secret app -o json | jq -r '.data | keys[]'", ""},
	{"kubectl get secret app -o jsonpath='{.data}' | jq keys", ""},
	{"kubectl get secrets -A -o json | jq '.items[] | {n: .metadata.name, k: (.data|keys)}'", ""},
	{"kubectl get secrets -o json | jq -r '.items[].metadata.name'", ""},
	{"kubectl -n x get secrets -o json | jq -c '[.items[] | {n: .metadata.name, k: (.data|keys)}] | .[:3]'", ""},
	{"kubectl get secrets -o json | jq '.items | map(.metadata.name) | .[0]'", ""},
	{"kubectl get secrets -A -o json | jq -c '[.items[] | {n: .metadata.name, k: ((.data // {})|keys)}]'", ""},
	{"kubectl get secret app -o yaml | yq '.metadata.labels'", ""},
	{"kubectl get secret app -o jsonpath='{.metadata.name}'", ""},
	{"kubectl get secret app -o jsonpath='{.metadata.labels}'", ""},
	{`kubectl get secret app -o go-template='{{range $k, $v := .data}}{{$k}}{{"\n"}}{{end}}'`, ""},
	{`kubectl get secret app -o go-template='{{len .data}} entries'`, ""},
	{"kubectl get secret tls -o jsonpath='{.data.tls\\.crt}' | base64 -d | openssl x509 -noout -subject -enddate", ""},
	{"kubectl get secrets -o jsonpath='{range .items[*]}{.metadata.name}{\"\\n\"}{end}'", ""},
	{"kubectl get secret app -o custom-columns=NAME:.metadata.name,TYPE:.type", ""},
	{"kubectl get secret app -o jsonpath='{.data.token}' | base64 -d | wc -c", ""},
	{"kubectl get secret app -o yaml >/dev/null", ""},
	{"kubectl get secret app -n a -o yaml | kubectl apply -n b -f -", ""},
	{"kubectl get secret app -n a -o yaml | kubectl --context b -n c apply -f -", ""},
	{"kubectl get secret app -n a -o yaml | kubectl --kubeconfig=k replace --force -f -", ""},
	{"kubectl create secret generic x --from-literal=a=b --dry-run=client -o yaml | kubectl apply -f -", ""},
	{`kc(){ kubectl --context a "$@"; }; kc get secret app -o json | jq '.data|keys'`, ""},
	{"kubectl get configmap x -o yaml", ""},
	{"kubectl get pods -o yaml", ""},
	{"kubectl get -f deploy.yaml -o yaml", ""},
	{"kubectl get --raw /api/v1/namespaces", ""},
	{"kubectl edit deployment app", ""},
	{"kubectl delete secret app", ""},
	{"vault status", ""},
	{"vault kv list secret/", ""},
	{"vault kv get -format=json secret/app | jq '.data.data|keys'", ""},
	{"vault kv metadata get secret/app", ""},
	{"echo aGk= | base64 -d", ""},
	{"helm template app ./chart -f values.yaml", ""},
	{"helm template app ./chart -f secrets.yaml | yq 'del(.data, .stringData)'", ""},
	{"helm template app ./chart -f secrets.yaml | yq '.data |= keys | .stringData |= keys' > out.yaml", ""},
	{"diff <(helm template a ./c -f secrets.yaml | yq 'del(.data, .stringData)') <(helm template b ./c -f secrets.yaml | yq 'del(.data, .stringData)')", ""},
	{"kustomize build overlays/x", ""},
	{"yq '.stringData|keys' x.enc.yaml", ""},
	{"diff a.yaml b.yaml", ""},
	{"diff x.enc.yaml y.enc.yaml", ""},
	{"sha256sum go.sum", ""},
	{"git diff --no-index internal/guard/secrets.go /tmp/x.go", ""},
	{"git diff main -- secrets.yaml", ""},
	{"ls ~/.config/sops", ""},
	{"grep -rn sops .", ""},
	{"echo op", ""},
	{"rg -n vault docs/", ""},
	{"gh repo view giantswarm/vault", ""},
	{"op-unlock", ""},
	{"git commit -m 'guard: refuse kubectl get secret -o yaml and sops -d'", ""},
	{"gh issue create --repo o/r --title x --body \"kubectl get secret x -o yaml prints values\"", ""},
	{"cat <<'EOF' >| body.md\nkubectl get secret x -o yaml\nsops -d x\nEOF", ""},
	{"grep -rn 'kubectl get secret' docs/ # kubectl get secret x -o yaml", ""},
	{"rg -n \"op read\" .", ""},
	{"rg -n 'op item get' .", ""},
	{"beekeeper secret copy a/src.sops.yaml b/dst.sops.yaml --name x --namespace y", ""},
	{"beekeeper secret compare a.sops.yaml b.sops.yaml#data.password", ""},
	{"beekeeper secret fingerprint op://Shared/item/password", ""},
	{"beekeeper secret copy op://Shared/item/password -- gh secret set TOKEN --repo o/r", ""},
	{"beekeeper secret set a.sops.yaml stringData.password --generate --vault op://Shared/item/password", ""},
}

func TestSecretGuardCorpus(t *testing.T) {
	for _, c := range secretCorpus {
		d := decide(t, hook(), "/", c.cmd, nil)
		refused := d != nil && d.PermissionDecision == decisionDeny
		switch {
		case c.safe == "" && refused:
			t.Errorf("%q is refused:\n%s", c.cmd, d.Reason)
		case c.safe == "":
		case !refused:
			t.Errorf("%q: want a refusal, got %+v", c.cmd, d)
		case !strings.HasPrefix(d.Reason, "Refused: `") || !strings.Contains(d.Reason, c.safe):
			t.Errorf("%q: the refusal does not name the safe form %q:\n%s", c.cmd, c.safe, d.Reason)
		case !strings.Contains(d.Reason, "beekeeper secret compare"):
			t.Errorf("%q: the refusal does not name beekeeper secret:\n%s", c.cmd, d.Reason)
		default:
			_, forms, _ := strings.Cut(d.Reason, "\n")
			for _, f := range unsafeForms {
				if strings.Contains(forms, f) {
					t.Errorf("%q: the refusal offers %q:\n%s", c.cmd, f, d.Reason)
				}
			}
		}
	}
}

// Every vault sign-in or unlock is refused in an agent session, however the
// command line reaches it: the person's helpers by name or path, op's own
// sign-ins, through a shell string, eval, a function, a script.
func TestSecretGuardRefusesVaultUnlocks(t *testing.T) {
	const helper = "vault-unlock"
	dir := t.TempDir()
	for name, body := range map[string]string{
		"signin.sh": "#!/bin/sh\nset -e\neval \"$(op signin --account team)\"\n",
		"helper.sh": "#!/bin/sh\n" + helper + "\n",
		"nested.sh": "#!/bin/sh\nbash ./signin.sh\n",
		"build.sh":  "#!/bin/sh\ngo build ./...\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	h := hook()
	h.UnlockCommands = []string{helper}
	for cmd, refused := range map[string]bool{
		"vault-unlock":                       true,
		"vault-unlock lock":                  true,
		"~/bin/vault-unlock":                 true,
		"command vault-unlock":               true,
		"zsh -ic vault-unlock":               true,
		"bash -c vault-unlock":               true,
		"zsh -c 'vault-unlock && make test'": true,
		"eval vault-unlock":                  true,
		"timeout 30 vault-unlock":            true,
		"op signin":                          true,
		"op signin --account team --raw":     true,
		"op --account team signin":           true,
		"op account add --address x":         true,
		`eval "$(op signin --account team)"`: true,
		"f(){ op signin; }; f":               true,
		"beekeeper secret unlock":            true,
		"bash signin.sh":                     true,
		"sh ./helper.sh":                     true,
		"./signin.sh":                        true,
		dir + "/signin.sh":                   true,
		"bash nested.sh":                     true,
		"source signin.sh":                   true,
		". ./helper.sh":                      true,
		"bash build.sh":                      false,
		"./build.sh":                         false,
		"echo vault-unlock":                  false,
		"grep -rn vault-unlock .":            false,
		"beekeeper secret fingerprint op://Shared/item/password":   false,
		"git commit -m 'guard: refuse op signin and vault-unlock'": false,
	} {
		d := decide(t, h, dir, cmd, nil)
		got := d != nil && d.PermissionDecision == decisionDeny
		switch {
		case got != refused:
			t.Errorf("%q: refused %v, want %v: %+v", cmd, got, refused, d)
		case refused && !strings.Contains(d.Reason, "beekeeper secret") || refused && strings.Contains(d.Reason, "op run --"):
			t.Errorf("%q: the refusal names no beekeeper secret, or offers op run:\n%s", cmd, d.Reason)
		}
	}
	for _, cmd := range []string{helper, "op signin", "zsh -ic vault-unlock", "./signin.sh"} {
		if d := decide(t, h, dir, cmd, nil); !strings.Contains(d.Reason, "signs in by itself") || strings.Contains(d.Reason, "beekeeper secret unlock") {
			t.Errorf("%q: the refusal does not name the broker's sign-in, or sends the person to a terminal:\n%s", cmd, d.Reason)
		}
	}
}

// Every op call is refused in an agent session, however the command line
// reaches it: op runs in beekeeper's broker alone.
func TestSecretGuardRefusesOp(t *testing.T) {
	dir := t.TempDir()
	for name, body := range map[string]string{
		"whoami.sh": "#!/bin/sh\nop whoami\n",
		"nested.sh": "#!/bin/sh\nsh ./whoami.sh\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, cmd := range []string{
		"op whoami",
		"op vault list",
		"/usr/bin/op whoami",
		"OP_ACCOUNT=team op whoami",
		"env OP_ACCOUNT=team op vault list",
		"command op whoami",
		"exec op whoami",
		"sudo -u teemow op whoami",
		"timeout 30 op whoami",
		"nohup op whoami",
		"setsid op whoami",
		"echo x | xargs op whoami",
		"bash -c 'op whoami'",
		"zsh -ic 'op vault list'",
		"eval 'op whoami'",
		"echo $(op whoami)",
		"f(){ op whoami; }; f",
		"sh whoami.sh",
		"./nested.sh",
		"source whoami.sh",
	} {
		d := decide(t, hook(), dir, cmd, nil)
		if d == nil || d.PermissionDecision != decisionDeny {
			t.Errorf("%q is not refused: %+v", cmd, d)
		}
	}
}

func TestCommandAt(t *testing.T) {
	for cmd, want := range map[string]string{
		"sops -d x":                     sopsCmd,
		"A=1 B=2 sops -d x":             sopsCmd,
		"timeout -k 5 30 sops -d x":     sopsCmd,
		"sudo -u root nice -n 10 op x":  "op",
		"beekeeper run -- sops -d x":    sopsCmd,
		"xargs -I{} sops -d {}":         sopsCmd,
		"ls ~/.config/sops":             "ls",
		"env":                           "",
		"/usr/local/bin/op item get a ": "/usr/local/bin/op",
	} {
		words := strings.Fields(cmd)
		got := ""
		if k := commandAt(words); k < len(words) {
			got = words[k]
		}
		if got != want {
			t.Errorf("commandAt(%q) = %q, want %q", cmd, got, want)
		}
	}
}
