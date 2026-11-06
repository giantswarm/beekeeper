package alerts

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	kubefake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"k8s.io/client-go/transport/spdy"
)

const forwardTestApp = "alerts"

func TestServicePod(t *testing.T) {
	for _, tt := range []struct {
		name   string
		target intstr.IntOrString
		want   int
	}{
		{"numeric", intstr.FromInt32(9094), 9094},
		{"named", intstr.FromString("web"), 9095},
		{"default", intstr.FromInt32(0), 9093},
	} {
		t.Run(tt.name, func(t *testing.T) {
			svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: plain.service, Namespace: plain.namespace},
				Spec: corev1.ServiceSpec{Selector: map[string]string{"app": forwardTestApp}, Ports: []corev1.ServicePort{{Port: 9093, TargetPort: tt.target}}}}
			pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "ready", Namespace: plain.namespace, Labels: svc.Spec.Selector},
				Spec:   corev1.PodSpec{Containers: []corev1.Container{{Name: forwardTestApp, Ports: []corev1.ContainerPort{{Name: "web", ContainerPort: 9095}}}}},
				Status: corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}}}
			unready := pod.DeepCopy()
			unready.Name = "a-unready"
			unready.Status.Conditions = nil
			unrelated := pod.DeepCopy()
			unrelated.Name = "a-unrelated"
			unrelated.Labels = map[string]string{"app": "other"}
			client := kubefake.NewClientset(svc, pod, unready, unrelated)
			name, port, err := servicePod(context.Background(), client.CoreV1(), plain)
			if err != nil || name != "ready" || port != tt.want {
				t.Fatalf("got %s:%d, %v", name, port, err)
			}
			if err := client.CoreV1().Pods(plain.namespace).Delete(context.Background(), "ready", metav1.DeleteOptions{}); err != nil {
				t.Fatal(err)
			}
			if _, _, err := servicePod(context.Background(), client.CoreV1(), plain); err == nil {
				t.Fatal("accepted unready or unrelated pod")
			}
		})
	}
}

func TestServicePodRejectsMissingSelectorAndPort(t *testing.T) {
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: plain.service, Namespace: plain.namespace}}
	client := kubefake.NewClientset(svc)
	if _, _, err := servicePod(context.Background(), client.CoreV1(), plain); err == nil {
		t.Fatal("accepted missing selector")
	}
	svc.Spec.Selector = map[string]string{"app": forwardTestApp}
	if _, err := client.CoreV1().Services(plain.namespace).Update(context.Background(), svc, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := servicePod(context.Background(), client.CoreV1(), plain); err == nil {
		t.Fatal("accepted missing service port")
	}
}

// Separate API and SPDY transports, including a replacement forward, must
// share the credential until the exec plugin's advertised expiration.
func TestForwardTransportsShareExecCredential(t *testing.T) {
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	calls := filepath.Join(dir, "calls")
	plugin := filepath.Join(dir, "credentials")
	script := "#!/bin/sh\necho called >> '" + calls + "'\necho '{\"apiVersion\":\"client.authentication.k8s.io/v1\",\"kind\":\"ExecCredential\",\"status\":{\"token\":\"test-token\",\"expirationTimestamp\":\"2099-01-01T00:00:00Z\"}}'\n"
	if err := root.WriteFile("credentials", []byte(script), 0600); err != nil {
		t.Fatal(err)
	}
	if err := root.Chmod("credentials", 0700); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-token" {
			t.Error("missing exec credential")
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	config := &rest.Config{Host: server.URL, TLSClientConfig: rest.TLSClientConfig{Insecure: true},
		ExecProvider: &clientcmdapi.ExecConfig{Command: plugin, APIVersion: "client.authentication.k8s.io/v1", InteractiveMode: clientcmdapi.NeverExecInteractiveMode}}
	for range 2 {
		api, err := rest.TransportFor(config)
		if err != nil {
			t.Fatal(err)
		}
		forward, _, err := spdy.RoundTripperFor(config)
		if err != nil {
			t.Fatal(err)
		}
		for _, transport := range []http.RoundTripper{api, forward} {
			req, err := http.NewRequest(http.MethodGet, server.URL, nil)
			if err != nil {
				t.Fatal(err)
			}
			resp, err := transport.RoundTrip(req)
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
		}
	}
	f, err := root.Open("calls")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	raw, err := io.ReadAll(f)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "called\n" {
		t.Fatalf("credential executions: %q", raw)
	}
}
