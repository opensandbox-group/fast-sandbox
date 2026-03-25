# Default registry and images
REGISTRY ?= fast-sandbox
AGENT_IMAGE ?= $(REGISTRY)/agent:dev
CONTROLLER_IMAGE ?= $(REGISTRY)/controller:dev
JANITOR_IMAGE ?= $(REGISTRY)/janitor:dev

# Kind cluster name
KIND_CLUSTER_NAME ?= fast-sandbox

# E2E test settings
E2E_SUITE ?=
FAST_SANDBOX_E2E ?= 1
E2E_KEEP_CLUSTER ?= 0

# Go settings
GO ?= go
GOFLAGS ?= -gcflags="all=-N -l"

.PHONY: all build build-controller build-agent build-janitor build-agent-linux build-controller-linux build-janitor-linux test test-e2e setup-e2e tidy e2e docker-agent docker-controller docker-janitor kind-load-agent kind-load-controller kind-load-janitor help

all: build

help:
	@echo "Common targets:"
	@echo "  make build                  - build all binaries"
	@echo "  make build-agent-linux      - build agent binary for linux/amd64"
	@echo "  make build-controller-linux - build controller binary for linux/amd64"
	@echo "  make build-janitor-linux    - build janitor binary for linux/amd64"
	@echo "  make test                   - run unit tests"
	@echo "  make docker-agent           - build agent image"
	@echo "  make docker-controller      - build controller image"
	@echo "  make docker-janitor         - build janitor image"
	@echo ""
	@echo "E2E targets:"
	@echo "  make setup-e2e              - setup fresh kind cluster + deploy components"
	@echo "  make test-e2e               - full e2e: setup + run all tests"
	@echo "  make test-e2e-<suite>       - run specific suite (env must be ready)"
	@echo ""
	@echo "E2E settings:"
	@echo "  E2E_KEEP_CLUSTER=1          - keep cluster after test failure for debugging (default: 0)"
	@echo ""
	@echo "E2E test suites:"
	@echo "  basicvalidation, lifecycle, scheduling, cleanupjanitor"
	@echo "  advancedfeatures, cliintegration, faultrecovery"
	@echo ""
	@echo "E2E examples:"
	@echo "  # Full verification (fresh env + all tests)"
	@echo "  make test-e2e"
	@echo ""
	@echo "  # Keep cluster on failure for debugging"
	@echo "  E2E_KEEP_CLUSTER=1 make test-e2e"
	@echo ""
	@echo "  # Quick iteration (setup once, run multiple times)"
	@echo "  make setup-e2e"
	@echo "  make test-e2e-basicvalidation"
	@echo "  make test-e2e-lifecycle"

build: build-controller build-agent build-janitor build-fsb-ctl

build-controller:
	$(GO) build $(GOFLAGS) -o bin/controller ./cmd/controller

build-agent:
	$(GO) build $(GOFLAGS) -o bin/agent ./cmd/agent

build-janitor:
	$(GO) build $(GOFLAGS) -o bin/janitor ./cmd/janitor

build-fsb-ctl:
	$(GO) build $(GOFLAGS) -o bin/fsb-ctl ./cmd/fsb-ctl

# Cross-compile for linux/amd64 (for docker images)
build-agent-linux:
	@mkdir -p bin
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build $(GOFLAGS) -o bin/agent ./cmd/agent

build-controller-linux:
	@mkdir -p bin
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build $(GOFLAGS) -o bin/controller ./cmd/controller

build-janitor-linux:
	@mkdir -p bin
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build $(GOFLAGS) -o bin/janitor ./cmd/janitor

build-fsb-ctl-linux:
	@mkdir -p bin
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build $(GOFLAGS) -o bin/fsb-ctl ./cmd/fsb-ctl

test:
	$(GO) test $(GOFLAGS) ./...

tidy:
	$(GO) mod tidy

docker-agent: build-agent-linux
	docker build -t $(AGENT_IMAGE) -f build/Dockerfile.agent .

docker-controller: build-controller-linux
	docker build -t $(CONTROLLER_IMAGE) -f build/Dockerfile.controller .

docker-janitor: build-janitor-linux
	docker build -t $(JANITOR_IMAGE) -f build/Dockerfile.janitor .

kind-load-agent: docker-agent
	kind load docker-image $(AGENT_IMAGE) --name fast-sandbox

kind-load-controller: docker-controller
	kind load docker-image $(CONTROLLER_IMAGE) --name fast-sandbox

kind-load-janitor: docker-janitor
	kind load docker-image $(JANITOR_IMAGE) --name fast-sandbox

# E2E test - setup environment (fresh kind cluster with all components)
setup-e2e:
	@echo "=== Setting up E2E environment ==="
	@echo "Cluster name: $(KIND_CLUSTER_NAME)"
	@echo ""
	@if kind get clusters 2>/dev/null | grep -q "^$(KIND_CLUSTER_NAME)$$"; then \
		echo "Deleting existing kind cluster '$(KIND_CLUSTER_NAME)'..."; \
		kind delete cluster --name $(KIND_CLUSTER_NAME); \
	fi
	@echo "Creating fresh kind cluster and deploying components..."
	@FORCE_RECREATE_CLUSTER=true ./test/e2e/setup-kind.sh
	@echo ""
	@echo "=== E2E environment ready ==="
	@echo "Run tests with: make test-e2e-<suite>"

# E2E test - full test with fresh environment (setup + test)
test-e2e: setup-e2e
	@echo ""
	@echo "=== Running all E2E tests ==="
	@FAST_SANDBOX_E2E=1 FAST_SANDBOX_AGENT_IMAGE=$(AGENT_IMAGE) \
		$(GO) test ./test/e2e/suites/... -v -count=1 -failfast || \
		(if [ "$(E2E_KEEP_CLUSTER)" = "1" ]; then \
			echo ""; \
			echo "❌ E2E tests failed, keeping cluster for debugging..."; \
			echo "Run 'kind delete cluster --name $(KIND_CLUSTER_NAME)' when done debugging"; \
		else \
			echo ""; \
			echo "❌ E2E tests failed, cleaning up cluster..."; \
			kind delete cluster --name $(KIND_CLUSTER_NAME); \
		fi; \
		exit 1)
	@echo ""
	@echo "✅ All E2E tests passed"
	@if [ "$(E2E_KEEP_CLUSTER)" = "0" ]; then \
		echo "Cleaning up kind cluster..."; \
		kind delete cluster --name $(KIND_CLUSTER_NAME); \
	fi

# E2E test - run specific suite (assumes environment is already setup)
test-e2e-basicvalidation test-e2e-lifecycle test-e2e-scheduling test-e2e-cleanupjanitor test-e2e-advancedfeatures test-e2e-cliintegration test-e2e-faultrecovery:
	@echo "=== Running E2E test: $@ ==="
	@FAST_SANDBOX_E2E=1 FAST_SANDBOX_AGENT_IMAGE=$(AGENT_IMAGE) \
		$(GO) test ./test/e2e/suites/$(subst test-e2e-,,$@)/... -v -count=1