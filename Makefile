.DEFAULT_GOAL := help

REGISTRY ?= fast-sandbox
FASTLET_IMAGE ?= $(REGISTRY)/fastlet:dev
FASTLET_PROXY_IMAGE ?= $(REGISTRY)/fastlet-proxy:dev
SANDBOX_PROXY_IMAGE ?= $(REGISTRY)/sandbox-proxy:dev
CONTROLLER_IMAGE ?= $(REGISTRY)/controller:dev
JANITOR_IMAGE ?= $(REGISTRY)/janitor:dev
BOXLITE_RUNTIME_IMAGE ?= $(REGISTRY)/boxlite-runtime:dev

GO ?= go
PYTHON ?= python3
GOPROXY ?= $(shell $(GO) env GOPROXY)
DOCKER_BUILD_FLAGS ?=
DEBUG ?= 0
COMPONENT ?= all
SCOPE ?= unit
SUITE ?= all
RUNTIME ?= container
INFRA ?= execd
PROFILE ?= basic
E2E_TEST_TIMEOUT ?= 30m

BIN_DIR := $(CURDIR)/bin
LINUX_BIN_DIR := $(CURDIR)/.build/linux-amd64
ALL_BINARIES := controller fastlet sandbox-init sandbox-tunnel fastlet-proxy sandbox-proxy janitor fastctl boxlite-runtime
CORE_IMAGES := controller fastlet fastlet-proxy sandbox-proxy janitor
ALL_IMAGES := $(CORE_IMAGES) boxlite-runtime
UNIT_PACKAGES := ./api/... ./cmd/... ./internal/... ./pkg/... ./test/e2e/env/... ./test/e2e/support/... ./test/performance/...

ifeq ($(DEBUG),1)
GO_BUILD_FLAGS := -gcflags="all=-N -l"
else ifeq ($(DEBUG),0)
GO_BUILD_FLAGS :=
else
$(error DEBUG must be 0 or 1)
endif

# Pinned generators live inside the repository so output does not depend on
# globally installed Go tools. Protoc installation currently targets the Linux
# development and CI environment used by this project.
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

.PHONY: help build images generate verify test e2e env quickstart quickstart-forward tidy
.PHONY: _network-test

help:
	@echo "Fast Sandbox developer interface"
	@echo ""
	@echo "  make build [COMPONENT=all] [DEBUG=0|1]"
	@echo "      Build host binaries. COMPONENT may be any cmd/ directory name."
	@echo ""
	@echo "  make images [COMPONENT=all|core|<image>]"
	@echo "      Build Linux binaries and development container images."
	@echo ""
	@echo "  make generate"
	@echo "      Regenerate Go/Python protobuf, deepcopy, and CRD output."
	@echo ""
	@echo "  make verify"
	@echo "      Verify generated output and run unit tests."
	@echo ""
	@echo "  make test [SCOPE=unit|python|race|network]"
	@echo "      Run one local or Linux integration test scope."
	@echo ""
	@echo "  make env PROFILE=basic|gvisor|kata-qemu|kata-clh|kata-fc"
	@echo "      Prepare a reusable kind environment without running tests."
	@echo ""
	@echo "  make e2e [SUITE=all|controlplane|network|proxy|infra|sdk|quickstart|runtime|drain|<suite>]"
	@echo "           [RUNTIME=container|gvisor|kata|boxlite]"
	@echo "      Run E2E tests; each suite prepares the runtime profile it needs."
	@echo ""
	@echo "  make quickstart [RUNTIME=container|gvisor|kata-qemu|kata-clh] [INFRA=execd|minimal]"
	@echo "      Prepare an interactive environment and print copy/paste examples."
	@echo ""
	@echo "  make quickstart-forward"
	@echo "      Forward Fast-Path and Sandbox Proxy until Ctrl-C."
	@echo ""
	@echo "  make tidy"
	@echo "      Run go mod tidy."

build:
	@case " $(ALL_BINARIES) " in \
		*" $(COMPONENT) "*) components="$(COMPONENT)" ;; \
		*) if [ "$(COMPONENT)" = "all" ]; then components="$(ALL_BINARIES)"; \
		   else echo "unknown build COMPONENT=$(COMPONENT)" >&2; exit 2; fi ;; \
	esac; \
	mkdir -p "$(BIN_DIR)"; \
	for component in $$components; do \
		echo "==> build $$component"; \
		$(GO) build $(GO_BUILD_FLAGS) -o "$(BIN_DIR)/$$component" "./cmd/$$component" || exit $$?; \
	done

images:
	@case " $(ALL_IMAGES) " in \
		*" $(COMPONENT) "*) components="$(COMPONENT)" ;; \
		*) case "$(COMPONENT)" in \
			all) components="$(ALL_IMAGES)" ;; \
			core) components="$(CORE_IMAGES)" ;; \
			*) echo "unknown image COMPONENT=$(COMPONENT)" >&2; exit 2 ;; \
		   esac ;; \
	esac; \
	mkdir -p "$(LINUX_BIN_DIR)"; \
	for component in $$components; do \
		case "$$component" in \
			fastlet) binaries="fastlet sandbox-init sandbox-tunnel" ;; \
			boxlite-runtime) binaries="" ;; \
			*) binaries="$$component" ;; \
		esac; \
		for binary in $$binaries; do \
			echo "==> build linux/amd64 $$binary"; \
			CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build $(GO_BUILD_FLAGS) \
				-o "$(LINUX_BIN_DIR)/$$binary" "./cmd/$$binary" || exit $$?; \
		done; \
		echo "==> image $$component"; \
		case "$$component" in \
			controller) docker build $(DOCKER_BUILD_FLAGS) -t "$(CONTROLLER_IMAGE)" -f build/Dockerfile.controller . ;; \
			fastlet) docker build $(DOCKER_BUILD_FLAGS) -t "$(FASTLET_IMAGE)" -f build/Dockerfile.fastlet . ;; \
			fastlet-proxy) docker build $(DOCKER_BUILD_FLAGS) -t "$(FASTLET_PROXY_IMAGE)" -f build/Dockerfile.fastlet-proxy . ;; \
			sandbox-proxy) docker build $(DOCKER_BUILD_FLAGS) -t "$(SANDBOX_PROXY_IMAGE)" -f build/Dockerfile.sandbox-proxy . ;; \
			janitor) docker build $(DOCKER_BUILD_FLAGS) -t "$(JANITOR_IMAGE)" -f build/Dockerfile.janitor . ;; \
			boxlite-runtime) docker build $(DOCKER_BUILD_FLAGS) \
				--build-arg GOPROXY="$(GOPROXY)" \
				-t "$(BOXLITE_RUNTIME_IMAGE)" -f build/Dockerfile.boxlite-runtime . ;; \
		esac || exit $$?; \
	done

$(PROTOC):
	@mkdir -p $(TOOLS_DIR) $(PROTOC_ROOT)
	@curl -fsSL https://github.com/protocolbuffers/protobuf/releases/download/v$(PROTOC_VERSION)/protoc-$(PROTOC_VERSION)-linux-x86_64.zip -o $(TOOLS_DIR)/protoc-$(PROTOC_VERSION)-linux-x86_64.zip
	@echo "$(PROTOC_SHA256_LINUX_X86_64)  $(TOOLS_DIR)/protoc-$(PROTOC_VERSION)-linux-x86_64.zip" | sha256sum -c -
	@if command -v unzip >/dev/null 2>&1; then \
		unzip -q -o $(TOOLS_DIR)/protoc-$(PROTOC_VERSION)-linux-x86_64.zip -d $(PROTOC_ROOT); \
	elif command -v busybox >/dev/null 2>&1; then \
		busybox unzip -q -o $(TOOLS_DIR)/protoc-$(PROTOC_VERSION)-linux-x86_64.zip -d $(PROTOC_ROOT); \
	else \
		$(PYTHON) -m zipfile -e $(TOOLS_DIR)/protoc-$(PROTOC_VERSION)-linux-x86_64.zip $(PROTOC_ROOT); \
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

generate: $(PROTOC) $(TOOLS_BIN)/protoc-gen-go $(TOOLS_BIN)/protoc-gen-go-grpc $(CONTROLLER_GEN)
	@PATH=$(TOOLS_BIN):$$PATH $(PROTOC) -I . \
		--go_out=. --go_opt=paths=source_relative \
		--go-grpc_out=. --go-grpc_opt=paths=source_relative \
		api/proto/v1/fastpath.proto
	@PYTHON=$(PYTHON) bash sdk/python/generate_proto.sh
	@$(CONTROLLER_GEN) object paths=./api/v1alpha1/...
	@$(CONTROLLER_GEN) crd paths=./api/v1alpha1/... output:crd:artifacts:config=config/crd

verify: generate
	@git diff --exit-code -- \
		api/proto/v1 \
		api/v1alpha1/zz_generated.deepcopy.go \
		config/crd \
		sdk/python/fast_sandbox/proto
	@$(GO) test $(UNIT_PACKAGES)

test:
	@case "$(SCOPE)" in \
		unit) $(GO) test $(UNIT_PACKAGES) ;; \
		python) PYTHONPATH=sdk/python:$${PYTHONPATH:-} $(PYTHON) -m unittest discover -s sdk/python/tests -v ;; \
		race) $(GO) test -race $(UNIT_PACKAGES) ;; \
		network) $(MAKE) --no-print-directory _network-test ;; \
		*) echo "unknown test SCOPE=$(SCOPE); expected unit, python, race, or network" >&2; exit 2 ;; \
	esac

_network-test:
	@$(MAKE) --no-print-directory images COMPONENT=fastlet
	@mkdir -p "$(LINUX_BIN_DIR)"
	@CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) test -c \
		-o "$(LINUX_BIN_DIR)/network.test" ./internal/fastlet/network
	@docker build $(DOCKER_BUILD_FLAGS) \
		--build-arg FASTLET_IMAGE=$(FASTLET_IMAGE) \
		-t fast-sandbox/network-test:dev -f build/Dockerfile.network-test .
	@docker run --rm --privileged \
		-e FAST_SANDBOX_RUN_PRIVILEGED_NETWORK_TEST=1 \
		fast-sandbox/network-test:dev \
		-test.run '^TestLinuxNetNSDriverPrivileged$$' -test.v

env:
	@case "$(PROFILE)" in \
		basic|gvisor|kata-qemu|kata-clh|kata-fc) ;; \
		*) echo "unknown PROFILE=$(PROFILE)" >&2; exit 2 ;; \
	esac
	@FAST_SANDBOX_FASTLET_IMAGE=$(FASTLET_IMAGE) \
		$(GO) run ./test/e2e/env/cmd/setup \
		-profile "$(PROFILE)" -timeout "$(E2E_TEST_TIMEOUT)"

e2e:
	@case "$(SUITE)" in \
		all) packages="./test/e2e/suites/..."; flags="-p 1 -failfast" ;; \
		controlplane) packages="./test/e2e/suites/controlplane/... ./test/e2e/suites/basicvalidation/... ./test/e2e/suites/lifecycle/... ./test/e2e/suites/scheduling/... ./test/e2e/suites/cleanupjanitor/..."; flags="-p 1 -failfast" ;; \
		network) packages="./test/e2e/suites/basicvalidation/..."; flags="" ;; \
		proxy) packages="./test/e2e/suites/basicvalidation/..."; flags="-run ^TestSandboxProxyDataPlane$$" ;; \
		infra) packages="./test/e2e/suites/basicvalidation/..."; flags="-run ^TestInfraRuntimeAugmentation$$" ;; \
		sdk) packages="./test/e2e/suites/basicvalidation/..."; flags="-run ^TestSDKAdapterDataPlane$$" ;; \
		quickstart) packages="./test/e2e/suites/cliintegration/..."; flags="-run ^TestQuickStartOpenSandboxExecd$$" ;; \
		drain) packages="./test/e2e/suites/drain/..."; flags="" ;; \
		runtime) \
			packages="./test/e2e/suites/secureruntime/..."; \
			case "$(RUNTIME)" in \
				container) flags="-run ^TestRuntimeValidationContainerDefault$$" ;; \
				gvisor) flags="-run ^TestGVisor" ;; \
				kata) flags="-p 1 -failfast -run ^TestKata\\(QemuSandbox\\|ClhSandbox\\|FcSandbox\\)$$" ;; \
				boxlite) flags="-run ^TestRuntimeValidationUnsupportedBoxLite$$" ;; \
				*) echo "unknown runtime gate RUNTIME=$(RUNTIME)" >&2; exit 2 ;; \
			esac ;; \
		*) \
			if [ -d "test/e2e/suites/$(SUITE)" ]; then \
				packages="./test/e2e/suites/$(SUITE)/..."; flags=""; \
			else \
				echo "unknown E2E SUITE=$(SUITE)" >&2; exit 2; \
			fi ;; \
	esac; \
	FAST_SANDBOX_FASTLET_IMAGE=$(FASTLET_IMAGE) \
		$(GO) test $$flags $$packages -v -count=1 -timeout "$(E2E_TEST_TIMEOUT)"

quickstart:
	@case "$(RUNTIME):$(INFRA)" in \
		container:execd) \
			profile=basic; pool_file=config/samples/pool-container-execd.yaml; \
			pool=quickstart-execd-pool; sandbox=quickstart-execd-sandbox; data_plane=execd ;; \
		container:minimal) \
			profile=basic; pool_file=config/samples/pool-container.yaml; \
			pool=quickstart-pool; sandbox=quickstart-minimal-sandbox; data_plane= ;; \
		gvisor:execd) \
			profile=gvisor; pool_file=config/samples/pool-gvisor.yaml; \
			pool=gvisor-execd-pool; sandbox=quickstart-gvisor-execd-sandbox; data_plane=execd ;; \
		kata-qemu:execd) \
			profile=kata-qemu; pool_file=config/samples/pool-kata-qemu.yaml; \
			pool=kata-qemu-execd-pool; sandbox=quickstart-kata-qemu-execd-sandbox; data_plane=execd ;; \
		kata-clh:execd) \
			profile=kata-clh; pool_file=config/samples/pool-kata.yaml; \
			pool=kata-clh-execd-pool; sandbox=quickstart-kata-clh-execd-sandbox; data_plane=execd ;; \
		*) echo "unsupported Quick Start RUNTIME=$(RUNTIME) INFRA=$(INFRA)" >&2; exit 2 ;; \
	esac; \
	echo ""; \
	echo "Fast Sandbox Quick Start"; \
	echo "Runtime:  $(RUNTIME)"; \
	echo "Infra:    $(INFRA)"; \
	echo "Profile:  $$profile"; \
	echo ""; \
	echo "The first run builds images and can take several minutes."; \
	echo "Existing clusters and cached runtime artifacts are reused."; \
	echo ""; \
	echo "[quickstart 1/4] Preparing the reusable kind environment..."; \
	$(MAKE) --no-print-directory env PROFILE=$$profile || exit $$?; \
	echo "[quickstart 2/4] Applying SandboxPool $$pool..."; \
	kubectl apply -f "$$pool_file" || exit $$?; \
	image_id=$$(docker image inspect --format='{{.Id}}' "$(FASTLET_IMAGE)") || exit $$?; \
	patch=$$(printf '{"spec":{"fastletTemplate":{"metadata":{"annotations":{"fast-sandbox.io/quickstart-image-id":"%s"}}}}}' "$$image_id"); \
	kubectl patch "sandboxpool/$$pool" --type=merge -p "$$patch" >/dev/null || exit $$?; \
	echo "[quickstart 3/4] Waiting for a ready Fastlet built from $$image_id..."; \
	ready=false; \
	for i in $$(seq 1 90); do \
		for pod in $$(kubectl get pods -l "fast-sandbox.io/pool=$$pool" -o name); do \
			pod_image_id=$$(kubectl get "$$pod" -o jsonpath='{.metadata.annotations.fast-sandbox\.io/quickstart-image-id}'); \
			pod_ready=$$(kubectl get "$$pod" -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}'); \
			if [ "$$pod_image_id" = "$$image_id" ] && [ "$$pod_ready" = "True" ]; then \
				ready=true; ready_pod=$$pod; ready_after=$$((($$i - 1) * 2)); break 2; \
			fi; \
		done; \
		if [ $$(( $$i % 5 )) -eq 0 ]; then \
			echo "  still waiting ($$(( $$i * 2 ))s elapsed):"; \
			kubectl get pods -l "fast-sandbox.io/pool=$$pool" --no-headers 2>/dev/null | sed 's/^/    /'; \
		fi; \
		sleep 2; \
	done; \
	if [ "$$ready" != true ]; then \
		echo "timed out waiting for the current Fastlet image in Pool $$pool" >&2; \
		kubectl get pods -l "fast-sandbox.io/pool=$$pool" -o wide; \
		exit 1; \
	fi; \
	echo "  ready after $${ready_after}s: $$ready_pod"; \
	echo "[quickstart 4/4] Building fastctl and preparing examples..."; \
	$(MAKE) --no-print-directory build COMPONENT=fastctl || exit $$?; \
	echo ""; \
	echo "Quick Start environment is ready."; \
	printf "Context: "; kubectl config current-context; \
	echo "Pool:    $$pool"; \
	echo ""; \
	bash test/e2e/hack/quickstart-print.sh "$$pool" "$$sandbox" "$$data_plane"

quickstart-forward:
	@bash test/e2e/hack/quickstart-forward.sh

tidy:
	@$(GO) mod tidy
