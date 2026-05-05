# Default registry and images
REGISTRY ?= fast-sandbox
FASTLET_IMAGE ?= $(REGISTRY)/fastlet:dev
CONTROLLER_IMAGE ?= $(REGISTRY)/controller:dev
JANITOR_IMAGE ?= $(REGISTRY)/janitor:dev

# Kind cluster name
KIND_CLUSTER_NAME ?= fast-sandbox

# E2E test settings
E2E_PROFILE ?= basic
E2E_TEST_TIMEOUT ?= 30m

# Go settings
GO ?= go
GOFLAGS ?= -gcflags="all=-N -l"

.PHONY: all build build-controller build-fastlet build-janitor build-fastlet-linux build-controller-linux build-janitor-linux build-fastctl build-fastctl-linux test test-e2e setup-e2e tidy e2e docker-fastlet docker-controller docker-janitor kind-load-fastlet kind-load-controller kind-load-janitor help

all: build

help:
	@echo "Common targets:"
	@echo "  make build                  - build all binaries"
	@echo "  make build-fastlet-linux    - build fastlet binary for linux/amd64"
	@echo "  make build-controller-linux - build controller binary for linux/amd64"
	@echo "  make build-janitor-linux    - build janitor binary for linux/amd64"
	@echo "  make test                   - run unit tests"
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

build: build-controller build-fastlet build-janitor build-fastctl

build-controller:
	$(GO) build $(GOFLAGS) -o bin/controller ./cmd/controller

build-fastlet:
	$(GO) build $(GOFLAGS) -o bin/fastlet ./cmd/fastlet

build-janitor:
	$(GO) build $(GOFLAGS) -o bin/janitor ./cmd/janitor

build-fastctl:
	$(GO) build $(GOFLAGS) -o bin/fastctl ./cmd/fastctl

# Cross-compile for linux/amd64 (for docker images)
build-fastlet-linux:
	@mkdir -p bin
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build $(GOFLAGS) -o bin/fastlet ./cmd/fastlet

build-controller-linux:
	@mkdir -p bin
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build $(GOFLAGS) -o bin/controller ./cmd/controller

build-janitor-linux:
	@mkdir -p bin
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build $(GOFLAGS) -o bin/janitor ./cmd/janitor

build-fastctl-linux:
	@mkdir -p bin
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build $(GOFLAGS) -o bin/fastctl ./cmd/fastctl

test:
	$(GO) test $(GOFLAGS) ./...

tidy:
	$(GO) mod tidy

e2e: test-e2e

docker-fastlet: build-fastlet-linux
	docker build -t $(FASTLET_IMAGE) -f build/Dockerfile.fastlet .

docker-controller: build-controller-linux
	docker build -t $(CONTROLLER_IMAGE) -f build/Dockerfile.controller .

docker-janitor: build-janitor-linux
	docker build -t $(JANITOR_IMAGE) -f build/Dockerfile.janitor .

kind-load-fastlet: docker-fastlet
	kind load docker-image $(FASTLET_IMAGE) --name fast-sandbox

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

# E2E test - run specific suite
test-e2e-basicvalidation test-e2e-lifecycle test-e2e-scheduling test-e2e-cleanupjanitor test-e2e-advancedfeatures test-e2e-cliintegration test-e2e-faultrecovery test-e2e-secureruntime:
	@echo "=== Running E2E test: $@ ==="
	@FAST_SANDBOX_FASTLET_IMAGE=$(FASTLET_IMAGE) \
		$(GO) test ./test/e2e/suites/$(subst test-e2e-,,$@)/... -v -count=1 -timeout $(E2E_TEST_TIMEOUT)
