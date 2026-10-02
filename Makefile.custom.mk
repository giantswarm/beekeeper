CONTROLLER_GEN_VERSION := v0.21.0
ENVTEST_VERSION := release-0.25
ENVTEST_K8S_VERSION := 1.37.0
GOIMPORTS_VERSION := v0.50.0

.PHONY: generate-crds
generate-crds: ## Regenerate the beekeeper.giantswarm.io deepcopy code and CRDs from pkg/apis (config/crd, and the chart's copy in helm/beekeeper/crds), imports as pre-commit's go-imports writes them.
	go run sigs.k8s.io/controller-tools/cmd/controller-gen@$(CONTROLLER_GEN_VERSION) \
		object paths=./pkg/apis/... crd:crdVersions=v1 output:crd:dir=./config/crd
	rm -f helm/beekeeper/crds/*.yaml && cp config/crd/*.yaml helm/beekeeper/crds/
	go run golang.org/x/tools/cmd/goimports@$(GOIMPORTS_VERSION) -w -local github.com/giantswarm/beekeeper pkg/apis

.PHONY: check-crds
check-crds: generate-crds ## Fail when the committed CRDs (config/crd, helm/beekeeper/crds) or deepcopy code are not what pkg/apis generates.
	git diff --exit-code -- config/crd helm/beekeeper/crds pkg/apis

.PHONY: test-envtest
test-envtest: ## Run the envtest-backed tests of the Kubernetes store and beekeeper serve, and the mailboxes' (downloads a kube-apiserver via setup-envtest; BEEKEEPER_TEST_DATABASE_URL names a Postgres).
	@test -n "$$BEEKEEPER_TEST_DATABASE_URL" || { echo "BEEKEEPER_TEST_DATABASE_URL is not set: a Postgres for the mailbox tests, see docs/development.md"; exit 1; }
	KUBEBUILDER_ASSETS="$$(go run sigs.k8s.io/controller-runtime/tools/setup-envtest@$(ENVTEST_VERSION) use $(ENVTEST_K8S_VERSION) -p path)" \
		go test ./internal/state/kube/ ./internal/mailbox/ ./cmd/ -run 'Envtest|Mailbox' -count=1 -v

HELM_UNITTEST_VERSION := 1.0.3

.PHONY: helm-test
helm-test: ## Lint the chart and run its helm-unittest suites (what CI's chart-test job runs).
	helm lint helm/beekeeper
	@helm plugin list | grep -q '^unittest' || helm plugin install https://github.com/helm-unittest/helm-unittest --version $(HELM_UNITTEST_VERSION)
	helm unittest helm/beekeeper
