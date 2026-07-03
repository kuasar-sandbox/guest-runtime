# guest-runtime — guest runtime image and native dependency builder.
#
# This repo owns guest runtime artifacts:
#   sandbox-runtime.erofs     virtio-pmem/DAX guest runtime image
#   native-deps/bin/*         vmlinux, cloud-hypervisor, mkfs.erofs, envd
#
# The sandbox-init binary is produced by the sibling sandboxer repo. This
# Makefile consumes ../sandboxer/bin/$(TARGET_ARCH)/sandbox-init and injects the
# guest payload needed by e2b/build flows into one runtime image.

SHELL := /bin/bash

.PHONY: all build sandbox-init sandbox-runtime native-deps test clean help

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

SANDBOXER_DIR ?= ../sandboxer
ACCELERATOR_DIR ?= ../accelerator
SANDBOX_INIT  ?= $(SANDBOXER_DIR)/$(BINDIR)/sandbox-init
ENVD          ?= native-deps/$(BINDIR)/envd
FLATTEN_CTL   ?= $(ACCELERATOR_DIR)/$(BINDIR)/flatten-ctl

# mkfs.erofs lookup chain (in priority order):
#   PATH → this repo's $(BINDIR)/ → this repo's bin/ symlink →
#   native-deps/bin/$(TARGET_ARCH)/ → native-deps/bin/ symlink
MKFS_EROFS ?= $(shell \
    command -v mkfs.erofs 2>/dev/null \
    || ( [ -x $(BINDIR)/mkfs.erofs ] && echo $(BINDIR)/mkfs.erofs ) \
    || ( [ -x bin/mkfs.erofs ] && echo bin/mkfs.erofs ) \
    || ( [ -x native-deps/$(BINDIR)/mkfs.erofs ] && echo native-deps/$(BINDIR)/mkfs.erofs ) \
    || ( [ -x native-deps/bin/mkfs.erofs ] && echo native-deps/bin/mkfs.erofs ))
MKFS_GUEST ?= $(MKFS_EROFS)

define link_bin
@if [ "$(HOST_ARCH)" = "$(TARGET_ARCH)" ]; then \
   mkdir -p bin && ln -sfn $(TARGET_ARCH)/$(1) bin/$(1); \
 fi
endef

# ---------------------------------------------------------------------------
# Targets
# ---------------------------------------------------------------------------
all: build

build: sandbox-runtime

native-deps:
	$(MAKE) -C native-deps build

sandbox-init:
	$(MAKE) -C $(SANDBOXER_DIR) sandbox-init

envd:
	$(MAKE) -C native-deps envd

flatten-ctl:
	$(MAKE) -C $(ACCELERATOR_DIR) flatten-ctl

# Pack sandbox-init into the guest "/" image (virtio-pmem, DAX, read-only,
# shared across sandboxes via host page cache). envd, flatten-ctl, and mkfs.erofs
# are projected into application roots by sandbox-init through
# /opt/sandbox-runtime/bin.
sandbox-runtime:
	@[ -n "$(MKFS_EROFS)" ] || { echo "mkfs.erofs not found — build it with \`make native-deps\` or set MKFS_EROFS=<path>" >&2; exit 1; }
	@[ -x "$(SANDBOX_INIT)" ] || $(MAKE) sandbox-init
	@[ -x "$(ENVD)" ] || $(MAKE) envd
	@[ -x "$(FLATTEN_CTL)" ] || $(MAKE) flatten-ctl
	@[ -x "$(MKFS_GUEST)" ] || { echo "guest mkfs.erofs not found — set MKFS_GUEST=<path>" >&2; exit 1; }
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
	cp "$(MKFS_GUEST)" $(BUILD_DIR)/sandbox-runtime/opt/sandbox-runtime/bin/mkfs.erofs
	chmod 0755 $(BUILD_DIR)/sandbox-runtime/opt/sandbox-runtime/bin/envd \
	           $(BUILD_DIR)/sandbox-runtime/opt/sandbox-runtime/bin/flatten-ctl \
	           $(BUILD_DIR)/sandbox-runtime/opt/sandbox-runtime/bin/mkfs.erofs
	rm -f $(BINDIR)/sandbox-runtime.erofs
	"$(MKFS_EROFS)" \
	    -Ededupe \
	    --chunksize=4096 \
	    --all-root \
	    -T0 \
	    -b4096 \
	    -x-1 \
	    -U 00000000-0000-0000-0000-000000000000 \
	    $(BINDIR)/sandbox-runtime.erofs $(BUILD_DIR)/sandbox-runtime 2>/dev/null
	@# virtio-pmem requires 2 MiB-aligned backing. EROFS self-describes its
	@# extent in the superblock so sparse padding is invisible to mount.
	@actual=$$(stat -c %s $(BINDIR)/sandbox-runtime.erofs); \
	 aligned=$$(( ($$actual + 2097151) / 2097152 * 2097152 )); \
	 [ "$$aligned" = "$$actual" ] || truncate -s $$aligned $(BINDIR)/sandbox-runtime.erofs
	$(call link_bin,sandbox-runtime.erofs)
	@echo "==> built $(BINDIR)/sandbox-runtime.erofs"

test:
	$(MAKE) -C native-deps test

clean:
	rm -rf bin build
	$(MAKE) -C native-deps clean

help:
	@echo "guest-runtime. Targets:"
	@echo "  build              build sandbox-runtime.erofs"
	@echo "  sandbox-runtime    pack sandboxer sandbox-init into guest erofs"
	@echo "  sandbox-init       delegate to ../sandboxer sandbox-init"
	@echo "  envd               build e2b guest agent"
	@echo "  flatten-ctl        delegate to ../accelerator flatten-ctl"
	@echo "  native-deps        build vmlinux / cloud-hypervisor / mkfs.erofs / envd"
	@echo "  test / clean"
	@echo "  TARGET_ARCH        x86_64 (default) | aarch64"
