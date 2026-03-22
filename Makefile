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

# Go settings
GO ?= go
GOFLAGS ?= -gcflags="all=-N -l"

.PHONY: all build build-controller build-agent build-janitor build-agent-linux build-controller-linux build-janitor-linux test test-e2e tidy e2e docker-agent docker-controller docker-janitor kind-load-agent kind-load-controller kind-load-janitor help

all: build

help:
	@echo "Common targets:"
	@echo "  make build                  - build all binaries"
	@echo "  make build-agent-linux      - build agent binary for linux/amd64"
	@echo "  make build-controller-linux - build controller binary for linux/amd64"
	@echo "  make build-janitor-linux    - build janitor binary for linux/amd64"
	@echo "  make test                   - run unit tests"
	@echo "  make test-e2e               - run e2e tests (fresh kind cluster)"
	@echo "  make test-e2e-<suite>       - run specific e2e suite"
	@echo "  make docker-agent           - build agent image"
	@echo "  make docker-controller      - build controller image"
	@echo "  make docker-janitor         - build janitor image"
	@echo ""
	@echo "E2E test suites:"
	@echo "  basicvalidation, lifecycle, scheduling, cleanupjanitor"
	@echo "  advancedfeatures, cliintegration, faultrecovery"
	@echo ""
	@echo "E2E examples:"
	@echo "  make test-e2e                          # run all tests"
	@echo "  make test-e2e-basicvalidation          # run basicvalidation suite"
	@echo "  E2E_SUITE=scheduling make test-e2e     # run scheduling suite"

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

# E2E test - full test suite with fresh kind cluster
test-e2e:
	@echo "=== Running E2E tests with fresh kind cluster ==="
	@echo "Cluster name: $(KIND_CLUSTER_NAME)"
	@echo "E2E suite: $(or $(E2E_SUITE),all)"
	@echo ""
	@if kind get clusters 2>/dev/null | grep -q "^$(KIND_CLUSTER_NAME)$$"; then \
		echo "Deleting existing kind cluster '$(KIND_CLUSTER_NAME)'..."; \
		kind delete cluster --name $(KIND_CLUSTER_NAME); \
	fi
	@echo "Creating fresh kind cluster and deploying components..."
	@FORCE_RECREATE_CLUSTER=true ./test/e2e/setup-kind.sh
	@echo ""
	@echo "Running E2E tests..."
	@if [ -n "$(E2E_SUITE)" ]; then \
		FAST_SANDBOX_E2E=1 FAST_SANDBOX_AGENT_IMAGE=$(AGENT_IMAGE) \
		$(GO) test ./test/e2e/suites/$(E2E_SUITE)/... -v; \
	else \
		FAST_SANDBOX_E2E=1 FAST_SANDBOX_AGENT_IMAGE=$(AGENT_IMAGE) \
		$(GO) test ./test/e2e/suites/... -v; \
	fi

# E2E test for specific suite (shorthand)
test-e2e-basicvalidation test-e2e-lifecycle test-e2e-scheduling test-e2e-cleanupjanitor test-e2e-advancedfeatures test-e2e-cliintegration test-e2e-faultrecovery:
	@$(MAKE) test-e2e E2E_SUITE=$(subst test-e2e-,,$@)