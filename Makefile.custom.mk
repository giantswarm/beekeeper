##@ Beekeeper

# The test binaries. hack/test-deps.sh fetches each into BEEKEEPER_TEST_DEPS
# if it is missing, checked against its published checksum.
# renovate: datasource=github-releases depName=k3s-io/kine
KINE_VERSION := 0.14.12
# k8senv's API server.
# renovate: datasource=github-releases depName=kubernetes/kubernetes
KUBE_APISERVER_VERSION := 1.37.2
# setup-envtest's assets for the RBAC tests.
ENVTEST_K8S_VERSION := 1.37.0
# embedded-postgres's Postgres 18 (its V18).
POSTGRES_VERSION := 18.3.0
# promtool, for the alert rules' tests.
# renovate: datasource=github-releases depName=prometheus/prometheus
PROMETHEUS_VERSION := 3.16.0
CONTROLLER_GEN_VERSION := v0.21.0

BEEKEEPER_TEST_DEPS ?= $(or $(XDG_CACHE_HOME),$(HOME)/.cache)/beekeeper/test-deps
ENVTEST_ASSETS = $(BEEKEEPER_TEST_DEPS)/envtest/k8s/$(ENVTEST_K8S_VERSION)-$(shell go env GOOS)-$(shell go env GOARCH)
PROMTOOL = $(BEEKEEPER_TEST_DEPS)/prometheus-$(PROMETHEUS_VERSION)/promtool

export BEEKEEPER_TEST_DEPS KINE_VERSION KUBE_APISERVER_VERSION ENVTEST_K8S_VERSION POSTGRES_VERSION PROMETHEUS_VERSION CONTROLLER_GEN_VERSION

.PHONY: test-deps
test-deps: ## Fetch the test binaries (kine, kube-apiserver, envtest, the embedded Postgres) into BEEKEEPER_TEST_DEPS, each if missing.
	hack/test-deps.sh kine kube-apiserver envtest postgres

.PHONY: test-deps-versions
test-deps-versions: ## Print the test binaries' versions (CI keys its cache of BEEKEEPER_TEST_DEPS on them).
	@echo "kine $(KINE_VERSION) kube-apiserver $(KUBE_APISERVER_VERSION) envtest $(ENVTEST_K8S_VERSION) postgres $(POSTGRES_VERSION) prometheus $(PROMETHEUS_VERSION)"

# `make test` is generated (Makefile.gen.go.mk: go test ./..., with -race where
# a C toolchain exists; Makefile.gen.app.mk adds helm-test). It is what CI's
# go-build job runs. The unit tests, the scenarios and the RBAC tests need
# their binaries first.
test: test-deps

# go-build runs `make test` as root, where Postgres' initdb refuses to run:
# there each test binary runs as an unprivileged user (hack/run-unprivileged.sh).
ifeq ($(shell id -u),0)
test: export GOFLAGS := $(strip $(GOFLAGS) -exec=$(CURDIR)/hack/run-unprivileged.sh)
test: test-unprivileged
endif

.PHONY: test-unprivileged
test-unprivileged: test-deps
	hack/run-unprivileged.sh --setup

.PHONY: test-race
test-race: test-deps ## Run every test, the scenarios included, with the race detector (needs a C toolchain): CI's test-loop-workflow.
	CGO_ENABLED=1 go test -race -count=1 -timeout=20m ./...

.PHONY: test-envtest
test-envtest: test-deps ## Run the RBAC tests: the chart's RBAC against envtest's kube-apiserver.
	KUBEBUILDER_ASSETS="$(ENVTEST_ASSETS)" go test -count=1 -v ./internal/rbactest/...

.PHONY: test-large-history
test-large-history: test-deps ## Replay a generated history of 10,000 decisions.
	go test -count=1 -timeout=20m -v -run '^TestLargeHistory$$' ./internal/replay

.PHONY: test-alerts
test-alerts: ## Test every alert rule of the chart with promtool (fetches promtool into BEEKEEPER_TEST_DEPS if missing).
	hack/test-deps.sh promtool
	PROMTOOL="$(PROMTOOL)" hack/test-alerts.sh

.PHONY: helm-template-prod
helm-template-prod: ## Lint and render the chart with its defaults and with production-like values (helm/beekeeper/ci/ci-values.yaml).
	helm lint --strict helm/beekeeper
	helm lint --strict helm/beekeeper --values helm/beekeeper/ci/ci-values.yaml
	helm template beekeeper helm/beekeeper --namespace beekeeper >/dev/null
	helm template beekeeper helm/beekeeper --namespace beekeeper --values helm/beekeeper/ci/ci-values.yaml

.PHONY: generate
generate: ## Regenerate the deepcopy code and the CRDs (config/crd) from api/, and copy the CRDs into the chart.
	hack/crds.sh generate

.PHONY: check-crds
check-crds: ## Fail when the deepcopy code, config/crd or the chart's CRDs are not what api/ generates.
	hack/crds.sh check

.PHONY: chart-docs
chart-docs: ## Regenerate the chart's values.schema.json and README.md from values.yaml, as pre-commit checks them.
	hack/chart-docs.sh

.PHONY: catalogue
catalogue: ## Lint the scenarios and write their catalogue (internal/testing/scenarios/CATALOGUE.md).
	go run ./internal/testing/scenariolint internal/testing/scenarios
