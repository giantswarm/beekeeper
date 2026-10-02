package guard

import (
	"path/filepath"
	"strings"
	"testing"
)

const (
	machineKC = "testdata/kubeconfig-machine"
	gazelleKC = "testdata/kubeconfig-default-gazelle"
	labKC     = "testdata/kubeconfig-lab"
)

// kubeHook is a hook whose session uses kubeconfig by default, with
// testdata/kubeconfig-machine as the machine kubeconfig.
func kubeHook(kubeconfig string) Hook {
	h := hook()
	h.Kubeconfig, h.MachineKubeconfig = kubeconfig, machineKC
	return h
}

func wantRefused(t *testing.T, h Hook, cmd, reason string) {
	t.Helper()
	d := decide(t, h, "/", cmd, nil)
	if d == nil || d.PermissionDecision != decisionDeny {
		t.Errorf("%q: want a refusal, got %+v", cmd, d)
		return
	}
	if !strings.Contains(d.Reason, reason) {
		t.Errorf("%q: reason lacks %q:\n%s", cmd, reason, d.Reason)
	}
}

func wantPassed(t *testing.T, h Hook, cmd string) {
	t.Helper()
	if d := decide(t, h, "/", cmd, nil); d != nil && d.PermissionDecision == decisionDeny {
		t.Errorf("%q is refused:\n%s", cmd, d.Reason)
	}
}

const (
	switchMsg = "switches a kubeconfig's current context"
	prodMsg   = "a production cluster"
	gitopsMsg = "GitOps pull request and platformctl"
	setMsg    = "sets the current context of the machine kubeconfig"
)

func TestKubeRefusesContextSwitches(t *testing.T) {
	h := kubeHook(machineKC)
	for _, cmd := range []string{
		"kubectl config use-context teleport.giantswarm.io-gazelle",
		"kubectl --kubeconfig ~/.kube/config config use-context kind-lab",
		"kubectl config use kind-lab",
		"kubectl config set current-context kind-lab",
		"kubectl ctx kind-lab",
		"kubectl-ctx kind-lab",
		"kubectx kind-lab",
		"kubectx -",
		"kubectl gs login gazelle",
		"kubectl-gs login gazelle",
		"KUBECONFIG=x kubectl config use-context a",
		"timeout 30 /usr/bin/kubectl config use-context a",
		"ls && kubectl config use-context a",
		"true | kubectl config use-context a",
		"echo $(kubectl config use-context a)",
		"bash -c 'kubectl config use-context a'",
		`zsh -lc "cd /x && kubectl config set current-context a"`,
		"/home/u/.go/bin/beekeeper run -- zsh -c 'kubectl config use-context a'",
		"sh <<'EOF'\nkubectl config use-context a\nEOF",
		`K="kubectl"; $K config use-context a`,
		"K=kubectl; $K config use-context a",
		`kc(){ kubectl "$@"; }; kc config use-context a`,
		"xargs kubectl config use-context <<< a",
		`"kubectl" config 'use-context' a`,
	} {
		wantRefused(t, h, cmd, switchMsg)
	}
	for _, cmd := range []string{
		"tsh kube login gazelle",
		"tsh kube login --all",
		"tsh login --proxy=teleport.example --kube-cluster=gazelle",
		"KUBECONFIG=testdata/kubeconfig-machine tsh kube login gazelle",
		"kind create cluster --name lab",
		"kind export kubeconfig --name lab",
		"kind create cluster --name lab --kubeconfig testdata/kubeconfig-machine",
	} {
		wantRefused(t, h, cmd, setMsg)
	}
	for _, cmd := range []string{
		"kubectl config get-contexts -o name",
		"kubectl config current-context",
		"kubectl config unset current-context",
		"kubectl config view --minify --context kind-lab",
		"kubectl config set-context kind-lab --namespace x",
		"kubectx",
		"kubectx -c",
		"kubectl gs login gazelle --self-contained /tmp/x/kubeconfig",
		"KUBECONFIG=~/.local/state/x/kc tsh kube login gazelle",
		"tsh kube ls",
		"tsh login --proxy=teleport.example",
		"tsh status",
		"kind create cluster --name lab --kubeconfig ~/.local/state/lab/kc",
		"KUBECONFIG=~/.local/state/lab/kc kind create cluster --name lab",
		"kind get clusters",
		"git commit -m 'hook: refuse kubectl config use-context'",
		"gh pr create --repo o/r --title x --body \"kubectl config use-context is refused\"",
		"cat <<'EOF' >| body.md\nkubectl config use-context a\nEOF",
	} {
		wantPassed(t, h, cmd)
	}
}

func TestKubeRefusesProductionWrites(t *testing.T) {
	h := kubeHook(machineKC)
	for _, cmd := range []string{
		"kubectl --context teleport.giantswarm.io-gazelle apply -f x.yaml",
		"kubectl --context=teleport.giantswarm.io-gazelle -n flux-system delete pod x",
		"kubectl apply -f x.yaml --context teleport.giantswarm.io-gazelle",
		"kubectl --context teleport.giantswarm.io-gazelle-operations patch deploy x -p '{}'",
		"kubectl --context prod scale deploy x --replicas 0",
		"kubectl --context kind-lab --cluster teleport.giantswarm.io-gazelle delete ns x",
		"kubectl --context teleport.giantswarm.io-gazelle rollout restart deploy/x",
		"kubectl --context teleport.giantswarm.io-gazelle annotate hr x a=b --overwrite",
		"kubectl --context teleport.giantswarm.io-gazelle label ns x a=b",
		"kubectl --context teleport.giantswarm.io-gazelle cordon node1",
		"kubectl --context teleport.giantswarm.io-gazelle drain node1 --ignore-daemonsets",
		"kubectl --context teleport.giantswarm.io-gazelle edit cm x",
		"kubectl --context teleport.giantswarm.io-gazelle exec -it x -- sh",
		"kubectl --context teleport.giantswarm.io-gazelle create ns x",
		"kubectl --context teleport.giantswarm.io-gazelle replace -f x.yaml",
		"kubectl --context teleport.giantswarm.io-gazelle apply --dry-run=none -f x",
		"helm --kube-context teleport.giantswarm.io-gazelle upgrade --install x chart",
		"helm uninstall x --kube-context=teleport.giantswarm.io-gazelle",
		"helm rollback x 3 --kube-context teleport.giantswarm.io-gazelle",
		"HELM_KUBECONTEXT=teleport.giantswarm.io-gazelle helm install x chart",
		"flux --context teleport.giantswarm.io-gazelle reconcile kustomization flux -n flux-giantswarm",
		"flux suspend hr x --context teleport.giantswarm.io-gazelle",
		"flux resume hr x --context=teleport.giantswarm.io-gazelle",
		// Bypasses: wrappers, shells, variables, a kubeconfig whose current
		// context is production.
		"sudo -E kubectl --context teleport.giantswarm.io-gazelle delete pod x",
		"env FOO=1 kubectl --context teleport.giantswarm.io-gazelle delete pod x",
		"bash -c 'kubectl --context teleport.giantswarm.io-gazelle delete pod x'",
		"/home/u/.go/bin/beekeeper run -- zsh -c 'helm --kube-context teleport.giantswarm.io-gazelle upgrade x c'",
		"kubectl get pods -o name | xargs kubectl --context teleport.giantswarm.io-gazelle delete",
		`K="kubectl --context teleport.giantswarm.io-gazelle"; $K delete pod x`,
		`kg(){ kubectl --context teleport.giantswarm.io-gazelle "$@"; }; kg delete pod x`,
		"KUBECONFIG=testdata/kubeconfig-default-gazelle kubectl delete pod x",
		"export KUBECONFIG=testdata/kubeconfig-default-gazelle; kubectl apply -f x",
		"KUBECONFIG=testdata/kubeconfig-default-gazelle && export KUBECONFIG && kubectl apply -f x",
		"kubectl --kubeconfig testdata/kubeconfig-default-gazelle apply -f x",
		"helm --kubeconfig=testdata/kubeconfig-default-gazelle upgrade x c",
		"KUBECONFIG=testdata/kubeconfig-lab:testdata/kubeconfig-default-gazelle kubectl --context prod delete pod x",
	} {
		wantRefused(t, h, cmd, gitopsMsg)
	}
	// The session's default kubeconfig points at production.
	for _, cmd := range []string{
		"kubectl apply -f x.yaml",
		"helm upgrade x chart",
		"flux reconcile hr x",
		"unset KUBECONFIG; kubectl --kubeconfig testdata/kubeconfig-default-gazelle delete pod x",
	} {
		wantRefused(t, kubeHook(gazelleKC), cmd, prodMsg)
	}
	for _, cmd := range []string{
		"kubectl --context teleport.giantswarm.io-gazelle get pods -A",
		"kubectl --context teleport.giantswarm.io-gazelle describe hr x -n y",
		"kubectl --context teleport.giantswarm.io-gazelle logs deploy/x",
		"kubectl --context teleport.giantswarm.io-gazelle top nodes",
		"kubectl --context teleport.giantswarm.io-gazelle auth can-i delete pods",
		"kubectl --context teleport.giantswarm.io-gazelle rollout status deploy/x",
		"kubectl --context teleport.giantswarm.io-gazelle apply --dry-run=server -f x.yaml",
		"kubectl --context teleport.giantswarm.io-gazelle diff -f x.yaml",
		"kubectl --context teleport.giantswarm.io-gazelle wait --for=condition=Ready hr/x",
		"helm --kube-context teleport.giantswarm.io-gazelle list -A",
		"helm --kube-context teleport.giantswarm.io-gazelle status x",
		"helm --kube-context teleport.giantswarm.io-gazelle get values x",
		"helm template x chart",
		"helm upgrade x chart --dry-run --kube-context teleport.giantswarm.io-gazelle",
		"flux --context teleport.giantswarm.io-gazelle get hr -A",
		"flux --context teleport.giantswarm.io-gazelle logs",
		"flux create source oci x --url oci://x --export",
		"kubectl --context teleport.giantswarm.io-graveler apply -f x.yaml",
		"kubectl --context kind-agentlab apply -f x.yaml",
		"KUBECONFIG=testdata/kubeconfig-lab kubectl apply -f x.yaml",
		"kubectl --kubeconfig testdata/kubeconfig-lab --context kind-agentlab delete pod x",
		"helm --kubeconfig testdata/kubeconfig-lab upgrade --install x chart",
		"kubectl apply -f x.yaml",
		"kubectl --context gazelleish apply -f x",
		"git commit -m 'no kubectl --context teleport.giantswarm.io-gazelle apply'",
		"grep -rn 'kubectl delete' docs/",
	} {
		wantPassed(t, h, cmd)
	}
	// A lab kubeconfig overrides a production default.
	wantPassed(t, kubeHook(gazelleKC), "KUBECONFIG=testdata/kubeconfig-lab kubectl apply -f x")
	wantPassed(t, kubeHook(gazelleKC), "kubectl --context kind-agentlab apply -f x")
}

func TestKubeRefusesProductionPluginWrites(t *testing.T) {
	h := kubeHook(machineKC)
	for _, cmd := range []string{
		"kubectl ate delete actor x -a ate-golden --context teleport.giantswarm.io-gazelle",
		"kubectl ate --context teleport.giantswarm.io-gazelle suspend actor x",
		"kubectl --context teleport.giantswarm.io-gazelle ate admin make-ca-pool --name x",
		"kubectl ate -a ate-golden --context=teleport.giantswarm.io-gazelle-operations pause actor x",
		"kubectl-ate admin make-jwt-pool --context teleport.giantswarm.io-gazelle --name x",
		"kubectl-ate --context teleport.giantswarm.io-gazelle-cicdprod create actor x --template t",
		"kubectl-ate --context teleport.giantswarm.io-gazelle delete actor-template x -a y",
		"kubectl-ate --kubeconfig testdata/kubeconfig-default-gazelle resume actor x",
		"/home/u/bin/kubectl-ate --context teleport.giantswarm.io-gazelle delete actor x",
		"go run ./cmd/kubectl-ate --context teleport.giantswarm.io-gazelle delete actor x",
		"kubectl gs update app --name x --version 1 --context teleport.giantswarm.io-gazelle",
		"kubectl gadget deploy --context teleport.giantswarm.io-gazelle",
		"kubectl-some_plugin apply --context teleport.giantswarm.io-gazelle",
		// Bypasses: wrappers, shells, heredocs, xargs, variables, functions,
		// a kubeconfig whose current context is production.
		"sudo -E kubectl-ate --context teleport.giantswarm.io-gazelle delete actor x",
		"env FOO=1 kubectl ate --context teleport.giantswarm.io-gazelle delete actor x",
		"timeout 30 kubectl-ate --context teleport.giantswarm.io-gazelle delete actor x",
		"ls && kubectl ate --context teleport.giantswarm.io-gazelle delete actor x",
		"echo $(kubectl-ate --context teleport.giantswarm.io-gazelle delete actor x)",
		"bash -c 'kubectl ate --context teleport.giantswarm.io-gazelle delete actor x'",
		`zsh -lc "cd /x && kubectl-ate admin make-ca-pool --context teleport.giantswarm.io-gazelle"`,
		"/home/u/.go/bin/beekeeper run -- zsh -c 'kubectl-ate --context teleport.giantswarm.io-gazelle delete actor x'",
		"sh <<'EOF'\nkubectl ate --context teleport.giantswarm.io-gazelle delete actor x\nEOF",
		"echo x | xargs kubectl-ate --context teleport.giantswarm.io-gazelle delete actor",
		"echo x | xargs kubectl ate --context teleport.giantswarm.io-gazelle delete actor",
		`K="kubectl --context teleport.giantswarm.io-gazelle"; $K ate delete actor x`,
		`A="kubectl-ate --context teleport.giantswarm.io-gazelle"; $A delete actor x`,
		`A=/home/u/bin/kubectl-ate; $A --context teleport.giantswarm.io-gazelle admin make-ca-pool`,
		`ka(){ kubectl-ate --context teleport.giantswarm.io-gazelle "$@"; }; ka delete actor x`,
		`kg(){ kubectl --context teleport.giantswarm.io-gazelle "$@"; }; kg ate delete actor x`,
		`"kubectl-ate" --context teleport.giantswarm.io-gazelle 'delete' actor x`,
		"KUBECONFIG=testdata/kubeconfig-default-gazelle kubectl ate delete actor x",
		"export KUBECONFIG=testdata/kubeconfig-default-gazelle; kubectl-ate admin make-ca-pool",
	} {
		wantRefused(t, h, cmd, gitopsMsg)
	}
	for _, cmd := range []string{
		"kubectl ate delete actor x",
		"kubectl-ate admin make-ca-pool --name x",
	} {
		wantRefused(t, kubeHook(gazelleKC), cmd, prodMsg)
	}
	for _, cmd := range []string{
		"kubectl ate get actors -A --context teleport.giantswarm.io-gazelle",
		"kubectl ate -a ate-golden get actors --context teleport.giantswarm.io-gazelle",
		"kubectl-ate --context teleport.giantswarm.io-gazelle get workers -n kagent",
		"kubectl-ate --context teleport.giantswarm.io-gazelle logs actors x -a y",
		"kubectl-ate --context teleport.giantswarm.io-gazelle top workers",
		"kubectl-ate --help",
		"kubectl ate",
		"kubectl-ate --context kind-agentlab delete actor x",
		"kubectl-ate --kubeconfig testdata/kubeconfig-lab admin make-ca-pool --name x",
		"KUBECONFIG=testdata/kubeconfig-lab kubectl ate delete actor x",
		"kubectl-ate --context teleport.giantswarm.io-graveler delete actor x",
		"kubectl tree deploy x --context teleport.giantswarm.io-gazelle",
		"kubectl resource-capacity --context teleport.giantswarm.io-gazelle",
		"kubectl gs get clusters --context teleport.giantswarm.io-gazelle",
		"kubectl gs template cluster --provider capa --name x",
		"kubectl krew install tree",
		"kubectl ctx",
		"git commit -m 'guard kubectl-ate --context teleport.giantswarm.io-gazelle delete actor x'",
		"rg -n 'kubectl-ate delete' docs/",
	} {
		wantPassed(t, h, cmd)
	}
	wantPassed(t, kubeHook(gazelleKC), "kubectl-ate --context kind-agentlab delete actor x")
}

func TestCurrentContext(t *testing.T) {
	for kc, want := range map[string]string{
		machineKC:                         "",
		gazelleKC:                         "teleport.giantswarm.io-gazelle",
		labKC + ":" + gazelleKC:           "kind-agentlab",
		filepath.Join(t.TempDir(), "nop"): "",
		"":                                "",
	} {
		if got := CurrentContext(kc); got != want {
			t.Errorf("CurrentContext(%q) = %q, want %q", kc, got, want)
		}
	}
}

// Without a production installation the kube guard is off.
func TestKubeGuardOffWithoutProduction(t *testing.T) {
	h := kubeHook(machineKC)
	h.Production = ""
	for _, cmd := range []string{
		"kubectl --context teleport.giantswarm.io-gazelle apply -f x.yaml",
		"kubectl config use-context teleport.giantswarm.io-gazelle",
		"tsh kube login gazelle",
	} {
		wantPassed(t, h, cmd)
	}
	if KubeGuardOff("") == "" || KubeGuardOff("gazelle") != "" {
		t.Error("KubeGuardOff reports the guard wrong")
	}
}
