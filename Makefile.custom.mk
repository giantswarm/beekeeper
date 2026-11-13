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
test-envtest: ## Run the envtest-backed tests of the Kubernetes store and beekeeper serve, and optional mailbox tests (set BEEKEEPER_TEST_DATABASE_URL for Postgres).
	assets="$$(go run sigs.k8s.io/controller-runtime/tools/setup-envtest@$(ENVTEST_VERSION) use $(ENVTEST_K8S_VERSION) -p path)" && \
		KUBEBUILDER_ASSETS="$$assets" go test ./internal/state/kube/ ./internal/mailbox/ ./cmd/ -run 'Envtest|Mailbox' -count=1 -v

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
	@have=$$(helm plugin list 2>/dev/null | awk '$$1 == "unittest" {print $$2}'); \
	[ "$$have" = "$(HELM_UNITTEST_VERSION)" ] && exit 0; \
	[ -z "$$have" ] || helm plugin uninstall unittest || exit 1; \
	sha="$(HELM_UNITTEST_SHA256_$(HELM_UNITTEST_PLATFORM))"; \
	test -n "$$sha" || { echo "helm-unittest: no pinned checksum for $(HELM_UNITTEST_PLATFORM)"; exit 1; }; \
	tmp="$$(mktemp -d)" && trap 'rm -rf "$$tmp"' EXIT; \
	curl -sSfL --retry 3 --retry-all-errors -o "$$tmp/$(HELM_UNITTEST_TGZ)" \
		"https://github.com/helm-unittest/helm-unittest/releases/download/v$(HELM_UNITTEST_VERSION)/$(HELM_UNITTEST_TGZ)" && \
	(cd "$$tmp" && echo "$$sha  $(HELM_UNITTEST_TGZ)" | \
		if command -v sha256sum >/dev/null 2>&1; then sha256sum -c -; else shasum -a 256 -c -; fi) && \
	dir="$$(helm env HELM_PLUGINS)/unittest" && mkdir -p "$$dir" && tar -xzf "$$tmp/$(HELM_UNITTEST_TGZ)" -C "$$dir" && \
	helm plugin list | grep -q '^unittest'

# The generated helm-test target also requires install-helm-unittest.
# Run our pinned installer first, so that target sees the exact version
# already installed and never runs the upstream hook/checksum downloader.
install-helm-unittest: helm-unittest-install

.PHONY: helm-test
helm-test: helm-unittest-install ## Lint the chart and run its helm-unittest suites (what CI's chart-test job runs).
	helm lint helm/beekeeper
	helm unittest helm/beekeeper

.PHONY: tools-doc
tools-doc: ## Regenerate docs/tools.md, every tool's description and input schema as a model reads them, from the registered tools.
	TOOLS_DOC_UPDATE=1 go test -count=1 -run TestToolsDocIsCurrent ./cmd/

.PHONY: test-scenarios
test-scenarios: ## Run the behavioral scenarios (docs/scenarios.md) against the real binary, each in its own namespace sandbox, the tests of the scenario clock, of the fake harnesses and of the sandbox. Set BEEKEEPER_SCENARIO_FLOW, _TAG, _SHARD or _REPORT.
	go test -tags scenario ./internal/clock ./internal/scenario/... ./internal/testenv -count=1 -timeout=15m -v

.PHONY: test-gh-contract
test-gh-contract: ## On demand: compare the scenarios' fake gh with the real gh on PATH against the same fake GitHub (docs/scenarios-github.md).
	BEEKEEPER_GH_CONTRACT=1 go test -tags scenario ./internal/scenario/fakegithub -run '^TestContractWithRealGH$$' -count=1 -timeout=5m -v

.PHONY: install-cosign-retry
install-cosign-retry: ## Retry Rekor's conflict lookup race in the later steps of this CircleCI job.
	bash .circleci/install-cosign-retry.sh

.PHONY: warm-cmd-test
warm-cmd-test: ## Compile ./cmd's test binary before go test ./... buffers output behind it, keeping go-build within CircleCI's 10m silence limit.
	go test -ldflags "$(LDFLAGS)" $(RACE) -run '^$$' ./cmd

# The generated go-build job runs make test before it signs the binaries, and
# align owns that job: this is where the repository can reach its later steps.
ifeq ($(CIRCLE_JOB),go-build)
test: install-cosign-retry warm-cmd-test
endif

.PHONY: browse-upload-live
browse-upload-live: ## Check browse --file against the installed Claude CLI and connected Chrome.
	go test -tags browselive -run '^TestBrowseUploadLive$$' -count=1 -timeout=10m ./cmd

PERF_TEST := go test -tags perf ./internal/perfsuite -count=1 -timeout=40m -v

.PHONY: test-performance
test-performance: ## Run the performance budgets (docs/performance.md). CI runs it; outside CI it refuses to run without a cgroup limit.
	BEEKEEPER_PERF=1 $(PERF_TEST)

.PHONY: test-performance-local
test-performance-local: ## Opt-in local run of the performance budgets, inside a cgroup limited to one core and 1 GiB. Never on a desk's coordinator.
	systemd-run --user --scope --quiet -p CPUQuota=100% -p MemoryMax=1G -- env BEEKEEPER_PERF=1 $(PERF_TEST)
