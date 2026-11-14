// Package rbactest tests the chart's RBAC (helm/beekeeper) against a real kube-apiserver with
// RBAC authorization (envtest): the engine's ServiceAccount may do exactly what the engine needs
// in its namespace and nothing anywhere else, the receiver's may do nothing at all.
//
// The tests render the chart with helm and start envtest's kube-apiserver and etcd from
// KUBEBUILDER_ASSETS, else from the test-deps cache (make test-deps). Without helm or those
// binaries they skip, unless CI is set: then they fail.
package rbactest
