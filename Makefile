# guest-runtime — guest runtime image and native dependency builder.
#
# This repo builds guest runtime artifacts:
#   sandbox-init              guest PID 1
#   flatten-ctl               OCI/dir -> deterministic EROFS builder
#   sandbox-runtime.bundle    virtio-pmem/DAX guest runtime image
#   native-deps/bin/*         vmlinux, envd
#
# sandbox-init and flatten-ctl are built from this module. Their selected
# target binaries and envd are packed into one runtime image. The reusable
# image-flattening implementation packages live in accelerator.

SHELL := /bin/bash

.PHONY: all build flatten-ctl sandbox-init sandbox-runtime native-deps envd vmlinux test vet test-e2e-scripts release-runtime release-vmlinux test-release clean help

# ---------------------------------------------------------------------------
# Architecture selection (identical block across all kuasar-sandbox repos)
# ---------------------------------------------------------------------------
HOST_ARCH   := $(shell uname -m)
TARGET_ARCH ?= $(HOST_ARCH)
ifeq ($(TARGET_ARCH),amd64)
  override TARGET_ARCH := x86_64
endif
ifeq ($(TARGET_ARCH),arm64)
  override TARGET_ARCH := aarch64
endif
ifeq ($(TARGET_ARCH),x86_64)
  GO_ARCH := amd64
else ifeq ($(TARGET_ARCH),aarch64)
  GO_ARCH := arm64
else
  $(error unsupported TARGET_ARCH=$(TARGET_ARCH); supported: x86_64, aarch64)
endif
export TARGET_ARCH

# ---------------------------------------------------------------------------
# Build settings
# ---------------------------------------------------------------------------
BINDIR    := bin/$(TARGET_ARCH)
BUILD_DIR := build/$(TARGET_ARCH)
GO        := go
GO_BUILD_FLAGS := -trimpath

SANDBOX_INIT  ?= $(BINDIR)/sandbox-init
ENVD          ?= native-deps/bin/$(TARGET_ARCH)/envd
FLATTEN_CTL   ?= $(BINDIR)/flatten-ctl
STORE_CTL     ?= ../accelerator/$(BINDIR)/store-ctl

# BUILD_MKFS_EROFS is the host executable that packs the raw runtime EROFS.
# It is a build/test dependency only; flatten-ctl generates EROFS in Go.
# Cross builds must use an executable for the build host.
BUILD_MKFS_EROFS ?= $(shell command -v mkfs.erofs 2>/dev/null)

define link_bin
@if [ "$(HOST_ARCH)" = "$(TARGET_ARCH)" ]; then \
   mkdir -p bin && ln -sfn $(TARGET_ARCH)/$(1) bin/$(1); \
 fi
endef

# ---------------------------------------------------------------------------
# Targets
# ---------------------------------------------------------------------------
all: build

# Finish both requested Go rebuilds before inspecting or staging their output.
# A direct sandbox-runtime invocation accepts prebuilt SANDBOX_INIT/FLATTEN_CTL.
build: sandbox-init flatten-ctl
	$(MAKE) sandbox-runtime

flatten-ctl:
	@mkdir -p "$(BINDIR)"
	GOOS=linux GOARCH=$(GO_ARCH) CGO_ENABLED=0 $(GO) build $(GO_BUILD_FLAGS) -o "$(BINDIR)/flatten-ctl" ./cmd/flatten-ctl
	$(call link_bin,flatten-ctl)

native-deps:
	$(MAKE) -C native-deps build

sandbox-init:
	@mkdir -p "$(BINDIR)"
	GOOS=linux GOARCH=$(GO_ARCH) CGO_ENABLED=0 $(GO) build $(GO_BUILD_FLAGS) -ldflags '-s -w' -o "$(BINDIR)/sandbox-init" ./cmd/sandbox-init
	$(call link_bin,sandbox-init)

envd:
	$(MAKE) -C native-deps envd BINDIR="bin/$(TARGET_ARCH)"

vmlinux:
	$(MAKE) -C native-deps vmlinux

# Pack sandbox-init into the guest "/" image (virtio-pmem, DAX, read-only,
# shared across sandboxes via host page cache). envd and flatten-ctl
# are projected into application roots by sandbox-init through
# /opt/sandbox-runtime/bin.
sandbox-runtime:
	@[ -n "$(BUILD_MKFS_EROFS)" ] || { echo "host mkfs.erofs not found; install erofs-utils or set BUILD_MKFS_EROFS=<host-executable>" >&2; exit 1; }
	@[ -x "$(SANDBOX_INIT)" ] || $(MAKE) sandbox-init
	@[ -x "$(ENVD)" ] || $(MAKE) envd
	@[ -x "$(FLATTEN_CTL)" ] || $(MAKE) flatten-ctl
	@set -o pipefail; $(GO) version -m "$(FLATTEN_CTL)" | awk -F '\t' '\
	  $$2 == "dep" && $$3 == "github.com/kuasar-sandbox/go-erofs" && $$4 ~ /^v/ && $$5 ~ /^h1:/ { found++ } \
	  END { exit found != 1 }' || { echo "selected flatten-ctl lacks the linked Go EROFS generator; rebuild with make flatten-ctl or select a matching FLATTEN_CTL" >&2; exit 1; }
	rm -rf $(BUILD_DIR)/sandbox-runtime
	mkdir -p $(BUILD_DIR)/sandbox-runtime/sbin $(BUILD_DIR)/sandbox-runtime/proc \
	         $(BUILD_DIR)/sandbox-runtime/sys $(BUILD_DIR)/sandbox-runtime/dev \
	         $(BUILD_DIR)/sandbox-runtime/overlay/lower $(BUILD_DIR)/sandbox-runtime/overlay/upper \
	         $(BUILD_DIR)/sandbox-runtime/sysroot $(BUILD_DIR)/sandbox-runtime/opt/sandbox-runtime/bin
	@# Pre-baked mountpoints for boot.disks[] data disks (max 8, ordinals 0-7).
	mkdir -p $(BUILD_DIR)/sandbox-runtime/sysdisks/disk-{0..7}{,-lower,-upper}
	cp "$(SANDBOX_INIT)" $(BUILD_DIR)/sandbox-runtime/sbin/init
	chmod +x $(BUILD_DIR)/sandbox-runtime/sbin/init
	cp "$(ENVD)" $(BUILD_DIR)/sandbox-runtime/opt/sandbox-runtime/bin/envd
	cp "$(FLATTEN_CTL)" $(BUILD_DIR)/sandbox-runtime/opt/sandbox-runtime/bin/flatten-ctl
	chmod 0755 $(BUILD_DIR)/sandbox-runtime/opt/sandbox-runtime/bin/envd \
	           $(BUILD_DIR)/sandbox-runtime/opt/sandbox-runtime/bin/flatten-ctl
	mkdir -p "$(BINDIR)"
	rm -f $(BUILD_DIR)/sandbox-runtime.erofs "$(BINDIR)/sandbox-runtime.bundle"
	"$(BUILD_MKFS_EROFS)" \
	    -Ededupe \
	    --chunksize=4096 \
	    --all-root \
	    -T0 \
	    -b4096 \
	    -x-1 \
	    -U 00000000-0000-0000-0000-000000000000 \
	    $(BUILD_DIR)/sandbox-runtime.erofs $(BUILD_DIR)/sandbox-runtime 2>/dev/null
	GOOS=linux GOARCH=$(shell $(GO) env GOHOSTARCH) CGO_ENABLED=0 $(GO) run ./cmd/runtime-bundle \
	    -input $(BUILD_DIR)/sandbox-runtime.erofs \
	    -output "$(BINDIR)/sandbox-runtime.bundle"
	$(call link_bin,sandbox-runtime.bundle)
	@echo "==> built "$(BINDIR)/sandbox-runtime.bundle""

test:
	PYTHONDONTWRITEBYTECODE=1 python3 scripts/test-environment-tools.py
	$(MAKE) test-e2e-scripts
	$(MAKE) -C native-deps test
	PYTHONPYCACHEPREFIX="$(abspath $(BUILD_DIR)/python-cache)" \
		python3 -m py_compile scripts/guest-inspect.py
	CGO_ENABLED=0 $(GO) test ./...

vet:
	CGO_ENABLED=0 $(GO) vet ./...

test-e2e-scripts:
	bash -n test/e2e/cases/image.flatten.sh test/e2e/cases/image.registry.sh test/e2e/lib/common.sh
	PYTHONDONTWRITEBYTECODE=1 python3 test/e2e/test_scripts.py

RUNTIME_VERSION ?= runtime-v0.1.0
VMLINUX_VERSION ?= vmlinux-v0.1.0
KUASAR_PLATFORM_DIR ?= ../kuasar-sandbox
# Explicit dependency tags select Preview inputs. Otherwise use this module's
# declared release requirement without resolving the module graph.
go_requirement = $$(GOWORK=off $(GO) mod edit -json | \
	jq -er --arg path "github.com/kuasar-sandbox/$(1)" '.Require[] | select(.Path == $$path) | .Version')
ACCELERATOR_VERSION ?= $(call go_requirement,accelerator)

# Release assembly rebuilds both Go payloads from the selected source, never
# reusing leftover development binaries. The generic sandbox-runtime target remains a dev entry.
# Mixed release recipes run sub-makes with the jobserver on real builds. GNU
# Make executes recipes that directly reference $(MAKE) even under --dry-run.
release_make = $(MAKE)
release_recursive = $(if $(findstring n,$(filter-out -% %=%,$(firstword $(MAKEFLAGS)))),,+)

release-runtime:
	$(release_recursive)@set -e; accelerator_tag="$(ACCELERATOR_VERSION)"; \
	workspace="$$(python3 -B "$(KUASAR_PLATFORM_DIR)/ci/integration/go_workspace.py" \
		--root .. --owners guest-runtime --version "$(RUNTIME_VERSION)" --dependency-tag "accelerator=$$accelerator_tag")"; \
	$(release_make) sandbox-init flatten-ctl GOWORK="$$workspace" BINDIR="$(abspath $(BINDIR))"; \
	$(release_make) sandbox-runtime GOWORK="$$workspace" BINDIR="$(abspath $(BINDIR))" \
		FLATTEN_CTL="$(abspath $(BINDIR))/flatten-ctl" \
		SANDBOX_INIT="$(abspath $(BINDIR))/sandbox-init"; \
	rm -rf "$(BUILD_DIR)/release-runtime-bundle"; \
	GOWORK="$$workspace" RELEASE_BIN_DIR="$(abspath $(BINDIR))" \
	RELEASE_SOURCE_ROOT="$(CURDIR)" \
	RELEASE_ACCELERATOR_SOURCE_DIR="$(abspath ../accelerator)" \
	SOURCE_DATE_EPOCH="$$(git show -s --format=%ct HEAD)" \
	RELEASE_ACCELERATOR_VERSION="$$accelerator_tag" \
	RELEASE_ENVD_BIN="$(abspath $(ENVD))" \
	RELEASE_SANDBOX_INIT_BIN="$(abspath $(BINDIR))/sandbox-init" \
		bash scripts/release.sh package runtime "$(RUNTIME_VERSION)" "$(TARGET_ARCH)" \
		$(BUILD_DIR)/release-runtime-bundle

release-vmlinux: vmlinux
	rm -rf $(BUILD_DIR)/release-vmlinux-bundle
	SOURCE_DATE_EPOCH="$$(git show -s --format=%ct HEAD)" \
		bash scripts/release.sh package vmlinux "$(VMLINUX_VERSION)" "$(TARGET_ARCH)" \
		$(BUILD_DIR)/release-vmlinux-bundle

test-release:
	PYTHONDONTWRITEBYTECODE=1 python3 scripts/test-environment-tools.py
	bash scripts/test-release.sh

clean:
	rm -rf bin build
	$(MAKE) -C native-deps clean

help:
	@echo "guest-runtime. Targets:"
	@echo "  build              build sandbox-init + flatten-ctl + sandbox-runtime.bundle"
	@echo "  flatten-ctl        OCI/dir -> deterministic EROFS builder"
	@echo "  sandbox-runtime    pack guest payloads into guest erofs"
	@echo "  sandbox-init       build the guest PID 1 binary"
	@echo "  envd               build e2b guest agent"
	@echo "  vmlinux            build the guest kernel"
	@echo "  native-deps        build vmlinux / envd"
	@echo "  test               unit/static checks"
	@echo "  vet                Go static analysis"
	@echo "  test-e2e-scripts   fixture, assertion and lifecycle regressions (no Docker required)"
	@echo "  release-runtime    package runtime-vX.Y.Z"
	@echo "  release-vmlinux    package vmlinux-vX.Y.Z"
	@echo "  test-release       test both independent release lines"
	@echo "  clean"
	@echo "  TARGET_ARCH        x86_64 (default) | aarch64"
