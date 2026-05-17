PKG         ?= ./cert/... ./lb/... ./multinode/... ./baremetal/... ./reboot/... ./tofu/...
ARTIFACT_DIR ?= /tmp/spinifex-e2e-artifacts
TIMEOUT     ?= 30m
MODE        ?= single

.PHONY: e2e e2e-cert e2e-lb e2e-multinode e2e-bm e2e-reboot e2e-tofu e2e-all clean

e2e:
	@mkdir -p $(ARTIFACT_DIR)
	SPINIFEX_E2E=1 \
		SPINIFEX_MODE=$(MODE) \
		ARTIFACT_DIR=$(ARTIFACT_DIR) \
		go test -tags=e2e -v -timeout=$(TIMEOUT) $(PKG)

e2e-cert:       ; $(MAKE) e2e PKG=./cert/...
e2e-lb:         ; $(MAKE) e2e PKG=./lb/...
e2e-multinode:  ; $(MAKE) e2e PKG=./multinode/...
e2e-bm:         ; $(MAKE) e2e PKG=./baremetal/... MODE=baremetal
e2e-reboot:     ; $(MAKE) e2e PKG=./reboot/...
e2e-tofu:       ; $(MAKE) e2e PKG=./tofu/...
e2e-all:        ; $(MAKE) e2e

clean:
	rm -rf $(ARTIFACT_DIR)
