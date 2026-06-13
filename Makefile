# sandbox-deps — native dependency builds for the kuasar-sandbox platform.
#
# Produces the runtime artifacts the Go repos consume but don't link:
#   mkfs.erofs        (erofs-utils)        — used by sandbox-builder, sandbox-runtime
#   vmlinux           (guest kernel)       — used by sandbox-runtime
#   cloud-hypervisor  (patched Rust VMM)   — used by sandbox-runtime
#   envd              (e2b guest agent)    — injected into sandbox-runtime-e2b.erofs
#
# (librocksdb, sandbox-accelerator's CGO link dep, is built in that repo.)
#
# `make build` builds all four (cloud-hypervisor + vmlinux + erofs + envd). Each
# artifact has fetch / (patch) / build sub-stages; idempotency lives inside the
# scripts. cloud-hypervisor and vmlinux are multi-minute cold builds.
# Cross-compile by setting TARGET_ARCH; CROSS_PREFIX auto-derives when
# HOST_ARCH != TARGET_ARCH (distros ship cross packages under the GNU triple).

SHELL := /bin/bash

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
  GO_ARCH     := amd64
  KERNEL_ARCH := x86_64
  RUST_TARGET := x86_64-unknown-linux-gnu
else ifeq ($(TARGET_ARCH),aarch64)
  GO_ARCH     := arm64
  KERNEL_ARCH := arm64
  RUST_TARGET := aarch64-unknown-linux-gnu
else
  $(error unsupported TARGET_ARCH=$(TARGET_ARCH); supported: x86_64, aarch64)
endif

# Auto-derive CROSS_PREFIX when host != target (distros: crossbuild-essential-amd64/arm64).
ifeq ($(HOST_ARCH),$(TARGET_ARCH))
  CROSS_PREFIX :=
else ifeq ($(TARGET_ARCH),x86_64)
  CROSS_PREFIX := x86_64-linux-gnu-
else ifeq ($(TARGET_ARCH),aarch64)
  CROSS_PREFIX := aarch64-linux-gnu-
endif

# ---------------------------------------------------------------------------
# Paths
# ---------------------------------------------------------------------------
GO             := go
GO_BUILD_FLAGS := -trimpath
BINDIR         := bin/$(TARGET_ARCH)
BUILD_DIR      := build/$(TARGET_ARCH)
TARBALL_DIR    := build/tarball

EROFS_BIN     := $(abspath $(BINDIR)/mkfs.erofs)
CH_BIN        := $(abspath $(BINDIR)/cloud-hypervisor)
VMLINUX_BIN   := $(abspath $(BINDIR)/vmlinux)
ENVD_BIN      := $(abspath $(BINDIR)/envd)
VERSITYGW_BIN := $(abspath $(BINDIR)/versitygw)

# Upstream tarballs + optional SHA256 (scripts skip verify when empty).
EROFS_TARBALL                   ?= https://github.com/erofs/erofs-utils/archive/refs/tags/v1.9.1.tar.gz\#erofs-utils-v1.9.1.tar.gz
EROFS_TARBALL_SHA256            ?=
LINUX_TARBALL                   ?= https://cdn.kernel.org/pub/linux/kernel/v6.x/linux-6.1.169.tar.gz
LINUX_TARBALL_SHA256            ?=
CLOUD_HYPERVISOR_TARBALL        ?= https://github.com/cloud-hypervisor/cloud-hypervisor/archive/refs/tags/v51.1.tar.gz\#cloud-hypervisor-51.1.tar.gz
CLOUD_HYPERVISOR_TARBALL_SHA256 ?=
ENVD_TARBALL                    ?= https://github.com/e2b-dev/infra/archive/refs/tags/2026.22.tar.gz\#e2b-infra-2026.22.tar.gz
ENVD_TARBALL_SHA256             ?=
VERSITYGW_TARBALL               ?= https://github.com/versity/versitygw/archive/refs/tags/v1.5.0.tar.gz\#versitygw-1.5.0.tar.gz
VERSITYGW_TARBALL_SHA256        ?=

# WSL2 + /mnt/<drive>/ detection: kernel tags itself "microsoft" and cwd is on
# a 9p/drvfs mount. WSL2 users pay a 5-10x per-file I/O penalty for the ~85k
# files in a kernel tree. They can opt into a Linux-native fs path by creating
# $HOME/linux-build/src — the Makefile auto-redirects LINUX_BUILD_SRC and
# LINUX_BUILD_OUT there.
IS_WSL_DRVFS := $(shell uname -r 2>/dev/null | grep -qi microsoft && pwd | grep -q '^/mnt/' && echo 1)
ifeq ($(IS_WSL_DRVFS),1)
  ifneq ($(wildcard $(HOME)/linux-build/src),)
    LINUX_BUILD_SRC ?= $(HOME)/linux-build/src
    LINUX_BUILD_OUT ?= $(HOME)/linux-build/out/$(TARGET_ARCH)
  endif
endif

# Kernel: arch-neutral source tree (kbuild ARCH= chooses target at build),
# per-arch build output.
LINUX_BUILD_SRC   ?= $(abspath build/src/linux)
LINUX_BUILD_OUT   ?= $(abspath $(BUILD_DIR)/linux)
LINUX_PATCHES_DIR := $(abspath deps/linux-patches)

# cloud-hypervisor: source tree (kept under git, holds patch dev WIP) is
# arch-neutral and shared; cargo target dir is per-arch.
CLOUD_HYPERVISOR_SRC       ?= $(abspath build/src/cloud-hypervisor)
CLOUD_HYPERVISOR_BUILD_OUT ?= $(abspath $(BUILD_DIR)/cloud-hypervisor)
CH_PATCHES_DIR             := $(abspath deps/ch-patches)

# envd: e2b-dev/infra source tree — arch-neutral and shared (Go: GOARCH picks
# the target at build), like cloud-hypervisor / linux.
ENVD_SRC ?= $(abspath build/src/e2b-infra)

# versitygw: versity/versitygw source tree — arch-neutral and shared (Go).
VERSITYGW_SRC ?= $(abspath build/src/versitygw)

# Cross toolchain env for the C/C++ steps (CGO/cargo/kbuild). Empty for native.
ifneq ($(CROSS_PREFIX),)
  CROSS_ENV := CC=$(CROSS_PREFIX)gcc CXX=$(CROSS_PREFIX)g++ AR=$(CROSS_PREFIX)ar STRIP=$(CROSS_PREFIX)strip
else
  CROSS_ENV :=
endif

# Common env passed to every dep build script (matches the source's contract).
DEPS_ENV = \
    TARGET_ARCH="$(TARGET_ARCH)" \
    KERNEL_ARCH="$(KERNEL_ARCH)" \
    RUST_TARGET="$(RUST_TARGET)" \
    CROSS_PREFIX="$(CROSS_PREFIX)" \
    BUILD_DIR="$(abspath $(BUILD_DIR))" \
    BINDIR="$(abspath $(BINDIR))" \
    TARBALL_CACHE="$(abspath $(TARBALL_DIR))"

# Native-only symlink: bin/<name> -> $(TARGET_ARCH)/<name>. Cross builds skip
# (the symlink would point to a non-runnable binary on the host).
define link_bin
@if [ "$(HOST_ARCH)" = "$(TARGET_ARCH)" ]; then \
   mkdir -p bin && ln -sfn $(TARGET_ARCH)/$(1) bin/$(1); \
 fi
endef

# ---------------------------------------------------------------------------
# Targets
# ---------------------------------------------------------------------------
.PHONY: all build erofs cloud-hypervisor vmlinux envd versitygw \
        ch-fetch ch-patches-apply ch-patches ch-patches-format ch-build \
        linux-fetch linux-patches-apply linux-patches linux-patches-format linux-build \
        clean help

all: build

# All native artifacts. cloud-hypervisor and vmlinux are multi-minute cold
# builds; erofs + envd are the fastest (subminute on a warm tarball cache).
build: cloud-hypervisor vmlinux erofs envd

# --- erofs-utils (mkfs.erofs) ----------------------------------------------
erofs: $(EROFS_BIN)
$(EROFS_BIN):
	$(DEPS_ENV) \
	EROFS_TARBALL="$(EROFS_TARBALL)" \
	EROFS_TARBALL_SHA256="$(EROFS_TARBALL_SHA256)" \
		bash deps/build-erofs.sh
	$(call link_bin,mkfs.erofs)

# --- envd (e2b guest agent; injected into sandbox-runtime-e2b.erofs) -------
envd: $(ENVD_BIN)
$(ENVD_BIN):
	$(DEPS_ENV) \
	ENVD_TARBALL="$(ENVD_TARBALL)" \
	ENVD_TARBALL_SHA256="$(ENVD_TARBALL_SHA256)" \
	ENVD_SRC="$(ENVD_SRC)" \
	GO_ARCH="$(GO_ARCH)" \
		bash deps/build-envd.sh
	$(call link_bin,envd)

# --- versitygw (S3 gateway for local/single-node COPY file storage) --------
# Opt-in (NOT in `build`): only deployments using builder.files_storage
# without a cloud object store need it. Multi-second Go build.
versitygw: $(VERSITYGW_BIN)
$(VERSITYGW_BIN):
	$(DEPS_ENV) \
	VERSITYGW_TARBALL="$(VERSITYGW_TARBALL)" \
	VERSITYGW_TARBALL_SHA256="$(VERSITYGW_TARBALL_SHA256)" \
	VERSITYGW_SRC="$(VERSITYGW_SRC)" \
	GO_ARCH="$(GO_ARCH)" \
		bash deps/build-versitygw.sh
	$(call link_bin,versitygw)

# --- cloud-hypervisor (patched Rust VMM) -----------------------------------
CH_INVOKE = $(DEPS_ENV) \
    CLOUD_HYPERVISOR_TARBALL="$(CLOUD_HYPERVISOR_TARBALL)" \
    CLOUD_HYPERVISOR_TARBALL_SHA256="$(CLOUD_HYPERVISOR_TARBALL_SHA256)" \
    CH_SRC="$(CLOUD_HYPERVISOR_SRC)" \
    CH_BUILD_OUT="$(CLOUD_HYPERVISOR_BUILD_OUT)" \
    PATCHES_DIR="$(CH_PATCHES_DIR)"

ch-fetch:
	STAGE=fetch $(CH_INVOKE) bash deps/build-cloud-hypervisor.sh
ch-patches-apply: ch-fetch
	STAGE=patches-apply $(CH_INVOKE) bash deps/build-cloud-hypervisor.sh
ch-patches: ch-patches-apply
ch-patches-format:
	STAGE=patches-format $(CH_INVOKE) bash deps/build-cloud-hypervisor.sh
ch-build:
	STAGE=build $(CH_INVOKE) bash deps/build-cloud-hypervisor.sh
cloud-hypervisor: ch-patches-apply ch-build
	$(call link_bin,cloud-hypervisor)

# --- vmlinux (guest kernel) ------------------------------------------------
LINUX_INVOKE = $(DEPS_ENV) \
    LINUX_TARBALL="$(LINUX_TARBALL)" \
    LINUX_TARBALL_SHA256="$(LINUX_TARBALL_SHA256)" \
    LINUX_BUILD_SRC="$(LINUX_BUILD_SRC)" \
    LINUX_BUILD_OUT="$(LINUX_BUILD_OUT)" \
    LINUX_PATCHES_DIR="$(LINUX_PATCHES_DIR)"

linux-fetch:
	STAGE=fetch $(LINUX_INVOKE) bash deps/build-vmlinux.sh
linux-patches-apply: linux-fetch
	STAGE=patches-apply $(LINUX_INVOKE) bash deps/build-vmlinux.sh
linux-patches: linux-patches-apply
linux-patches-format:
	STAGE=patches-format $(LINUX_INVOKE) bash deps/build-vmlinux.sh
linux-build:
	STAGE=build $(LINUX_INVOKE) bash deps/build-vmlinux.sh
vmlinux: linux-patches-apply linux-build
	$(call link_bin,vmlinux)

clean:
	# Preserve build/tarball/ (re-downloading source is expensive) and the
	# arch-neutral source trees under build/src/ (may contain WIP patch dev
	# under build/src/cloud-hypervisor/.git, build/src/linux/.git).
	rm -rf $(CLOUD_HYPERVISOR_BUILD_OUT) $(LINUX_BUILD_OUT) $(BUILD_DIR)/src/erofs-utils
	rm -rf bin

help:
	@echo "sandbox-deps — native dependency builds. Targets:"
	@echo "  build (=all)          cloud-hypervisor + vmlinux + erofs + envd (multi-min cold)"
	@echo "  erofs                 build mkfs.erofs (erofs-utils)"
	@echo "  vmlinux               build the guest kernel (~5-10 min cold)"
	@echo "  cloud-hypervisor      build the patched VMM (~minutes cold)"
	@echo "  envd                  build the e2b guest agent (e2b-dev/infra; pin via ENVD_TARBALL)"
	@echo "  versitygw             build the S3 gateway (opt-in; for builder.files_storage on single-node/local)"
	@echo "  ch-patches-format     extract HEAD CH commits back to deps/ch-patches/"
	@echo "  linux-patches-format  extract HEAD linux commits back to deps/linux-patches/"
	@echo "  clean                 wipe build artifacts (preserves tarball cache + source trees)"
	@echo "  TARGET_ARCH           x86_64 (default) | aarch64; CROSS_PREFIX auto-derives"
