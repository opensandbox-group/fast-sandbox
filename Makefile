# Default registry and images
REGISTRY ?= fast-sandbox
FASTLET_IMAGE ?= $(REGISTRY)/fastlet:dev
FASTLET_PROXY_IMAGE ?= $(REGISTRY)/fastlet-proxy:dev
SANDBOX_PROXY_IMAGE ?= $(REGISTRY)/sandbox-proxy:dev
CONTROLLER_IMAGE ?= $(REGISTRY)/controller:dev
JANITOR_IMAGE ?= $(REGISTRY)/janitor:dev

# Kind cluster name
KIND_CLUSTER_NAME ?= fast-sandbox

# E2E test settings
E2E_PROFILE ?= basic
E2E_TEST_TIMEOUT ?= 30m
UNIT_PACKAGES := ./api/... ./cmd/... ./internal/... ./pkg/... ./test/e2e/env/... ./test/e2e/support/...

# Go settings
GO ?= go
GOFLAGS ?= -gcflags="all=-N -l"
DOCKER_BUILD_FLAGS ?=

# Pinned code-generation tools. They are installed under the repository-local
# .tools directory so generation does not depend on developer machine state.
TOOLS_DIR := $(CURDIR)/.tools
TOOLS_BIN := $(TOOLS_DIR)/bin
PROTOC_VERSION := 29.3
PROTOC_SHA256_LINUX_X86_64 := 3e866620c5be27664f3d2fa2d656b5f3e09b5152b42f1bedbf427b333e90021a
PROTOC_ROOT := $(TOOLS_DIR)/protoc-$(PROTOC_VERSION)
PROTOC := $(PROTOC_ROOT)/bin/protoc
PROTOC_GEN_GO_VERSION := v1.36.11
PROTOC_GEN_GO_GRPC_VERSION := v1.6.0
CONTROLLER_GEN_VERSION := v0.20.1
CONTROLLER_GEN := $(TOOLS_BIN)/controller-gen

.PHONY: all build build-controller build-fastlet build-sandbox-init build-fastlet-proxy build-sandbox-proxy build-janitor build-fastlet-linux build-sandbox-init-linux build-fastlet-proxy-linux build-sandbox-proxy-linux build-controller-linux build-janitor-linux build-fastctl build-fastctl-linux tools generate generate-proto generate-python-proto generate-deepcopy manifests verify-generated test test-unit test-python-sdk test-race test-network-integration test-e2e test-e2e-controlplane test-e2e-network test-e2e-proxy test-e2e-infra test-e2e-sdk test-e2e-runtime test-e2e-drain verify setup-e2e tidy e2e docker-fastlet docker-fastlet-proxy docker-sandbox-proxy docker-controller docker-janitor kind-load-fastlet kind-load-fastlet-proxy kind-load-sandbox-proxy kind-load-controller kind-load-janitor help

all: build

help:
	@echo "Common targets:"
	@echo "  make build                  - build all binaries"
	@echo "  make build-fastlet-linux    - build fastlet binary for linux/amd64"
	@echo "  make build-controller-linux - build controller binary for linux/amd64"
	@echo "  make build-janitor-linux    - build janitor binary for linux/amd64"
	@echo "  make test                   - run unit tests (alias of test-unit)"
	@echo "  make test-unit              - run tests that require no live runtime"
	@echo "  make test-race              - run unit tests with the race detector"
	@echo "  make test-network-integration - run privileged Linux netns/veth/NAT validation in Docker"
	@echo "  make test-e2e-proxy         - run the focused Sandbox/Fastlet Proxy data-plane e2e"
	@echo "  make test-e2e-infra         - run the focused Infra runtime-augmentation e2e"
	@echo "  make test-e2e-sdk           - run the focused SDK Adapter data-plane e2e"
	@echo "  make test-e2e-drain         - run the focused durable Pool Drain e2e"
	@echo "  make test-python-sdk        - run Python SDK unit tests in the active Python environment"
	@echo "  make generate               - regenerate protobuf and deepcopy code"
	@echo "  make manifests              - regenerate CRD manifests"
	@echo "  make verify-generated       - fail if generated files are stale"
	@echo "  make verify                 - run generated-file and unit-test gates"
	@echo "  make docker-fastlet         - build fastlet image"
	@echo "  make docker-controller      - build controller image"
	@echo "  make docker-janitor         - build janitor image"
	@echo ""
	@echo "E2E targets:"
	@echo "  make setup-e2e              - prepare one e2e profile, default E2E_PROFILE=basic"
	@echo "  make test-e2e               - run all e2e suites; tests prepare their own profiles"
	@echo "  make test-e2e-<suite>       - run a specific suite"
	@echo ""
	@echo "E2E settings:"
	@echo "  E2E_PROFILE=basic|gvisor|kata-qemu|kata-clh|kata-fc"
	@echo "  E2E_TEST_TIMEOUT=30m"
	@echo ""
	@echo "E2E test suites:"
	@echo "  basicvalidation, lifecycle, scheduling, cleanupjanitor"
	@echo "  advancedfeatures, cliintegration, faultrecovery, secureruntime"
	@echo ""
	@echo "E2E examples:"
	@echo "  # Full verification"
	@echo "  make test-e2e"
	@echo ""
	@echo "  # Prepare one profile explicitly"
	@echo "  E2E_PROFILE=kata-qemu make setup-e2e"
	@echo ""
	@echo "  # Quick iteration"
	@echo "  make test-e2e-basicvalidation"
	@echo "  make test-e2e-lifecycle"

build: build-controller build-fastlet build-sandbox-init build-fastlet-proxy build-sandbox-proxy build-janitor build-fastctl

build-controller:
	$(GO) build $(GOFLAGS) -o bin/controller ./cmd/controller

build-fastlet:
	$(GO) build $(GOFLAGS) -o bin/fastlet ./cmd/fastlet

build-sandbox-init:
	$(GO) build $(GOFLAGS) -o bin/sandbox-init ./cmd/sandbox-init

build-fastlet-proxy:
	$(GO) build $(GOFLAGS) -o bin/fastlet-proxy ./cmd/fastlet-proxy

build-sandbox-proxy:
	$(GO) build $(GOFLAGS) -o bin/sandbox-proxy ./cmd/sandbox-proxy

build-janitor:
	$(GO) build $(GOFLAGS) -o bin/janitor ./cmd/janitor

build-fastctl:
	$(GO) build $(GOFLAGS) -o bin/fastctl ./cmd/fastctl

# Cross-compile for linux/amd64 (for docker images)
build-fastlet-linux:
	@mkdir -p bin
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build $(GOFLAGS) -o bin/fastlet ./cmd/fastlet

build-sandbox-init-linux:
	@mkdir -p bin
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build $(GOFLAGS) -o bin/sandbox-init ./cmd/sandbox-init

build-fastlet-proxy-linux:
	@mkdir -p bin
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build $(GOFLAGS) -o bin/fastlet-proxy ./cmd/fastlet-proxy

build-sandbox-proxy-linux:
	@mkdir -p bin
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build $(GOFLAGS) -o bin/sandbox-proxy ./cmd/sandbox-proxy

build-controller-linux:
	@mkdir -p bin
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build $(GOFLAGS) -o bin/controller ./cmd/controller

build-janitor-linux:
	@mkdir -p bin
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build $(GOFLAGS) -o bin/janitor ./cmd/janitor

build-fastctl-linux:
	@mkdir -p bin
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build $(GOFLAGS) -o bin/fastctl ./cmd/fastctl

test: test-unit

test-unit:
	$(GO) test $(GOFLAGS) $(UNIT_PACKAGES)

test-python-sdk:
	PYTHONPATH=sdk/python python3 -m unittest discover -s sdk/python/tests -v

test-race:
	$(GO) test -race $(UNIT_PACKAGES)

test-network-integration: docker-fastlet
	CGO_ENABLED=0 $(GO) test -c -o bin/network.test ./internal/fastlet/network
	docker build $(DOCKER_BUILD_FLAGS) \
		--build-arg FASTLET_IMAGE=$(FASTLET_IMAGE) \
		-t fast-sandbox/network-test:dev -f build/Dockerfile.network-test .
	docker run --rm --privileged \
		-e FAST_SANDBOX_RUN_PRIVILEGED_NETWORK_TEST=1 \
		fast-sandbox/network-test:dev \
		-test.run '^TestLinuxNetNSDriverPrivileged$$' -test.v

tools: $(PROTOC) $(TOOLS_BIN)/protoc-gen-go $(TOOLS_BIN)/protoc-gen-go-grpc $(CONTROLLER_GEN)

$(PROTOC):
	@mkdir -p $(TOOLS_DIR) $(PROTOC_ROOT)
	@curl -fsSL https://github.com/protocolbuffers/protobuf/releases/download/v$(PROTOC_VERSION)/protoc-$(PROTOC_VERSION)-linux-x86_64.zip -o $(TOOLS_DIR)/protoc-$(PROTOC_VERSION)-linux-x86_64.zip
	@echo "$(PROTOC_SHA256_LINUX_X86_64)  $(TOOLS_DIR)/protoc-$(PROTOC_VERSION)-linux-x86_64.zip" | sha256sum -c -
	@if command -v unzip >/dev/null 2>&1; then \
		unzip -q -o $(TOOLS_DIR)/protoc-$(PROTOC_VERSION)-linux-x86_64.zip -d $(PROTOC_ROOT); \
	elif command -v busybox >/dev/null 2>&1; then \
		busybox unzip -q -o $(TOOLS_DIR)/protoc-$(PROTOC_VERSION)-linux-x86_64.zip -d $(PROTOC_ROOT); \
	else \
		python3 -m zipfile -e $(TOOLS_DIR)/protoc-$(PROTOC_VERSION)-linux-x86_64.zip $(PROTOC_ROOT); \
	fi

$(TOOLS_BIN)/protoc-gen-go:
	@mkdir -p $(TOOLS_BIN)
	@GOBIN=$(TOOLS_BIN) $(GO) install google.golang.org/protobuf/cmd/protoc-gen-go@$(PROTOC_GEN_GO_VERSION)

$(TOOLS_BIN)/protoc-gen-go-grpc:
	@mkdir -p $(TOOLS_BIN)
	@GOBIN=$(TOOLS_BIN) $(GO) install google.golang.org/grpc/cmd/protoc-gen-go-grpc@$(PROTOC_GEN_GO_GRPC_VERSION)

$(CONTROLLER_GEN):
	@mkdir -p $(TOOLS_BIN)
	@GOBIN=$(TOOLS_BIN) $(GO) install sigs.k8s.io/controller-tools/cmd/controller-gen@$(CONTROLLER_GEN_VERSION)

generate: generate-proto generate-deepcopy

generate-python-proto:
	bash sdk/python/generate_proto.sh

generate-proto: tools
	@PATH=$(TOOLS_BIN):$$PATH $(PROTOC) -I . --go_out=. --go_opt=paths=source_relative --go-grpc_out=. --go-grpc_opt=paths=source_relative api/proto/v1/fastpath.proto

generate-deepcopy: tools
	@$(CONTROLLER_GEN) object paths=./api/v1alpha1/...

manifests: tools
	@$(CONTROLLER_GEN) crd paths=./api/v1alpha1/... output:crd:artifacts:config=config/crd

verify-generated: generate manifests
	@git diff --exit-code -- api/proto/v1 api/v1alpha1/zz_generated.deepcopy.go config/crd

verify: verify-generated test-unit

tidy:
	$(GO) mod tidy

e2e: test-e2e

docker-fastlet: build-fastlet-linux build-sandbox-init-linux
	docker build $(DOCKER_BUILD_FLAGS) -t $(FASTLET_IMAGE) -f build/Dockerfile.fastlet .

docker-fastlet-proxy: build-fastlet-proxy-linux
	docker build $(DOCKER_BUILD_FLAGS) -t $(FASTLET_PROXY_IMAGE) -f build/Dockerfile.fastlet-proxy .

docker-sandbox-proxy: build-sandbox-proxy-linux
	docker build $(DOCKER_BUILD_FLAGS) -t $(SANDBOX_PROXY_IMAGE) -f build/Dockerfile.sandbox-proxy .

docker-controller: build-controller-linux
	docker build -t $(CONTROLLER_IMAGE) -f build/Dockerfile.controller .

docker-janitor: build-janitor-linux
	docker build -t $(JANITOR_IMAGE) -f build/Dockerfile.janitor .

kind-load-fastlet: docker-fastlet
	kind load docker-image $(FASTLET_IMAGE) --name fast-sandbox

kind-load-fastlet-proxy: docker-fastlet-proxy
	kind load docker-image $(FASTLET_PROXY_IMAGE) --name fast-sandbox

kind-load-sandbox-proxy: docker-sandbox-proxy
	kind load docker-image $(SANDBOX_PROXY_IMAGE) --name fast-sandbox

kind-load-controller: docker-controller
	kind load docker-image $(CONTROLLER_IMAGE) --name fast-sandbox

kind-load-janitor: docker-janitor
	kind load docker-image $(JANITOR_IMAGE) --name fast-sandbox

# E2E test - prepare one environment profile
setup-e2e:
	@echo "=== Preparing E2E environment ==="
	@echo "Profile: $(E2E_PROFILE)"
	@FAST_SANDBOX_FASTLET_IMAGE=$(FASTLET_IMAGE) \
		$(GO) run ./test/e2e/env/cmd/setup -profile $(E2E_PROFILE) -timeout $(E2E_TEST_TIMEOUT)
	@echo ""
	@echo "=== E2E environment ready ==="
	@echo "Run tests with: make test-e2e-<suite> or go test ./test/e2e/suites/<suite>"

# E2E test - full test. Each test prepares the profile it needs.
test-e2e:
	@echo ""
	@echo "=== Running all E2E tests ==="
	@FAST_SANDBOX_FASTLET_IMAGE=$(FASTLET_IMAGE) \
		$(GO) test -p 1 ./test/e2e/suites/... -v -count=1 -failfast -timeout $(E2E_TEST_TIMEOUT)
	@echo ""
	@echo "All E2E tests passed"

test-e2e-controlplane:
	@FAST_SANDBOX_FASTLET_IMAGE=$(FASTLET_IMAGE) \
		$(GO) test -p 1 ./test/e2e/suites/controlplane/... ./test/e2e/suites/basicvalidation/... ./test/e2e/suites/lifecycle/... ./test/e2e/suites/scheduling/... ./test/e2e/suites/cleanupjanitor/... -v -count=1 -failfast -timeout $(E2E_TEST_TIMEOUT)

test-e2e-network:
	@FAST_SANDBOX_FASTLET_IMAGE=$(FASTLET_IMAGE) \
		$(GO) test -p 1 ./test/e2e/suites/basicvalidation/... -v -count=1 -failfast -timeout $(E2E_TEST_TIMEOUT)

test-e2e-proxy:
	@FAST_SANDBOX_FASTLET_IMAGE=$(FASTLET_IMAGE) \
		$(GO) test ./test/e2e/suites/basicvalidation/... -run '^TestSandboxProxyDataPlane$$' -v -count=1 -timeout $(E2E_TEST_TIMEOUT)

test-e2e-infra:
	@FAST_SANDBOX_FASTLET_IMAGE=$(FASTLET_IMAGE) \
		$(GO) test ./test/e2e/suites/basicvalidation/... -run '^TestInfraRuntimeAugmentation$$' -v -count=1 -timeout $(E2E_TEST_TIMEOUT)

test-e2e-sdk:
	@FAST_SANDBOX_FASTLET_IMAGE=$(FASTLET_IMAGE) \
		$(GO) test ./test/e2e/suites/basicvalidation/... -run '^TestSDKAdapterDataPlane$$' -v -count=1 -timeout $(E2E_TEST_TIMEOUT)

test-e2e-runtime:
	@FAST_SANDBOX_FASTLET_IMAGE=$(FASTLET_IMAGE) \
		$(GO) test -p 1 ./test/e2e/suites/secureruntime/... -v -count=1 -failfast -timeout $(E2E_TEST_TIMEOUT)

test-e2e-drain:
	@FAST_SANDBOX_FASTLET_IMAGE=$(FASTLET_IMAGE) \
		$(GO) test ./test/e2e/suites/drain/... -v -count=1 -timeout $(E2E_TEST_TIMEOUT)

# E2E test - run specific suite
test-e2e-basicvalidation test-e2e-lifecycle test-e2e-scheduling test-e2e-cleanupjanitor test-e2e-advancedfeatures test-e2e-cliintegration test-e2e-faultrecovery test-e2e-secureruntime:
	@echo "=== Running E2E test: $@ ==="
	@FAST_SANDBOX_FASTLET_IMAGE=$(FASTLET_IMAGE) \
		$(GO) test ./test/e2e/suites/$(subst test-e2e-,,$@)/... -v -count=1 -timeout $(E2E_TEST_TIMEOUT)
