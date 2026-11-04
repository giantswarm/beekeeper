package alerts

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/intstr"
	coreclient "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/transport/spdy"
)

// forward resolves the service to a ready pod and forwards its target port.
// client-go shares exec authenticators, including their expiring credentials,
// between the API client and SPDY transport and across reconnections.
func (r *Reader) forward(ctx context.Context, kubeContext string, ep endpoint) (*portForward, string) {
	if r.startForward != nil {
		return r.startForward(ctx, kubeContext, ep)
	}
	config, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(r.loadingRules(),
		&clientcmd.ConfigOverrides{CurrentContext: kubeContext}).ClientConfig()
	if err != nil {
		return nil, err.Error()
	}
	client, err := coreclient.NewForConfig(config)
	if err != nil {
		return nil, err.Error()
	}
	setup, cancelSetup := context.WithTimeout(ctx, forwardWithin)
	defer cancelSetup()
	pod, port, err := servicePod(setup, client, ep)
	if err != nil {
		return nil, err.Error()
	}
	transport, upgrader, err := spdy.RoundTripperFor(config)
	if err != nil {
		return nil, err.Error()
	}
	url := client.RESTClient().Post().Namespace(ep.namespace).Resource("pods").Name(pod).SubResource("portforward").URL()
	dialer := spdy.NewDialer(upgrader, &http.Client{Transport: transport}, http.MethodPost, url)
	life := ctx
	if r.Keep {
		life = context.WithoutCancel(ctx)
	}
	life, cancel := context.WithCancel(life)
	ready := make(chan struct{})
	pf, err := portforward.NewOnAddresses(dialer, []string{"127.0.0.1"},
		[]string{":" + strconv.Itoa(port)}, life.Done(), ready, io.Discard, io.Discard)
	if err != nil {
		cancel()
		return nil, err.Error()
	}
	f := &portForward{cancel: cancel, ep: ep, exited: make(chan struct{})}
	result := make(chan error, 1)
	go func() {
		result <- pf.ForwardPorts()
		close(f.exited)
	}()
	select {
	case <-ready:
		ports, err := pf.GetPorts()
		if err != nil || len(ports) != 1 {
			f.close()
			return nil, fmt.Sprintf("get forwarded port: %v", err)
		}
		f.port = int(ports[0].Local)
		return f, ""
	case err := <-result:
		f.close()
		return nil, fmt.Sprintf("port-forward ended: %v", err)
	case <-setup.Done():
		f.close()
		return nil, "timed out"
	}
}

// servicePod uses the service port, which can differ from the pod's port.
func servicePod(ctx context.Context, client coreclient.CoreV1Interface, ep endpoint) (string, int, error) {
	svc, err := client.Services(ep.namespace).Get(ctx, ep.service, metav1.GetOptions{})
	if err != nil {
		return "", 0, err
	}
	if len(svc.Spec.Selector) == 0 {
		return "", 0, fmt.Errorf("service %s has no selector", ep.service)
	}
	var servicePort *corev1.ServicePort
	for i := range svc.Spec.Ports {
		p := &svc.Spec.Ports[i]
		if int(p.Port) == ep.port && (p.Protocol == "" || p.Protocol == corev1.ProtocolTCP) {
			servicePort = p
			break
		}
	}
	if servicePort == nil {
		return "", 0, fmt.Errorf("service %s has no TCP port %d", ep.service, ep.port)
	}
	pods, err := client.Pods(ep.namespace).List(ctx, metav1.ListOptions{LabelSelector: labels.SelectorFromSet(svc.Spec.Selector).String()})
	if err != nil {
		return "", 0, err
	}
	for _, pod := range pods.Items {
		if pod.DeletionTimestamp != nil || pod.Status.Phase != corev1.PodRunning {
			continue
		}
		ready := false
		for _, c := range pod.Status.Conditions {
			if c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue {
				ready = true
			}
		}
		if !ready {
			continue
		}
		target := servicePort.TargetPort
		if target.Type == intstr.Int {
			port := int(target.IntVal)
			if port == 0 {
				port = int(servicePort.Port)
			}
			return pod.Name, port, nil
		}
		for _, c := range pod.Spec.Containers {
			for _, p := range c.Ports {
				if p.Name == target.StrVal && (p.Protocol == "" || p.Protocol == corev1.ProtocolTCP) {
					return pod.Name, int(p.ContainerPort), nil
				}
			}
		}
	}
	return "", 0, fmt.Errorf("service %s has no ready pod with its target port", ep.service)
}
