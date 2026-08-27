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
	cd spinifex/services/spinifexui/frontend && pnpm build

# GO commands
VERSION ?= $(shell git describe --tags --always --dirty)
COMMIT  ?= $(shell git rev-parse --short HEAD)
LDFLAGS := -s -w -X github.com/mulgadc/spinifex/cmd/spinifex/cmd.Version=$(VERSION) -X github.com/mulgadc/spinifex/cmd/spinifex/cmd.Commit=$(COMMIT)

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
	GOFIPS140=v1.0.0 go build -ldflags "-s -w" -o ./bin/spx-loadgen ./cmd/spx-loadgen

build-system-image: ## Build a system image from manifest (use IMAGE=lb or IMAGE=eks-node)
ifndef IMAGE
	$(error IMAGE is required. Usage: make build-system-image IMAGE=lb)
endif
	@if [ -f scripts/images/$(IMAGE).conf ]; then \
		./scripts/build-system-image.sh scripts/images/$(IMAGE).conf $(if $(IMPORT),--import); \
	elif [ -f scripts/images/$(IMAGE)/manifest.conf ]; then \
		./scripts/build-system-image.sh scripts/images/$(IMAGE)/manifest.conf $(if $(IMPORT),--import); \
	elif ./scripts/mkosi-build.sh --known-image $(IMAGE) >/dev/null 2>&1; then \
		if [ -n "$(IMPORT)" ]; then \
			echo "ERROR: IMPORT=1 has no mkosi equivalent — import with 'spx admin images import' on the node"; \
			exit 1; \
		fi; \
		./scripts/mkosi-build.sh --image $(IMAGE); \
	else \
		echo "ERROR: $(IMAGE) is neither a manifest under scripts/images/ nor a known mkosi image"; \
		exit 1; \
	fi

build-eks-node-image: ## Build the unified eks-node AMI (K3s server+agent; role at first boot; IMPORT=1 to register)
	$(MAKE) build-system-image IMAGE=eks-node

import-eks-node-image: ## Build + register the eks-node AMI (requires a running cluster)
	$(MAKE) build-system-image IMAGE=eks-node IMPORT=1

publish-eks-node-image: ## Build + publish the eks-node AMI to Cloudflare R2 (needs R2_ENDPOINT + AWS_* env)
	./scripts/publish-system-image.sh scripts/images/eks-node/manifest.conf --build

build-ecs-node-image: ## Build the spinifex-ecs-node AMI (Alpine + containerd + ecs-agent; IMPORT=1 to register)
	$(MAKE) build-system-image IMAGE=ecs-agent

import-ecs-node-image: ## Build + register the ecs-node AMI (requires a running cluster)
	$(MAKE) build-system-image IMAGE=ecs-agent IMPORT=1

# Same entry point as every other image; build-system-image dispatches to mkosi
# because these two have no manifest under scripts/images/ any more. The
# artefact is images/output/<image>.raw, imported with `spx admin images
# import` on the node rather than via IMPORT=1.
build-rds-postgres-image: ## Build the spinifex-rds-postgres image (Ubuntu 26.04 + PostgreSQL 18 + rds-init)
	$(MAKE) build-system-image IMAGE=spinifex-rds-postgres

build-rds-mariadb-image: ## Build the spinifex-rds-mariadb image (Ubuntu 26.04 + MariaDB 11.8 + rds-init)
	$(MAKE) build-system-image IMAGE=spinifex-rds-mariadb

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

build-microvm-image: $(MICROVM_ARTIFACTS) ## Build microVM kernel + initramfs (incremental — skips when up to date)

install-microvm: $(MICROVM_ARTIFACTS) ## Install microVM artifacts to /usr/share/spinifex/microvm/
	sudo install -d /usr/share/spinifex/microvm
	sudo install -m 0644 $(MICROVM_OUT_DIR)/vmlinuz /usr/share/spinifex/microvm/vmlinuz
	sudo install -m 0644 $(MICROVM_OUT_DIR)/initramfs.cpio.gz /usr/share/spinifex/microvm/initramfs.cpio.gz

# the pre-commit gate: manifest checks, lint, vuln, and the unit and e2e-harness tiers.
# integration and race tests skipped to keep quick, they run in CI.
preflight:
	@$(MAKE) --no-print-directory QUIET=1 manifest-check manifest-lint lint govulncheck test-cover diff-coverage test-package-check test-harness test-build-scripts
	@echo -e "\n ✅ Preflight passed — safe to commit."

# Shell suites + shellcheck for build/scripts/, the systemd-unit helpers that
# ship on every node (unlike scripts/images/, kept in preflight: a wrong
# restart decision here is an outage, not asset churn).
test-build-scripts:
	@echo -e "\n....Running build/scripts/**/*_test.sh...."
	@for t in $$(find build/scripts -name '*_test.sh' | sort); do \
		echo "-- $$t"; \
		sh "$$t" || exit 1; \
	done
	@echo -e "\n....Running shellcheck over build/scripts/**/*.sh...."
	shellcheck -S warning $$(find build/scripts -name '*.sh' | sort)
	@echo "  test-build-scripts ok"

# E2E harness unit tests. Build-tagged `e2e` so they're skipped by the
# default `go test ./spinifex/...`. Runs with mocked AWS clients — no
# infrastructure required, safe to run in CI without a cluster.
test-harness:
	@echo -e "\n....Running e2e harness unit tests...."
	$(_Q)LOG_IGNORE=1 go test -tags=e2e -timeout 60s ./tests/e2e/harness/... $(_RACEQ)

# In-process integration tier: the real gateway router against embedded NATS
# JetStream, with only the daemon-side NATS subjects stubbed.
AWS_MODEL_CONFORMANCE_REPORT ?= $(CURDIR)/.cache/aws-model-conformance-report.txt
AWS_MODEL_CONFORMANCE_MODE ?= fail
AWS_MODEL_OPERATION_COVERAGE_REPORT ?= $(CURDIR)/docs/aws-model-operation-coverage.md
# -count=1 is load-bearing: the conformance report is written by the test binary,
# so a cached pass skips the run, leaves no report, and the cat below fails the
# target. It also keeps the conformance gate honest — a cached result would mean
# the check never ran against this commit.
test-integration:
	@echo -e "\n....Running in-process integration tests...."
	$(_Q)LOG_IGNORE=1 AWS_MODEL_CONFORMANCE_MODE=$(AWS_MODEL_CONFORMANCE_MODE) AWS_MODEL_CONFORMANCE_REPORT=$(AWS_MODEL_CONFORMANCE_REPORT) go test -count=1 -tags=integration -timeout 60s ./tests/integration/... $(_RACEQ)
	@cat $(AWS_MODEL_CONFORMANCE_REPORT)
	@$(MAKE) --no-print-directory generate-aws-model-coverage
	@echo "AWS operation coverage: $(AWS_MODEL_OPERATION_COVERAGE_REPORT)"

generate-aws-model-coverage:
	@mkdir -p $(dir $(AWS_MODEL_OPERATION_COVERAGE_REPORT))
	@go run ./cmd/aws-model-coverage > $(AWS_MODEL_OPERATION_COVERAGE_REPORT)

aws-model-coverage: generate-aws-model-coverage
	@cat $(AWS_MODEL_OPERATION_COVERAGE_REPORT)

# Segscan storage oracle: needs the mulga umbrella repo's scripts/segscan
# checked out alongside spinifex (see spinifex/testutil/segscanoracle), which
# is not the default local or CI layout, so this is a separate target from
# test-integration rather than folded into it. Skips itself when segscan's
# source isn't found.
test-segscan-oracle:
	@echo -e "\n....Running segscan storage oracle test...."
	$(_Q)LOG_IGNORE=1 go test -tags=integration,segscanoracle -timeout 120s ./tests/integration/... -run TestSegscanOracle $(_RACEQ)

# Validate docs/service-interfaces.yaml. Schema check + cross-reference
# of services/suites/fixtures + on-disk path existence.
manifest-check:
	@echo -e "\n....Checking service-interfaces.yaml...."
	@go run ./tests/e2e/manifest-check/cmd/manifest-check -repo-root . -manifest docs/service-interfaces.yaml

# Drift guards: direct-create fixture lint + NATS subject lint,
# ratcheted against tests/e2e/manifest-lint/baseline.txt. Fails only on NEW
# drift beyond the baseline.
manifest-lint:
	@echo -e "\n....Linting manifest drift (fixtures + subjects)...."
	@go run ./tests/e2e/manifest-lint/cmd/manifest-lint -repo-root .

# Accept current drift into the baseline. Run after an intentional change.
manifest-lint-update:
	@go run ./tests/e2e/manifest-lint/cmd/manifest-lint -repo-root . -update

# Run unit tests
test:
	@echo -e "\n....Running tests for $(GO_PROJECT_NAME)...."
	LOG_IGNORE=1 go test -timeout 180s ./spinifex/... ./cmd/... ./internal/...

# Empty locally, where reusing a cached result between runs is the point.
# CI passes -count=1 so a green run means the suite executed against that commit
# rather than replaying results the persisted build cache carried over.
GOTESTFLAGS ?=

# Run unit tests with coverage profile
COVERPROFILE ?= coverage.out
test-cover:
	@echo -e "\n....Running tests with coverage for $(GO_PROJECT_NAME)...."
	$(_Q)LOG_IGNORE=1 go test $(GOTESTFLAGS) -timeout 180s -coverprofile=$(COVERPROFILE) -covermode=atomic ./spinifex/... ./cmd/... ./internal/... $(_COVQ)
	@scripts/check-coverage.sh $(COVERPROFILE) $(QUIET)

# Run unit tests with race detector
test-race:
	@echo -e "\n....Running tests with race detector for $(GO_PROJECT_NAME)...."
	$(_Q)LOG_IGNORE=1 go test $(GOTESTFLAGS) -race -timeout 300s ./spinifex/... ./cmd/... ./internal/... $(_RACEQ)

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

# Fast iteration: build + install binary + restart all services.
# Microvm artifacts are reinstalled when they already exist on disk — the rule's
# input timestamps drive a rebuild only if anything actually changed. On a fresh
# checkout (no build/microvm/vmlinuz yet) the install step is skipped; run
# `make install-microvm` explicitly the first time.
deploy: build
	sudo install -m 755 bin/spx /usr/local/bin/spx
	@if [ "$${SKIP_ASSET_PREFLIGHT:-}" != "1" ]; then \
		if ! sudo /usr/local/bin/spx admin preflight; then \
			echo ""; \
			echo "[deploy] host-asset preflight failed: this node's helper scripts or sudoers"; \
			echo "  grants don't match the binary just installed. Remediate with:"; \
			echo "    scripts/update-nodes.sh   (from the mulga root)"; \
			echo "  or:"; \
			echo "    make reinstall"; \
			echo "  Set SKIP_ASSET_PREFLIGHT=1 to bypass for a deliberate binary-only push."; \
			exit 1; \
		fi; \
	else \
		echo "[deploy] SKIP_ASSET_PREFLIGHT=1 — skipping host-asset preflight"; \
	fi
	@if [ -f $(MICROVM_OUT_DIR)/vmlinuz ]; then \
		$(MAKE) install-microvm; \
	else \
		echo "[deploy] microvm artifacts absent — run 'make install-microvm' for first-time setup"; \
	fi
	sudo systemctl daemon-reload
	sudo systemctl restart spinifex.target

# Re-run setup.sh after changing systemd units, helper scripts, or logrotate config.
# Not needed for code-only changes — use deploy for those.
reinstall:
	scripts/dev-install.sh

clean:
	rm -f ./bin/$(GO_PROJECT_NAME)
	rm -rf spinifex/services/spinifexui/frontend/dist

install-system:
	@echo -e "\n....Installing system dependencies for $(ARCH)...."
	sudo apt-get update && sudo DEBIAN_FRONTEND=noninteractive apt-get install -y \
		-o Dpkg::Options::="--force-confdef" \
		-o Dpkg::Options::="--force-confold" \
		nbdkit nbdkit-plugin-dev pkg-config qemu-system-x86 qemu-system-arm qemu-utils \
		ovmf qemu-efi-aarch64 \
		libvirt-daemon-system libvirt-clients libvirt-dev make gcc jq curl \
		iproute2 netcat-openbsd openssh-client wget git unzip sudo xz-utils file \
		ovn-central ovn-host openvswitch-switch dhcpcd-base

install-go:
	@echo -e "\n....Installing Go 1.27.0 for $(ARCH) ($(GO_ARCH))...."
	@if [ ! -d "/usr/local/go" ]; then \
		curl -L https://go.dev/dl/go1.27.0.linux-$(GO_ARCH).tar.gz | tar -C /usr/local -xz; \
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
	@if [ -f $(MICROVM_OUT_DIR)/vmlinuz ] && [ -f $(MICROVM_OUT_DIR)/initramfs.cpio.gz ]; then \
		echo "[distro-amd64] staging microvm artifacts into tarball"; \
		mkdir -p dist/amd64/microvm; \
		cp $(MICROVM_OUT_DIR)/vmlinuz $(MICROVM_OUT_DIR)/initramfs.cpio.gz dist/amd64/microvm/; \
	else \
		echo "[distro-amd64] WARNING: microvm artifacts missing — tarball will not include them"; \
		echo "[distro-amd64]          run 'make build-microvm-image' before 'make distro-amd64'"; \
	fi
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

.PHONY: test-package-check build build-ui build-installer build-lb-agent build-ecs-agent build-system-image build-eks-node-image import-eks-node-image publish-eks-node-image build-ecs-node-image import-ecs-node-image build-rds-postgres-image build-rds-mariadb-image build-microvm-image install-microvm go_build preflight test test-cover test-race diff-coverage bench test-actions test-images test-build-scripts test-harness test-integration generate-aws-model-coverage aws-model-coverage test-segscan-oracle manifest-check manifest-lint manifest-lint-update \
	deploy reinstall clean \
	install-system install-go install-aws quickinstall \
	lint fix govulncheck nilaway \
	distro distro-amd64 distro-arm64 distro-clean
