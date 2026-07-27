PKG         ?= ./cert/... ./lb/... ./eks/... ./ecs/... ./iam/... ./ecr/... ./bedrock/... ./multinode/... ./baremetal/... ./reboot/... ./tofu/...
ARTIFACT_DIR ?= /tmp/spinifex-e2e-artifacts
TIMEOUT     ?= 30m
MODE        ?= single
# RUN restricts the run to tests matching a regex, for packages holding several
# independent long-running suites. Empty means every test in PKG, so this is a
# no-op unless asked for. Needed where one package's tests are not
# interchangeable: storagegrowth holds workloads that are each driven with their
# own environment and whose assertions are deliberately hostile, so running a
# sibling by accident reports a failure that says nothing about the run.
RUN         ?=
# GOTESTSUM=1 emits JUnit XML + JSON streams for CI. Suite tag derives from the
# first path component of PKG so per-suite targets get distinct artifact names.
GOTESTSUM   ?=
SUITE_TAG   ?= $(notdir $(patsubst %/...,%,$(firstword $(PKG))))

# Pre-built test binaries — see mulga-siv-80. `e2e-build` cross-compiles each
# named suite into BIN_DIR/<suite>.test so CI can ship binaries (not source +
# go toolchain) to the remote VM.
BIN_DIR      ?= $(CURDIR)/_bin
SUITES_BUILD ?= cert lb eks ecs single iam ecr bedrock gpu natuplink multinode storagegrowth partialblock

.PHONY: e2e e2e-cert e2e-lb e2e-eks e2e-ecs e2e-single e2e-iam e2e-ecr e2e-bedrock e2e-multinode e2e-bm e2e-reboot e2e-tofu e2e-gpu e2e-natuplink e2e-storagegrowth e2e-partialblock e2e-all clean e2e-build

RUN_FLAG = $(if $(RUN),-run '$(RUN)',)

ifeq ($(GOTESTSUM),1)
E2E_CMD = gotestsum \
	--junitfile $(ARTIFACT_DIR)/junit-$(SUITE_TAG).xml \
	--jsonfile  $(ARTIFACT_DIR)/gotest-$(SUITE_TAG).json \
	--format=testname \
	-- -tags=e2e -timeout=$(TIMEOUT) $(RUN_FLAG) $(PKG)
else
E2E_CMD = go test -tags=e2e -v -timeout=$(TIMEOUT) $(RUN_FLAG) $(PKG)
endif

e2e:
	@mkdir -p $(ARTIFACT_DIR)
	SPINIFEX_E2E=1 \
		SPINIFEX_MODE=$(MODE) \
		ARTIFACT_DIR=$(ARTIFACT_DIR) \
		$(E2E_CMD)

e2e-cert:       ; $(MAKE) e2e PKG=./cert/...
e2e-lb:         ; $(MAKE) e2e PKG=./lb/...
e2e-eks:        ; $(MAKE) e2e PKG=./eks/...
e2e-ecs:        ; $(MAKE) e2e PKG=./ecs/...
e2e-single:     ; $(MAKE) e2e PKG=./single/...
e2e-iam:        ; $(MAKE) e2e PKG=./iam/...
e2e-ecr:        ; $(MAKE) e2e PKG=./ecr/...
e2e-bedrock:    ; $(MAKE) e2e PKG=./bedrock/...
e2e-multinode:  ; $(MAKE) e2e PKG=./multinode/... MODE=multinode
e2e-bm:         ; $(MAKE) e2e PKG=./baremetal/... MODE=baremetal
e2e-reboot:     ; $(MAKE) e2e PKG=./reboot/...
e2e-tofu:       ; $(MAKE) e2e PKG=./tofu/...
e2e-gpu:        ; $(MAKE) e2e PKG=./gpu/...
e2e-natuplink:  ; $(MAKE) e2e PKG=./natuplink/...
e2e-storagegrowth: ; $(MAKE) e2e PKG=./storagegrowth/...
e2e-partialblock:  ; $(MAKE) e2e PKG=./partialblock/...
e2e-all:        ; $(MAKE) e2e

e2e-build:
	@mkdir -p $(BIN_DIR)
	@for s in $(SUITES_BUILD); do \
	    echo "==> building $$s.test"; \
	    go test -c -tags=e2e -o $(BIN_DIR)/$$s.test ./$$s/ || exit 1; \
	done

clean:
	rm -rf $(ARTIFACT_DIR) $(BIN_DIR)
