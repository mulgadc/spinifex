GO_PROJECT_NAME := spx
SHELL := /bin/bash

# Detect architecture for cross-platform support
ARCH := $(shell uname -m)
ifeq ($(ARCH),x86_64)
  GO_ARCH := amd64
  AWS_ARCH := x86_64
  # On x86, we can run ARM VMs via emulation
  QEMU_PACKAGES := qemu-system-x86 qemu-system-arm
else ifeq ($(ARCH),aarch64)
  GO_ARCH := arm64
  AWS_ARCH := aarch64
  # On ARM, we can run x86 VMs via emulation
  QEMU_PACKAGES := qemu-system-aarch64 qemu-system-x86
else ifeq ($(ARCH),arm64)
  GO_ARCH := arm64
  AWS_ARCH := aarch64
  QEMU_PACKAGES := qemu-system-aarch64 qemu-system-x86
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

build: go_build build-installer build-lb-agent

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
	GOFIPS140=v1.0.0 go build $(GO_BUILD_MOD) -ldflags "$(LDFLAGS)" -o ./bin/$(GO_PROJECT_NAME) cmd/spinifex/main.go

build-installer:
	@echo -e "\n....Building spinifex-installer"
	GOFIPS140=v1.0.0 go build -ldflags "-s -w" -o ./bin/spinifex-installer cmd/installer/main.go

build-lb-agent:
	@echo -e "\n....Building lb-agent (static)"
	CGO_ENABLED=0 GOFIPS140=v1.0.0 go build -ldflags "-s -w" -o ./bin/lb-agent cmd/lb-agent/main.go

build-system-image: ## Build a system image from manifest (use IMAGE=lb or IMAGE=eks-server)
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

build-eks-server-image: ## Build the eks-server AMI (K3s control-plane variant; IMPORT=1 to register it)
	$(MAKE) build-system-image IMAGE=eks-server

import-eks-server-image: ## Build + register the eks-server AMI (requires a running cluster)
	$(MAKE) build-system-image IMAGE=eks-server IMPORT=1

build-eks-agent-image: ## Build the eks-agent AMI (K3s worker variant)
	$(MAKE) build-system-image IMAGE=eks-agent

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

go_run:
	@echo -e "\n....Running $(GO_PROJECT_NAME)...."
	$(GOPATH)/bin/$(GO_PROJECT_NAME)

# Preflight — runs the same checks as GitHub Actions (lint + vuln + tests).
# Use this before committing to catch CI failures locally.
preflight:
	@$(MAKE) --no-print-directory QUIET=1 manifest-check lint govulncheck test-cover diff-coverage test-race test-harness
	@echo -e "\n ✅ Preflight passed — safe to commit."

# E2E harness unit tests. Build-tagged `e2e` so they're skipped by the
# default `go test ./spinifex/...`. Runs with mocked AWS clients — no
# infrastructure required, safe to run in CI without a cluster.
test-harness:
	@echo -e "\n....Running e2e harness unit tests...."
	$(_Q)LOG_IGNORE=1 go test -tags=e2e -timeout 60s ./tests/e2e/harness/... $(_RACEQ)

# Validate docs/service-interfaces.yaml. Schema check + cross-reference
# of services/suites/fixtures + on-disk path existence. Subject content
# vs source is enforced separately in Bead 5 drift lint.
manifest-check:
	@echo -e "\n....Checking service-interfaces.yaml...."
	@go run ./tests/e2e/manifest-check/cmd/manifest-check -repo-root . -manifest docs/service-interfaces.yaml

# Run unit tests
test:
	@echo -e "\n....Running tests for $(GO_PROJECT_NAME)...."
	LOG_IGNORE=1 go test -timeout 120s ./spinifex/...

# Run unit tests with coverage profile
COVERPROFILE ?= coverage.out
test-cover:
	@echo -e "\n....Running tests with coverage for $(GO_PROJECT_NAME)...."
	$(_Q)LOG_IGNORE=1 go test -timeout 120s -coverprofile=$(COVERPROFILE) -covermode=atomic ./spinifex/... $(_COVQ)
	@scripts/check-coverage.sh $(COVERPROFILE) $(QUIET)

# Run unit tests with race detector
test-race:
	@echo -e "\n....Running tests with race detector for $(GO_PROJECT_NAME)...."
	$(_Q)LOG_IGNORE=1 go test -race -timeout 300s ./spinifex/... $(_RACEQ)

# Unit tests for in-repo GitHub Actions (e.g. .github/actions/e2e-analyze).
# Kept out of `test-cover` so coverage % isn't diluted by CI-only tooling.
test-actions:
	@echo -e "\n....Running action tests...."
	LOG_IGNORE=1 go test -timeout 60s ./.github/actions/...

# Check that new/changed code meets coverage threshold (runs tests first)
diff-coverage: test-cover
	@QUIET=$(QUIET) scripts/diff-coverage.sh $(COVERPROFILE)

bench:
	@echo -e "\n....Running benchmarks for $(GO_PROJECT_NAME)...."
	$(MAKE) easyjson
	LOG_IGNORE=1 go test -benchmem -run=. -bench=. ./...

run:
	$(MAKE) go_build
	$(MAKE) go_run

# Fast iteration: build + install binary + restart all services.
# Microvm artifacts are reinstalled when they already exist on disk — the rule's
# input timestamps drive a rebuild only if anything actually changed. On a fresh
# checkout (no build/microvm/vmlinuz yet) the install step is skipped; run
# `make install-microvm` explicitly the first time.
deploy: build
	sudo install -m 755 bin/spx /usr/local/bin/spx
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
	@echo "QEMU packages: $(QEMU_PACKAGES)"
	sudo apt-get update && sudo DEBIAN_FRONTEND=noninteractive apt-get install -y \
		-o Dpkg::Options::="--force-confdef" \
		-o Dpkg::Options::="--force-confold" \
		nbdkit nbdkit-plugin-dev pkg-config $(QEMU_PACKAGES) qemu-utils qemu-kvm \
		ovmf qemu-efi-aarch64 \
		libvirt-daemon-system libvirt-clients libvirt-dev make gcc jq curl \
		iproute2 netcat-openbsd openssh-client wget git unzip sudo xz-utils file \
		ovn-central ovn-host openvswitch-switch dhcpcd-base

install-go:
	@echo -e "\n....Installing Go 1.26.4 for $(ARCH) ($(GO_ARCH))...."
	@if [ ! -d "/usr/local/go" ]; then \
		curl -L https://go.dev/dl/go1.26.4.linux-$(GO_ARCH).tar.gz | tar -C /usr/local -xz; \
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
	@echo "   Installed: Go ($(GO_ARCH)), AWS CLI ($(AWS_ARCH)), QEMU ($(QEMU_PACKAGES))"

# Lint all Go code via golangci-lint (replaces check-format, vet, gosec, staticcheck)
lint:
	@echo "Running golangci-lint..."
	$(_Q)golangci-lint run ./...
	@echo "  golangci-lint ok"

# Auto-fix all linter issues that have fixers
fix:
	golangci-lint run --fix ./...

# Govulncheck — dependency vulnerability scanning (not covered by golangci-lint)
govulncheck:
	@echo "Running govulncheck..."
	$(_Q)go tool govulncheck ./...
	@echo "  govulncheck ok"

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

# Ansible dev lifecycle (experimental, parallel to dev-*.sh / reset-dev-env.sh).
# See scripts/ansible/README.md and docs/development/improvements/ansible-dev-lifecycle.md.
#
# Variable overrides: pass EXTRA_VARS="key=val key2=val2" to forward as
# `--extra-vars` to ansible-playbook. Plain `-e` on the make command line is
# make's own flag and does NOT reach ansible.
#   make ansible-dev-install EXTRA_VARS="spinifex_external_pool_start=192.168.1.90 spinifex_external_pool_end=192.168.1.99"
EXTRA_VARS ?=
_ANSIBLE_EXTRA = $(if $(strip $(EXTRA_VARS)),--extra-vars "$(EXTRA_VARS)",)

ansible-dev-preflight:
	cd scripts/ansible && ansible-playbook playbooks/dev-preflight.yml $(_ANSIBLE_EXTRA)

ansible-dev-teardown:
	cd scripts/ansible && ansible-playbook playbooks/dev-teardown.yml $(_ANSIBLE_EXTRA)

ansible-dev-install:
	cd scripts/ansible && ansible-playbook playbooks/dev-install.yml $(_ANSIBLE_EXTRA)

ansible-dev-reset:
	cd scripts/ansible && ansible-playbook playbooks/dev-reset.yml $(_ANSIBLE_EXTRA)

ansible-dev-deploy:
	cd scripts/ansible && ansible-playbook playbooks/dev-deploy.yml $(_ANSIBLE_EXTRA)

ansible-dev-status:
	cd scripts/ansible && ansible-playbook playbooks/dev-status.yml

ansible-dev-logs:
	cd scripts/ansible && ansible-playbook playbooks/dev-logs.yml

# Snapshot / restore require an explicit -e snapshot_name=<name>. The
# wrapper passes ANSIBLE_EXTRA through so devs can run
# `make ansible-dev-snapshot ANSIBLE_EXTRA='-e snapshot_name=before-merge'`.
ansible-dev-snapshot:
	cd scripts/ansible && ansible-playbook playbooks/dev-snapshot.yml $(ANSIBLE_EXTRA)

ansible-dev-restore:
	cd scripts/ansible && ansible-playbook playbooks/dev-restore.yml $(ANSIBLE_EXTRA)

ansible-dev-version:
	cd scripts/ansible && ansible-playbook playbooks/dev-version.yml

ansible-dev-vpc:
	cd scripts/ansible && ansible-playbook playbooks/dev-vpc.yml $(ANSIBLE_EXTRA)

# Multi-node cluster bootstrap against a tofu env (env1, env2, bryn, etc).
# Inventory is generated from scripts/tofu-cluster/envs/<env>.tfvars.
#   make ansible-cluster-bootstrap ENV=env1 POOL=192.168.1.150-192.168.1.159
ENV ?=
POOL ?=
ansible-cluster-bootstrap:
	@test -n "$(ENV)" || { echo "ENV=<env> required (e.g. env1, env2)"; exit 64; }
	cd scripts/ansible && CLUSTER_ENV=$(ENV) ansible-playbook -i inventory/tofu.py playbooks/cluster-bootstrap.yml \
		-e cluster_env=$(ENV) \
		$(if $(POOL),-e cluster_external_pool=$(POOL),) \
		$(_ANSIBLE_EXTRA)

.PHONY: build build-ui build-installer build-lb-agent build-system-image build-eks-server-image import-eks-server-image build-eks-agent-image build-microvm-image install-microvm go_build go_run preflight test test-cover test-race diff-coverage bench run test-actions test-harness manifest-check diff-coverage bench run \
	deploy reinstall clean \
	install-system install-go install-aws quickinstall \
	lint fix govulncheck \
	distro distro-amd64 distro-arm64 distro-clean \
	ansible-dev-preflight ansible-dev-teardown ansible-dev-install ansible-dev-reset ansible-dev-deploy ansible-dev-status ansible-dev-logs ansible-dev-snapshot ansible-dev-restore ansible-dev-version ansible-dev-vpc \
	ansible-cluster-bootstrap
