GO_PROJECT_NAME := spx
SHELL := /bin/bash

# GOFIPS140 is a global build-config change: it alters every package's action ID,
# stdlib included, so a target that omits it shares no build-cache entry with one
# that sets it. Exported once here rather than per-recipe so the two can't drift.
export GOFIPS140 := v1.0.0

# Detect architecture for cross-platform support
ARCH := $(shell uname -m)
ifeq ($(ARCH),x86_64)
  GO_ARCH := amd64
  AWS_ARCH := x86_64
else ifeq ($(ARCH),aarch64)
  GO_ARCH := arm64
  AWS_ARCH := aarch64
else ifeq ($(ARCH),arm64)
  GO_ARCH := arm64
  AWS_ARCH := aarch64
else
  $(error Unsupported architecture: $(ARCH). Only x86_64 and aarch64/arm64 are supported.)
endif

# Ask Go whether workspace mode is active
IN_WORKSPACE := $(shell go env GOWORK)

# Use -mod=mod unless Go reports an active workspace path
ifeq ($(IN_WORKSPACE),)
  GO_BUILD_MOD := -mod=mod
else ifeq ($(IN_WORKSPACE),off)
  GO_BUILD_MOD := -mod=mod
else
  GO_BUILD_MOD :=
endif

# Quiet-mode filters (active when QUIET=1, set by preflight via recursive make)
# Note: grep pipelines use PIPESTATUS[0] so the exit status of `go test`
# propagates through the filter — otherwise a test failure is swallowed by
# grep's own (success) exit code and preflight prints "passed" on red.
ifdef QUIET
  _Q     = @
  _COVQ  = 2>&1 | { grep -Ev '^\s*(ok|PASS|\?|=== RUN|--- PASS:)\s' | grep -v 'coverage: 0\.0%' || true; }; exit $${PIPESTATUS[0]}
  _RACEQ = 2>&1 | { grep -Ev '^\s*(ok|PASS|\?|=== RUN|--- PASS:)\s' || true; }; exit $${PIPESTATUS[0]}
  _SECQ  = >
else
  _Q     =
  _COVQ  =
  _RACEQ =
  _SECQ  = 2>&1 | tee
endif

build: go_build build-installer build-lb-agent generate-aws-model-coverage

# Build spinifex-ui frontend (requires pnpm)
build-ui:
	@echo -e "\n....Building spinifex-ui frontend...."
	cd spinifex/runtime/roles/spinifexui/frontend && pnpm build

# GO commands
VERSION ?= $(shell git describe --tags --always --dirty --exclude '*-dev')
COMMIT  ?= $(shell git rev-parse --short HEAD)
LDFLAGS := -s -w -X github.com/mulgadc/spinifex/spinifex/operator/cli.Version=$(VERSION) -X github.com/mulgadc/spinifex/spinifex/operator/cli.Commit=$(COMMIT)

go_build:
	@echo -e "\n....Building $(GO_PROJECT_NAME)"
	go build $(GO_BUILD_MOD) -ldflags "$(LDFLAGS)" -o ./bin/$(GO_PROJECT_NAME) cmd/spinifex/main.go

build-installer:
	@echo -e "\n....Building spinifex-installer"
	go build -ldflags "-s -w" -o ./bin/spinifex-installer cmd/installer/main.go

build-lb-agent:
	@echo -e "\n....Building lb-agent (static)"
	CGO_ENABLED=0 go build -ldflags "-s -w" -o ./bin/lb-agent cmd/lb-agent/main.go

build-ecs-agent: ## Build the ecs-agent (ships in the ECS-AMI guest; not in host `build`)
	@echo -e "\n....Building ecs-agent (static)"
	CGO_ENABLED=0 go build -ldflags "-s -w -X main.version=$(VERSION)" -o ./bin/ecs-agent ./cmd/ecs-agent

build-loadgen: ## Build spx-loadgen (drives the stress harness's measured load; not shipped to nodes)
	@echo -e "\n....Building spx-loadgen"
	go build -ldflags "-s -w" -o ./bin/spx-loadgen ./cmd/spx-loadgen

build-system-image: ## Build a system image from manifest (use IMAGE=lb or IMAGE=eks-node)
ifndef IMAGE
	$(error IMAGE is required. Usage: make build-system-image IMAGE=lb)
endif
	@if [ -f scripts/images/$(IMAGE).conf ]; then \
		./scripts/build-system-image.sh scripts/images/$(IMAGE).conf $(if $(IMPORT),--import); \
	elif [ -f scripts/images/$(IMAGE)/manifest.conf ]; then \
		./scripts/build-system-image.sh scripts/images/$(IMAGE)/manifest.conf $(if $(IMPORT),--import); \
	else \
		echo "ERROR: no manifest at scripts/images/$(IMAGE).conf or scripts/images/$(IMAGE)/manifest.conf"; \
		exit 1; \
	fi

build-eks-node-image: ## Build the unified eks-node AMI (K3s server+agent; role at first boot; IMPORT=1 to register)
	$(MAKE) build-system-image IMAGE=eks-node

import-eks-node-image: ## Build + register the eks-node AMI (requires a running cluster)
	$(MAKE) build-system-image IMAGE=eks-node IMPORT=1

publish-eks-node-image: ## Build + publish the eks-node AMI to Cloudflare R2 (needs R2_ENDPOINT + AWS_* env)
	./scripts/publish-system-image.sh scripts/images/eks-node/manifest.conf --build

# The Ubuntu/systemd EKS image, as opposed to build-eks-node-image's Alpine one.
# Composition lives in mkosi-build.sh's image_profiles table, so the image name
# is the whole interface here.
build-eks-node-mkosi-image: ## Build the Ubuntu mkosi EKS node image (server+agent; role at first boot)
	./scripts/mkosi-build.sh --image spinifex-eks-node

build-ecs-node-image: ## Build the spinifex-ecs-node AMI (Alpine + containerd + ecs-agent; IMPORT=1 to register)
	$(MAKE) build-system-image IMAGE=ecs-agent

import-ecs-node-image: ## Build + register the ecs-node AMI (requires a running cluster)
	$(MAKE) build-system-image IMAGE=ecs-agent IMPORT=1

build-rds-postgres-image: ## Build the spinifex-rds-postgres AMI (Alpine + PostgreSQL 18 + rds-init; IMPORT=1 to register)
	$(MAKE) build-system-image IMAGE=rds-postgres

import-rds-postgres-image: ## Build + register the rds-postgres AMI (requires a running cluster)
	$(MAKE) build-system-image IMAGE=rds-postgres IMPORT=1

build-rds-mariadb-image: ## Build the spinifex-rds-mariadb AMI (Alpine + MariaDB 11.8 + rds-init; IMPORT=1 to register)
	$(MAKE) build-system-image IMAGE=rds-mariadb

import-rds-mariadb-image: ## Build + register the rds-mariadb AMI (requires a running cluster)
	$(MAKE) build-system-image IMAGE=rds-mariadb IMPORT=1

MICROVM_OUT_DIR := build/microvm
MICROVM_ARTIFACTS := $(MICROVM_OUT_DIR)/vmlinuz $(MICROVM_OUT_DIR)/initramfs.cpio.gz
MICROVM_INPUTS := scripts/build-microvm-image.sh $(MICROVM_OUT_DIR)/init.sh $(MICROVM_OUT_DIR)/inittab bin/lb-agent

# Grouped target — script writes both files in one run.
$(MICROVM_ARTIFACTS) &: $(MICROVM_INPUTS)
	./scripts/build-microvm-image.sh

# Only triggers when bin/lb-agent is missing; preserves the artifact's mtime so
# build-microvm-image stays correctly stale-aware.
bin/lb-agent:
	$(MAKE) build-lb-agent

# The SQS store microVM image: a separate artifact from the LB image above, not
# installed or released by any target yet.
SQS_STORE_MICROVM_OUT_DIR := $(MICROVM_OUT_DIR)/sqs-store
SQS_STORE_MICROVM_ARTIFACTS := $(SQS_STORE_MICROVM_OUT_DIR)/vmlinuz $(SQS_STORE_MICROVM_OUT_DIR)/initramfs.cpio.gz
SQS_STORE_MICROVM_INPUTS := scripts/build-sqs-store-microvm-image.sh scripts/lib/sqs-store-microvm-publish.sh $(SQS_STORE_MICROVM_OUT_DIR)/init.sh $(SQS_STORE_MICROVM_OUT_DIR)/inittab $(addprefix $(SQS_STORE_MICROVM_OUT_DIR)/,sqs-store-lib.sh sqs-store-prepare sqs-store-run sqs-store-stop) bin/nats-server

$(SQS_STORE_MICROVM_ARTIFACTS) &: $(SQS_STORE_MICROVM_INPUTS)
	./scripts/build-sqs-store-microvm-image.sh

# The dedicated SQS store server, built from the go.mod-pinned module so the
# image ships exactly the reviewed version. Static for the musl guest.
bin/nats-server: go.mod go.sum
	@mkdir -p bin
	GOWORK=off CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags '-s -w' -o $@ github.com/nats-io/nats-server/v2

build-sqs-store-microvm-image: $(SQS_STORE_MICROVM_ARTIFACTS) ## Build the SQS store microVM kernel and initramfs

# The shared SQS worker microVM image: the store image's /init and kernel with
# the worker binary instead of nats-server. Not installed or released yet.
SQS_WORKER_MICROVM_OUT_DIR := $(MICROVM_OUT_DIR)/sqs-worker
SQS_WORKER_MICROVM_ARTIFACTS := $(SQS_WORKER_MICROVM_OUT_DIR)/vmlinuz $(SQS_WORKER_MICROVM_OUT_DIR)/initramfs.cpio.gz
SQS_WORKER_MICROVM_INPUTS := scripts/build-sqs-store-microvm-image.sh scripts/lib/sqs-store-microvm-publish.sh $(SQS_STORE_MICROVM_OUT_DIR)/init.sh $(SQS_WORKER_MICROVM_OUT_DIR)/inittab $(addprefix $(SQS_WORKER_MICROVM_OUT_DIR)/,sqs-worker-prepare sqs-worker-stop) bin/sqs-worker

$(SQS_WORKER_MICROVM_ARTIFACTS) &: $(SQS_WORKER_MICROVM_INPUTS)
	SQS_MICROVM_PROFILE=worker ./scripts/build-sqs-store-microvm-image.sh

bin/sqs-worker: go.mod go.sum $(shell find cmd/sqs-worker spinifex/agents/sqs -name '*.go' ! -name '*_test.go')
	@mkdir -p bin
	GOWORK=off CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags '-s -w' -o $@ ./cmd/sqs-worker

build-sqs-worker-microvm-image: $(SQS_WORKER_MICROVM_ARTIFACTS) ## Build the shared SQS worker microVM kernel and initramfs

# qboot is the microvm firmware. The source tarball ships with each release
# because qboot is GPL-2.0.
QBOOT_COMMIT := $(shell sed -n 's/^QBOOT_COMMIT="\(.*\)"$$/\1/p' scripts/build-qboot.sh)
QBOOT_SOURCE := build/qboot/qboot-$(QBOOT_COMMIT).tar.gz
QBOOT_ARTIFACTS := $(MICROVM_OUT_DIR)/qboot.rom $(MICROVM_OUT_DIR)/qboot.LICENSE $(QBOOT_SOURCE)

$(QBOOT_ARTIFACTS) &: scripts/build-qboot.sh
	./scripts/build-qboot.sh

build-microvm-image: $(MICROVM_ARTIFACTS) $(QBOOT_ARTIFACTS) ## Build microVM kernel, initramfs and qboot firmware (incremental — skips when up to date)

install-sqs-store-microvm: $(SQS_STORE_MICROVM_ARTIFACTS) ## Install the SQS store microVM image to /usr/share/spinifex/microvm/sqs-store/
	sudo install -d /usr/share/spinifex/microvm/sqs-store
	sudo rm -f /usr/share/spinifex/microvm/sqs-store/initramfs.cpio.gz
	sudo install -m 0644 $(SQS_STORE_MICROVM_OUT_DIR)/vmlinuz /usr/share/spinifex/microvm/sqs-store/vmlinuz
	sudo install -m 0644 $(SQS_STORE_MICROVM_OUT_DIR)/initramfs.cpio.gz /usr/share/spinifex/microvm/sqs-store/initramfs.cpio.gz

install-sqs-worker-microvm: $(SQS_WORKER_MICROVM_ARTIFACTS) ## Install the SQS worker microVM image to /usr/share/spinifex/microvm/sqs-worker/
	sudo install -d /usr/share/spinifex/microvm/sqs-worker
	sudo rm -f /usr/share/spinifex/microvm/sqs-worker/initramfs.cpio.gz
	sudo install -m 0644 $(SQS_WORKER_MICROVM_OUT_DIR)/vmlinuz /usr/share/spinifex/microvm/sqs-worker/vmlinuz
	sudo install -m 0644 $(SQS_WORKER_MICROVM_OUT_DIR)/initramfs.cpio.gz /usr/share/spinifex/microvm/sqs-worker/initramfs.cpio.gz

install-microvm: $(MICROVM_ARTIFACTS) $(QBOOT_ARTIFACTS) ## Install microVM artifacts to /usr/share/spinifex/microvm/
	sudo install -d /usr/share/spinifex/microvm
	sudo install -m 0644 $(MICROVM_OUT_DIR)/vmlinuz /usr/share/spinifex/microvm/vmlinuz
	sudo install -m 0644 $(MICROVM_OUT_DIR)/initramfs.cpio.gz /usr/share/spinifex/microvm/initramfs.cpio.gz
	sudo install -m 0644 $(MICROVM_OUT_DIR)/qboot.rom /usr/share/spinifex/microvm/qboot.rom
	sudo install -m 0644 $(MICROVM_OUT_DIR)/qboot.LICENSE /usr/share/spinifex/microvm/qboot.LICENSE

# the pre-commit gate: lint, vuln, and the unit and e2e-harness tiers.
# integration and race tests skipped to keep quick, they run in CI.
#
# -j2 overlaps the only two heavy gates, lint and test-cover: alone each leaves
# most of the machine idle, so pairing them cuts a cold run by about a quarter.
# -Otarget keeps each gate's output in one block instead of interleaving them.
preflight:
	@$(MAKE) --no-print-directory -j2 -Otarget QUIET=1 lint govulncheck test-cover diff-coverage test-harness test-build-scripts
	@echo -e "\n ✅ Preflight passed — safe to commit."

# Shell suites + shellcheck for build/scripts/, the systemd-unit helpers that
# ship on every node (unlike scripts/images/, kept in preflight: a wrong
# restart decision here is an outage, not asset churn).
#
# NODE_SCRIPTS are the four that setup.sh installs into
# /usr/local/share/spinifex on every node. They are covered here for the same
# reason: between them they install the software, wire OVN, erase the state and
# remove the install, so a shellcheck warning in one is an outage or a wrong
# deletion, not a style note.
NODE_SCRIPTS := scripts/setup.sh scripts/setup-ovn.sh scripts/install-node.sh scripts/smoke-test.sh scripts/node-reset.sh scripts/uninstall-spx.sh scripts/lib/distro.sh scripts/lib/stage-sqs-store-microvm.sh

test-build-scripts:
	@echo -e "\n....Running build/scripts/**/*_test.sh...."
	@for t in $$(find build/scripts -name '*_test.sh' | sort); do \
		echo "-- $$t"; \
		sh "$$t" || exit 1; \
	done
	@echo -e "\n....Running scripts/*_test.sh, scripts/lib/*_test.sh and scripts/appliance/*_test.sh...."
	@for t in $$( (find scripts -maxdepth 1 -name '*_test.sh'; find scripts/lib scripts/appliance -name '*_test.sh') | sort); do \
		echo "-- $$t"; \
		sh "$$t" || exit 1; \
	done
	@echo -e "\n....Running shellcheck over build/scripts/**/*.sh + the node scripts...."
	shellcheck -S warning $$(find build/scripts -name '*.sh' | sort) $(NODE_SCRIPTS)
	shellcheck -S warning -x scripts/build-oracle-appliance.sh $$(find scripts/appliance -name '*.sh' | sort)
	@echo "  test-build-scripts ok"

# E2E harness unit tests. Build-tagged `e2e` so they're skipped by the
# default `go test ./spinifex/...`. Runs with mocked AWS clients — no
# infrastructure required, safe to run in CI without a cluster.
#
# diskperf/ is here for the same reason: its baseline comparison and probe
# accounting decide whether the disk-performance gate passes, and that
# arithmetic has to be covered off-cluster or the gate is trusted untested.
test-harness:
	@echo -e "\n....Running e2e harness unit tests...."
	$(_Q)LOG_IGNORE=1 go test -tags=e2e -timeout 60s ./tests/e2e/harness/... ./tests/e2e/diskperf/... $(_RACEQ)

# In-process integration tier: the real gateway router against embedded NATS
# JetStream, with only the daemon-side NATS subjects stubbed.
AWS_MODEL_CONFORMANCE_REPORT ?= $(CURDIR)/.cache/aws-model-conformance-report.txt
AWS_MODEL_CONFORMANCE_MODE ?= fail
AWS_MODEL_OPERATION_COVERAGE_DIR ?= $(CURDIR)/docs/coverage
# -count=1 is load-bearing: the conformance report is written by the test binary,
# so a cached pass skips the run, leaves no report, and the cat below fails the
# target. It also keeps the conformance gate honest — a cached result would mean
# the check never ran against this commit. The generated request sweep adds
# about 15s, most of it EC2 fan-outs waiting out their window for instances
# no daemon-lite owns.
test-integration:
	@echo -e "\n....Running in-process integration tests...."
	$(_Q)LOG_IGNORE=1 AWS_MODEL_CONFORMANCE_MODE=$(AWS_MODEL_CONFORMANCE_MODE) AWS_MODEL_CONFORMANCE_REPORT=$(AWS_MODEL_CONFORMANCE_REPORT) go test -count=1 -tags=integration -timeout 120s ./tests/integration/... $(_RACEQ)
	@cat $(AWS_MODEL_CONFORMANCE_REPORT)
	@$(MAKE) --no-print-directory generate-aws-model-coverage
	@echo "AWS operation coverage: $(AWS_MODEL_OPERATION_COVERAGE_DIR)"

generate-aws-model-coverage:
	@go run ./cmd/aws-model-coverage -out $(AWS_MODEL_OPERATION_COVERAGE_DIR) -json $(AWS_MODEL_OPERATION_COVERAGE_DIR)/coverage.json

aws-model-coverage: generate-aws-model-coverage
	@go run ./cmd/aws-model-coverage

# Segscan storage oracle: needs the mulga umbrella repo's scripts/segscan
# checked out alongside spinifex (see internal/testkit/segscanoracle), which
# is not the default local or CI layout, so this is a separate target from
# test-integration rather than folded into it. Skips itself when segscan's
# source isn't found.
test-segscan-oracle:
	@echo -e "\n....Running segscan storage oracle test...."
	$(_Q)LOG_IGNORE=1 go test -tags=integration,segscanoracle -timeout 120s ./tests/integration/... -run TestSegscanOracle $(_RACEQ)

# Run unit tests
test:
	@echo -e "\n....Running tests for $(GO_PROJECT_NAME)...."
	LOG_IGNORE=1 go test -timeout 180s ./spinifex/... ./cmd/... ./internal/... ./contracts/... ./architecture/...

# Empty locally, where reusing a cached result between runs is the point.
# CI passes -count=1 so a green run means the suite executed against that commit
# rather than replaying results the persisted build cache carried over.
GOTESTFLAGS ?=

# Run unit tests with coverage profile
COVERPROFILE ?= coverage.out
test-cover:
	@echo -e "\n....Running tests with coverage for $(GO_PROJECT_NAME)...."
	$(_Q)LOG_IGNORE=1 go test $(GOTESTFLAGS) -timeout 180s -coverprofile=$(COVERPROFILE) -covermode=atomic ./spinifex/... ./cmd/... ./internal/... ./contracts/... ./architecture/... $(_COVQ)
	@scripts/check-coverage.sh $(COVERPROFILE) $(QUIET)

# Run unit tests with race detector
test-race:
	@echo -e "\n....Running tests with race detector for $(GO_PROJECT_NAME)...."
	$(_Q)LOG_IGNORE=1 go test $(GOTESTFLAGS) -race -timeout 300s ./spinifex/... ./cmd/... ./internal/... ./contracts/... ./architecture/... $(_RACEQ)

# Unit tests for in-repo GitHub Actions (e.g. .github/actions/e2e-analyze).
# Kept out of `test-cover` so coverage % isn't diluted by CI-only tooling.
test-actions:
	@echo -e "\n....Running action tests...."
	LOG_IGNORE=1 go test $(GOTESTFLAGS) -timeout 60s ./.github/actions/...

# Shell suites + shellcheck for scripts/images/ and images/mkosi.profiles/
# helpers baked into system images. Kept out of `preflight` (a dedicated CI
# job gates it on scripts/images/**+images/mkosi.profiles/** changes instead)
# so image-asset churn doesn't run on every Go contributor's commit.
#
# images/mkosi.profiles/ ships scripts with no .sh suffix (mkosi's own
# mkosi.*.chroot lifecycle hooks, and wrapper binaries like vllm-serve under
# mkosi.extra/) so a plain `-name '*.sh'` glob silently skips exactly the
# files most worth linting. Sources there are found by shebang instead of
# name; this also means a profile that ships no scripts at all yields an
# empty match rather than an error.
test-images:
	@echo -e "\n....Running scripts/images/**/*_test.sh...."
	@for t in $$(find scripts/images -name '*_test.sh' | sort); do \
		echo "-- $$t"; \
		bash "$$t" || exit 1; \
	done
	@echo -e "\n....Running images/mkosi.profiles/**/*_test.sh...."
	@for t in $$(find images/mkosi.profiles -name '*_test.sh' | sort); do \
		echo "-- $$t"; \
		bash "$$t" || exit 1; \
	done
	@echo -e "\n....Running shellcheck over scripts/images/**/*.sh...."
	shellcheck -S warning $$(find scripts/images -name '*.sh' | sort)
	@echo -e "\n....Running shellcheck over images/mkosi.profiles/** shell sources...."
	@srcs="$$(grep -rlIE '^#!/bin/(bash|sh)' images/mkosi.profiles 2>/dev/null | sort)"; \
	if [ -n "$$srcs" ]; then \
		shellcheck -S warning $$srcs; \
	else \
		echo "  (no shell sources found under images/mkosi.profiles)"; \
	fi
	@echo "  test-images ok"

# Check that new/changed code meets coverage threshold (runs tests first)
diff-coverage: test-cover
	@QUIET=$(QUIET) scripts/diff-coverage.sh $(COVERPROFILE)

# Check that newly added test files use an external test package
test-package-check:
	@QUIET=$(QUIET) scripts/check-test-package.sh

bench:
	@echo -e "\n....Running benchmarks for $(GO_PROJECT_NAME)...."
	LOG_IGNORE=1 go test -benchmem -run=. -bench=. ./...

# Fuzz every Fuzz* target for FUZZTIME each; -fuzz takes one target per run.
# Plain `go test` already replays the seeds and testdata/fuzz on preflight.
FUZZTIME ?= 30s
fuzz:
	@set -e; grep -rlE '^func Fuzz\w+\(' --include='*_test.go' spinifex cmd internal | while read -r file; do \
		for name in $$(grep -oE '^func Fuzz\w+' "$$file" | cut -d' ' -f2); do \
			echo -e "\n....Fuzzing $$name ($$(dirname $$file)) for $(FUZZTIME)...."; \
			LOG_IGNORE=1 go test -run='^$$' -fuzz="^$$name$$" -fuzztime=$(FUZZTIME) ./$$(dirname $$file); \
		done; \
	done

# Fast iteration: build + install binary + restart all services.
# Microvm artifacts are reinstalled when they already exist on disk — the rule's
# input timestamps drive a rebuild only if anything actually changed. On a fresh
# checkout (no build/microvm/vmlinuz yet) the install step is skipped; run
# `make install-microvm` explicitly the first time. They go in before the
# preflight, which checks the microvm firmware.
deploy: build
	sudo install -m 755 bin/spx /usr/local/bin/spx
	@if [ -f $(MICROVM_OUT_DIR)/vmlinuz ]; then \
		$(MAKE) install-microvm; \
	else \
		echo "[deploy] microvm artifacts absent — run 'make install-microvm' for first-time setup"; \
	fi
	@if [ "$${SKIP_ASSET_PREFLIGHT:-}" != "1" ]; then \
		if ! sudo /usr/local/bin/spx admin preflight; then \
			echo ""; \
			echo "[deploy] host-asset preflight failed: this node's helper scripts, sudoers"; \
			echo "  grants, QEMU or microvm firmware don't match the binary just installed. Remediate with:"; \
			echo "    scripts/update-nodes.sh   (from the mulga root)"; \
			echo "  or:"; \
			echo "    make reinstall"; \
			echo "  Set SKIP_ASSET_PREFLIGHT=1 to bypass for a deliberate binary-only push."; \
			exit 1; \
		fi; \
	else \
		echo "[deploy] SKIP_ASSET_PREFLIGHT=1 — skipping host-asset preflight"; \
	fi
	sudo systemctl daemon-reload
	sudo systemctl restart spinifex.target

# Re-run setup.sh after changing systemd units, helper scripts, or logrotate config.
# Not needed for code-only changes — use deploy for those.
reinstall:
	scripts/dev-install.sh

clean:
	rm -f ./bin/$(GO_PROJECT_NAME)
	rm -rf spinifex/runtime/roles/spinifexui/frontend/dist

# On dnf hosts the runtime packages and repositories come from setup.sh, so the
# two cannot drift; nbdkit-devel, for building the plugin, is only in CodeReady.
install-system:
	@echo -e "\n....Installing system dependencies for $(ARCH)...."
	@pm=$$(INSTALL_SPINIFEX_LIB_ONLY=1 bash -c '. scripts/setup.sh && distro_pkg_manager') || exit 1; \
	if [ "$$pm" = dnf ]; then \
		INSTALL_SPINIFEX_LIB_ONLY=1 bash -c '. scripts/setup.sh && setup_sudo && detect_os && install_dnf_deps' && \
		sudo dnf config-manager --set-enabled ol10_codeready_builder && \
		sudo dnf install -y nbdkit-devel pkgconf-pkg-config make gcc openssh-clients git sudo; \
	else \
		sudo apt-get update && sudo DEBIAN_FRONTEND=noninteractive apt-get install -y \
			-o Dpkg::Options::="--force-confdef" \
			-o Dpkg::Options::="--force-confold" \
			nbdkit nbdkit-plugin-dev pkg-config qemu-system-x86 qemu-system-arm qemu-utils \
			ovmf qemu-efi-aarch64 \
			libvirt-daemon-system libvirt-clients libvirt-dev make gcc jq curl \
			iproute2 netcat-openbsd openssh-client wget git unzip sudo xz-utils file \
			ovn-central ovn-host openvswitch-switch dhcpcd-base; \
	fi

install-go:
	@echo -e "\n....Installing Go 1.27.3 for $(ARCH) ($(GO_ARCH))...."
	@if [ ! -d "/usr/local/go" ]; then \
		curl -L https://go.dev/dl/go1.27.3.linux-$(GO_ARCH).tar.gz | tar -C /usr/local -xz; \
	else \
		echo "Go already installed in /usr/local/go"; \
	fi
	@echo "Go version: $$(go version)"

install-aws:
	@echo -e "\n....Installing AWS CLI v2 for $(ARCH) ($(AWS_ARCH))...."
	@if ! command -v aws >/dev/null 2>&1; then \
		curl "https://awscli.amazonaws.com/awscli-exe-linux-$(AWS_ARCH).zip" -o "awscliv2.zip"; \
		unzip -o awscliv2.zip; \
		./aws/install; \
		rm -rf awscliv2.zip aws/; \
	else \
		echo "AWS CLI already installed"; \
	fi

quickinstall: install-system install-go install-aws
	@echo -e "\n✅ Quickinstall complete for $(ARCH)."
	@echo "   Please ensure /usr/local/go/bin is in your PATH."

lint:
	@echo "Running golangci-lint..."
	$(_Q)scripts/run-gate.sh golangci-lint golangci-lint run ./...
	@echo "  golangci-lint ok"

fix:
	golangci-lint run --fix ./...

govulncheck:
	@echo "Running govulncheck..."
	$(_Q)scripts/run-gate.sh govulncheck go tool govulncheck ./...
	@echo "  govulncheck ok"

# NilAway — advisory nil-panic analysis. Not in preflight due to false positives
nilaway:
	@echo "Running nilaway..."
	$(_Q)scripts/run-gate.sh nilaway go tool nilaway -include-pkgs=github.com/mulgadc/spinifex -exclude-test-files ./...
	@echo "  nilaway ok"

# Build release tarballs — use distro-ARCH for single arch, distro for both
distro: distro-amd64 distro-arm64
	@echo ""
	@echo "Distribution tarballs:"
	@ls -lh dist/*.tar.gz
	@echo ""
	@cat dist/*.sha256

distro-amd64:
	@echo "Building spinifex $(VERSION) linux/amd64..."
	@mkdir -p dist/
	docker buildx build \
		--platform linux/amd64 \
		--build-arg VERSION=$(VERSION) \
		-f build/Dockerfile.distro \
		--output type=local,dest=dist/amd64/ \
		../
	@if [ -f $(MICROVM_OUT_DIR)/vmlinuz ] && [ -f $(MICROVM_OUT_DIR)/initramfs.cpio.gz ] && [ -f $(MICROVM_OUT_DIR)/qboot.rom ] && [ -f $(QBOOT_SOURCE) ]; then \
		echo "[distro-amd64] staging microvm artifacts into tarball"; \
		mkdir -p dist/amd64/microvm; \
		cp $(MICROVM_OUT_DIR)/vmlinuz $(MICROVM_OUT_DIR)/initramfs.cpio.gz $(MICROVM_OUT_DIR)/qboot.rom $(MICROVM_OUT_DIR)/qboot.LICENSE dist/amd64/microvm/; \
		cp $(QBOOT_SOURCE) dist/; \
	else \
		echo "[distro-amd64] WARNING: microvm artifacts missing — tarball will not include them"; \
		echo "[distro-amd64]          run 'make build-microvm-image' before 'make distro-amd64'"; \
	fi
	./scripts/lib/stage-sqs-store-microvm.sh $(SQS_STORE_MICROVM_OUT_DIR) dist/amd64
	./scripts/lib/stage-sqs-store-microvm.sh $(SQS_WORKER_MICROVM_OUT_DIR) dist/amd64 sqs-worker
	tar -czf dist/spinifex-$(VERSION)-linux-amd64.tar.gz -C dist/amd64 .
	sha256sum dist/spinifex-$(VERSION)-linux-amd64.tar.gz > dist/spinifex-$(VERSION)-linux-amd64.tar.gz.sha256

distro-arm64:
	@echo "Building spinifex $(VERSION) linux/arm64..."
	@mkdir -p dist/
	docker buildx build \
		--platform linux/arm64 \
		--build-arg VERSION=$(VERSION) \
		-f build/Dockerfile.distro \
		--output type=local,dest=dist/arm64/ \
		../
	tar -czf dist/spinifex-$(VERSION)-linux-arm64.tar.gz -C dist/arm64 .
	sha256sum dist/spinifex-$(VERSION)-linux-arm64.tar.gz > dist/spinifex-$(VERSION)-linux-arm64.tar.gz.sha256

distro-clean:
	rm -rf dist/

.PHONY: test-package-check build build-ui build-installer build-lb-agent build-ecs-agent build-system-image build-eks-node-image import-eks-node-image publish-eks-node-image build-ecs-node-image import-ecs-node-image build-rds-postgres-image import-rds-postgres-image build-rds-mariadb-image import-rds-mariadb-image build-microvm-image build-sqs-store-microvm-image build-sqs-worker-microvm-image install-microvm install-sqs-store-microvm install-sqs-worker-microvm go_build preflight test test-cover test-race diff-coverage bench fuzz test-actions test-images test-build-scripts test-harness test-integration generate-aws-model-coverage aws-model-coverage test-segscan-oracle \
	deploy reinstall clean \
	install-system install-go install-aws quickinstall \
	lint fix govulncheck nilaway \
	distro distro-amd64 distro-arm64 distro-clean
