# guest-runtime — guest runtime image and native dependency builder.
#
# This repo builds guest runtime artifacts:
#   flatten-ctl               OCI/dir -> deterministic EROFS builder
#   sandbox-runtime.bundle    virtio-pmem/DAX guest runtime image
#   native-deps/bin/*         vmlinux, mkfs.erofs, fsck.erofs, envd
#
# The sandbox-init binary is produced by the sibling sandboxer repo. This
# Makefile consumes ../sandboxer/bin/$(TARGET_ARCH)/sandbox-init and injects the
# guest payload needed by e2b/build flows into one runtime image. flatten-ctl is
# a CLI in this repo; its reusable implementation packages live in accelerator.

SHELL := /bin/bash

.PHONY: all build flatten-ctl sandbox-init sandbox-runtime native-deps erofs envd vmlinux test vet test-e2e release-runtime release-vmlinux test-release clean help

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

SANDBOXER_DIR ?= ../sandboxer
SANDBOX_INIT  ?= $(SANDBOXER_DIR)/$(BINDIR)/sandbox-init
ENVD          ?= native-deps/$(BINDIR)/envd
FLATTEN_CTL   ?= $(BINDIR)/flatten-ctl
STORE_CTL     ?= ../accelerator/$(BINDIR)/store-ctl
ZOT_BIN       ?= zot

# BUILD_MKFS_EROFS is the host executable that packs the raw runtime EROFS.
# GUEST_MKFS_EROFS is the target-arch static binary shipped inside the guest
# runtime image. Cross builds must keep them separate.
BUILD_MKFS_EROFS ?= $(shell \
    command -v mkfs.erofs 2>/dev/null \
    || ( [ "$(HOST_ARCH)" = "$(TARGET_ARCH)" ] && [ -x native-deps/$(BINDIR)/mkfs.erofs ] && echo native-deps/$(BINDIR)/mkfs.erofs ) \
    || ( [ -x native-deps/bin/mkfs.erofs ] && echo native-deps/bin/mkfs.erofs ))
GUEST_MKFS_EROFS ?= native-deps/$(BINDIR)/mkfs.erofs

define link_bin
@if [ "$(HOST_ARCH)" = "$(TARGET_ARCH)" ]; then \
   mkdir -p bin && ln -sfn $(TARGET_ARCH)/$(1) bin/$(1); \
 fi
endef

# ---------------------------------------------------------------------------
# Targets
# ---------------------------------------------------------------------------
all: build

build: flatten-ctl sandbox-runtime

flatten-ctl:
	@mkdir -p $(BINDIR)
	GOOS=linux GOARCH=$(GO_ARCH) CGO_ENABLED=0 $(GO) build $(GO_BUILD_FLAGS) -o $(BINDIR)/flatten-ctl ./cmd/flatten-ctl
	$(call link_bin,flatten-ctl)

native-deps:
	$(MAKE) -C native-deps build

erofs:
	$(MAKE) -C native-deps erofs

sandbox-init:
	$(MAKE) -C $(SANDBOXER_DIR) sandbox-init

envd:
	$(MAKE) -C native-deps envd

vmlinux:
	$(MAKE) -C native-deps vmlinux

# Pack sandbox-init into the guest "/" image (virtio-pmem, DAX, read-only,
# shared across sandboxes via host page cache). envd, flatten-ctl, and mkfs.erofs
# are projected into application roots by sandbox-init through
# /opt/sandbox-runtime/bin.
sandbox-runtime:
	@[ -n "$(BUILD_MKFS_EROFS)" ] || { echo "host mkfs.erofs not found; install erofs-utils or set BUILD_MKFS_EROFS=<host-executable>" >&2; exit 1; }
	@[ -x "$(SANDBOX_INIT)" ] || $(MAKE) sandbox-init
	@[ -x "$(ENVD)" ] || $(MAKE) envd
	@[ -x "$(FLATTEN_CTL)" ] || $(MAKE) flatten-ctl
	@[ -x "$(GUEST_MKFS_EROFS)" ] || $(MAKE) erofs
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
	cp "$(GUEST_MKFS_EROFS)" $(BUILD_DIR)/sandbox-runtime/opt/sandbox-runtime/bin/mkfs.erofs
	chmod 0755 $(BUILD_DIR)/sandbox-runtime/opt/sandbox-runtime/bin/envd \
	           $(BUILD_DIR)/sandbox-runtime/opt/sandbox-runtime/bin/flatten-ctl \
	           $(BUILD_DIR)/sandbox-runtime/opt/sandbox-runtime/bin/mkfs.erofs
	mkdir -p $(BINDIR)
	rm -f $(BUILD_DIR)/sandbox-runtime.erofs $(BINDIR)/sandbox-runtime.bundle
	"$(BUILD_MKFS_EROFS)" \
	    -Ededupe \
	    --chunksize=4096 \
	    --all-root \
	    -T0 \
	    -b4096 \
	    -x-1 \
	    -U 00000000-0000-0000-0000-000000000000 \
	    $(BUILD_DIR)/sandbox-runtime.erofs $(BUILD_DIR)/sandbox-runtime 2>/dev/null
	$(GO) run ./cmd/runtime-bundle \
	    -input $(BUILD_DIR)/sandbox-runtime.erofs \
	    -output $(BINDIR)/sandbox-runtime.bundle
	$(call link_bin,sandbox-runtime.bundle)
	@echo "==> built $(BINDIR)/sandbox-runtime.bundle"

test:
	$(MAKE) -C native-deps test
	python3 -m py_compile scripts/guest-inspect.py
	CGO_ENABLED=0 $(GO) test ./...

vet:
	CGO_ENABLED=0 $(GO) vet ./...

test-e2e: flatten-ctl
	REQUIRE_GUEST_RUNTIME=1 FLATTEN_CTL="$(FLATTEN_CTL)" STORE_CTL="$(STORE_CTL)" ZOT_BIN="$(ZOT_BIN)" bash test/e2e/e2e_flatten.sh

RUNTIME_VERSION ?= runtime-v0.1.0
VMLINUX_VERSION ?= vmlinux-v0.1.0
SANDBOXER_VERSION ?=

release-runtime: sandbox-runtime
	@[[ "$(SANDBOXER_VERSION)" =~ ^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$$ ]] \
		|| { echo "SANDBOXER_VERSION must match vX.Y.Z" >&2; exit 1; }
	@{ \
		printf 'repository\trequested_ref\tresolved_sha\trole\n'; \
		printf 'kuasar-sandbox/accelerator\tHEAD\t%s\tdependency\n' "$$(git -C ../accelerator rev-parse HEAD)"; \
		printf 'kuasar-sandbox/connector\tHEAD\t%s\tdependency\n' "$$(git -C ../connector rev-parse HEAD)"; \
		printf 'kuasar-sandbox/guest-runtime\tHEAD\t%s\tprimary\n' "$$(git rev-parse HEAD)"; \
		printf 'kuasar-sandbox/sandboxer\t$(SANDBOXER_VERSION)\t%s\tdependency\n' "$$(git -C ../sandboxer rev-parse HEAD)"; \
	} > $(BUILD_DIR)/runtime-revisions.tsv
	rm -rf $(BUILD_DIR)/release-runtime-bundle
	SOURCE_DATE_EPOCH="$$(git show -s --format=%ct HEAD)" \
		bash scripts/release.sh package runtime "$(RUNTIME_VERSION)" "$(TARGET_ARCH)" \
		$(BUILD_DIR)/runtime-revisions.tsv $(BUILD_DIR)/release-runtime-bundle

release-vmlinux: vmlinux
	@printf 'repository\trequested_ref\tresolved_sha\trole\n' > $(BUILD_DIR)/vmlinux-revisions.tsv
	@printf 'kuasar-sandbox/guest-runtime\tHEAD\t%s\tprimary\n' "$$(git rev-parse HEAD)" >> $(BUILD_DIR)/vmlinux-revisions.tsv
	rm -rf $(BUILD_DIR)/release-vmlinux-bundle
	SOURCE_DATE_EPOCH="$$(git show -s --format=%ct HEAD)" \
		bash scripts/release.sh package vmlinux "$(VMLINUX_VERSION)" "$(TARGET_ARCH)" \
		$(BUILD_DIR)/vmlinux-revisions.tsv $(BUILD_DIR)/release-vmlinux-bundle

test-release:
	bash scripts/test-release.sh

clean:
	rm -rf bin build
	$(MAKE) -C native-deps clean

help:
	@echo "guest-runtime. Targets:"
	@echo "  build              build flatten-ctl + sandbox-runtime.bundle"
	@echo "  flatten-ctl        OCI/dir -> deterministic EROFS builder"
	@echo "  sandbox-runtime    pack sandboxer sandbox-init into guest erofs"
	@echo "  sandbox-init       delegate to ../sandboxer sandbox-init"
	@echo "  erofs              build target-arch guest mkfs.erofs"
	@echo "  envd               build e2b guest agent"
	@echo "  vmlinux            build the guest kernel"
	@echo "  native-deps        build vmlinux / mkfs.erofs / fsck.erofs / envd"
	@echo "  test               unit/static checks"
	@echo "  vet                Go static analysis"
	@echo "  test-e2e           full flatten-ctl registry/store e2e"
	@echo "  release-runtime    package runtime-vX.Y.Z (requires SANDBOXER_VERSION=vX.Y.Z)"
	@echo "  release-vmlinux    package vmlinux-vX.Y.Z"
	@echo "  test-release       test both independent release lines"
	@echo "  clean"
	@echo "  TARGET_ARCH        x86_64 (default) | aarch64"
