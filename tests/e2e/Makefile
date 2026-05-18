PKG         ?= ./cert/... ./lb/... ./multinode/... ./baremetal/... ./reboot/... ./tofu/...
ARTIFACT_DIR ?= /tmp/spinifex-e2e-artifacts
TIMEOUT     ?= 30m
MODE        ?= single
# GOTESTSUM=1 emits JUnit XML + JSON streams for CI. Suite tag derives from the
# first path component of PKG so per-suite targets get distinct artifact names.
GOTESTSUM   ?=
SUITE_TAG   ?= $(notdir $(patsubst %/...,%,$(firstword $(PKG))))

.PHONY: e2e e2e-cert e2e-lb e2e-multinode e2e-bm e2e-reboot e2e-tofu e2e-all clean

ifeq ($(GOTESTSUM),1)
E2E_CMD = gotestsum \
	--junitfile $(ARTIFACT_DIR)/junit-$(SUITE_TAG).xml \
	--jsonfile  $(ARTIFACT_DIR)/gotest-$(SUITE_TAG).json \
	--format=testname \
	-- -tags=e2e -timeout=$(TIMEOUT) $(PKG)
else
E2E_CMD = go test -tags=e2e -v -timeout=$(TIMEOUT) $(PKG)
endif

e2e:
	@mkdir -p $(ARTIFACT_DIR)
	SPINIFEX_E2E=1 \
		SPINIFEX_MODE=$(MODE) \
		ARTIFACT_DIR=$(ARTIFACT_DIR) \
		$(E2E_CMD)

e2e-cert:       ; $(MAKE) e2e PKG=./cert/...
e2e-lb:         ; $(MAKE) e2e PKG=./lb/...
e2e-multinode:  ; $(MAKE) e2e PKG=./multinode/...
e2e-bm:         ; $(MAKE) e2e PKG=./baremetal/... MODE=baremetal
e2e-reboot:     ; $(MAKE) e2e PKG=./reboot/...
e2e-tofu:       ; $(MAKE) e2e PKG=./tofu/...
e2e-all:        ; $(MAKE) e2e

clean:
	rm -rf $(ARTIFACT_DIR)
