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
# The release tarball's sha256 per platform, from the release's helm-unittest-checksum.sha: pinned here,
# so installing the plugin fetches the tarball alone (with retries), never a checksum file at job time.
HELM_UNITTEST_SHA256_linux-amd64 := 9761f23d9509c98770c026e019e743b524b57010f4bc29175f78d2582ace0633
HELM_UNITTEST_SHA256_linux-arm64 := 1e645d96b36582cd8b9fbd53240110267f14d80aa01137341251c60438bbe6b0
HELM_UNITTEST_SHA256_macos-amd64 := 46413a86ded6bfc70cd704ebac16f8d4a0f36712ae399a5d24e32bc44f96985f
HELM_UNITTEST_SHA256_macos-arm64 := 6a6b67b3f638f015e09c093b67c7609a07101b971a1a6d6a83d1a7f75861a4b2
HELM_UNITTEST_PLATFORM := $(shell uname -s | sed 's/Linux/linux/;s/Darwin/macos/')-$(shell uname -m | sed 's/x86_64/amd64/;s/aarch64/arm64/')
HELM_UNITTEST_TGZ := helm-unittest-$(HELM_UNITTEST_PLATFORM)-$(HELM_UNITTEST_VERSION).tgz

.PHONY: helm-unittest-install
helm-unittest-install: ## Install the pinned helm-unittest plugin from its release tarball, verified against the checksum pinned above (no-op when installed).
	@helm plugin list | grep -q '^unittest' && exit 0; \
	sha="$(HELM_UNITTEST_SHA256_$(HELM_UNITTEST_PLATFORM))"; \
	test -n "$$sha" || { echo "helm-unittest: no pinned checksum for $(HELM_UNITTEST_PLATFORM)"; exit 1; }; \
	tmp="$$(mktemp -d)" && trap 'rm -rf "$$tmp"' EXIT; \
	curl -sSfL --retry 3 --retry-all-errors -o "$$tmp/$(HELM_UNITTEST_TGZ)" \
		"https://github.com/helm-unittest/helm-unittest/releases/download/v$(HELM_UNITTEST_VERSION)/$(HELM_UNITTEST_TGZ)" && \
	(cd "$$tmp" && echo "$$sha  $(HELM_UNITTEST_TGZ)" | shasum -a 256 -c -) && \
	dir="$$(helm env HELM_PLUGINS)/unittest" && mkdir -p "$$dir" && tar -xzf "$$tmp/$(HELM_UNITTEST_TGZ)" -C "$$dir" && \
	helm plugin list | grep -q '^unittest'

.PHONY: helm-test
helm-test: helm-unittest-install ## Lint the chart and run its helm-unittest suites (what CI's chart-test job runs).
	helm lint helm/beekeeper
	helm unittest helm/beekeeper
