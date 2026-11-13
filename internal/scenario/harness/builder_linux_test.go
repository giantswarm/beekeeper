//go:build scenario && linux

package harness

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestKubeArgsDropTheClusterFlags(t *testing.T) {
	got := kubeArgs([]string{"--kubeconfig", "/k", "--context", "c", "--namespace", "n", "--request-timeout=0", "exec", "pod", "-c", "builder", "-i", "--", "bash", "-c", "echo --context"})
	want := []string{"exec", "pod", "-c", "builder", "-i", "--", "bash", "-c", "echo --context"}
	if !slices.Equal(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestBuilderValidate(t *testing.T) {
	if err := (&Builder{Tools: []string{"go", "golangci-lint"}}).Validate(); err != nil {
		t.Fatal(err)
	}
	if err := (*Builder)(nil).Validate(); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		tools []string
		want  string
	}{
		{[]string{"cargo"}, `"cargo" is none of go, golangci-lint`},
		{[]string{"go", "go"}, `"go" twice`},
	} {
		err := (&Builder{Tools: tc.tools}).Validate()
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("tools %v: %v, want %q", tc.tools, err, tc.want)
		}
	}
}

// The cluster of a scenario is the Jobs the desk created: a Job is there
// once created, has one running pod, lends that pod's exec its container
// environment under the image's PATH, and takes the pod's /work with it
// when it is deleted.
func TestKubeKeepsTheDesksJobs(t *testing.T) {
	old := podWork
	podWork = t.TempDir()
	t.Cleanup(func() { podWork = old })
	w, err := NewWorld(Options{Builder: &Builder{Tools: []string{"go"}}})
	if err != nil {
		t.Fatal(err)
	}
	ask := func(verb string, names []string, stdin string) reply {
		t.Helper()
		return w.kube(request{Op: opKube, Verb: verb, Names: names, Stdin: stdin})
	}
	if rep := ask("get-job", []string{"bkb-1"}, ""); rep.Message != "" || rep.Exit != 0 {
		t.Fatalf("a Job that is not there: %+v", rep)
	}
	if rep := ask("exec", []string{"bkb-1-pod"}, ""); rep.SSH != nil || rep.Exit == 0 {
		t.Fatalf("exec into no pod: %+v", rep)
	}
	job := `{"metadata":{"name":"bkb-1","labels":{"app.kubernetes.io/name":"beekeeper-builder"}},"spec":{"template":{"spec":{"containers":[{"image":"i","env":[{"name":"GOCACHE","value":"/work/go-cache"}]}]}}}}`
	if rep := ask("create", nil, job); rep.Exit != 0 || rep.Error != "" {
		t.Fatalf("create: %+v", rep)
	}
	if rep := ask("create", nil, job); rep.Exit == 0 {
		t.Fatalf("a second Job of the name was created: %+v", rep)
	}
	if rep := ask("get-job", []string{"bkb-1"}, ""); rep.Message != job {
		t.Fatalf("the Job is not as the desk wrote it: %+v", rep)
	}
	if rep := ask("wait", []string{"bkb-1"}, ""); rep.Exit != 0 {
		t.Fatalf("wait: %+v", rep)
	}
	var pods struct {
		Items []struct {
			Metadata struct{ Name string }
			Status   struct{ Phase string }
		}
	}
	if err := json.Unmarshal([]byte(ask("get-pods", []string{"bkb-1"}, "").Message), &pods); err != nil || len(pods.Items) != 1 || pods.Items[0].Metadata.Name != "bkb-1-pod" || pods.Items[0].Status.Phase != "Running" {
		t.Fatalf("the Job's pod: %+v, %v", pods, err)
	}
	if err := json.Unmarshal([]byte(ask("get-pods", []string{"other"}, "").Message), &pods); err != nil || len(pods.Items) != 0 {
		t.Fatalf("another Job's pods: %+v, %v", pods, err)
	}
	rep := ask("exec", []string{"bkb-1-pod"}, "")
	if rep.SSH == nil {
		t.Fatalf("exec: %+v", rep)
	}
	for _, want := range []string{"PATH=" + filepath.Join(BuilderRoot, "bin"), "GOCACHE=/work/go-cache", builderEnv + "=1"} {
		if !slices.Contains(rep.SSH.Env, want) {
			t.Fatalf("the pod's environment %q lacks %s", rep.SSH.Env, want)
		}
	}
	// What a run leaves in the pod goes with the Job.
	if err := os.MkdirAll(filepath.Join(podWork, "tools", "v1"), 0o700); err != nil {
		t.Fatal(err)
	}
	if rep := ask("delete", []string{"bkb-1"}, ""); rep.Exit != 0 {
		t.Fatalf("delete: %+v", rep)
	}
	for _, gone := range []string{"tools", "runs", "active"} {
		if _, err := os.Stat(filepath.Join(podWork, gone)); !os.IsNotExist(err) {
			t.Fatalf("%s outlived its Job: %v", gone, err)
		}
	}
	if rep := ask("get-job", []string{"bkb-1"}, ""); rep.Message != "" {
		t.Fatalf("the deleted Job is still there: %+v", rep)
	}
	if rep := (&World{}).kube(request{Op: opKube, Verb: "get-pods"}); rep.Exit == 0 {
		t.Fatalf("a scenario without given.builder answered kubectl: %+v", rep)
	}
}
