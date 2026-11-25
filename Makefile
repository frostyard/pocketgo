.PHONY: verify verify-static verify-template check ci fmt fmt-check lint lint-version-check test race cross portable tidy build deploy corrections-base run app

# A bare `make` verifies; formatting stays an explicit `make fmt` or `make check`.
.DEFAULT_GOAL := verify

.PHONY: linux-client rc nsgo-live nsgo-pictures
HOST_OS := $(shell uname -s)
HOST_ARCH := $(shell uname -m)
NSGO_ARCH := $(patsubst x86_64,x64,$(patsubst aarch64,arm64,$(HOST_ARCH)))

GOCMD ?= go

# The Go modules of go.work (ADR-0007). Every Go gate runs once in each.
MODULES := proto service desktop

# Everything builds without CGO (ADR-0007). Only make race turns it on, for
# the race detector.
CGO_ENABLED = 0
export CGO_ENABLED

# Pinned golangci-lint release, read from mise.toml, the single source of
# every tool pin: `mise install` provisions it locally and CI installs it with
# jdx/mise-action, verified against mise.lock. Compared with `golangci-lint
# version --short`, which prints the bare "MAJOR.MINOR.PATCH".
GOLANGCI_LINT_VERSION := $(strip $(shell sed -n 's/^golangci-lint = "\(.*\)"/\1/p' mise.toml))
# The Go release from go.work's go line, the only Go pin. golangci-lint must
# be built with a Go of at least this language version (MAJOR.MINOR).
GO_VERSION := $(strip $(shell sed -n 's/^go \(.*\)/\1/p' go.work))
GO_LANGUAGE := $(shell echo "$(GO_VERSION)" | cut -d. -f1,2)
# The major version of the Node on PATH (mise.toml pins it): the template gate
# runs on it.
NODE_MAJOR := $(shell node -p "process.versions.node.split('.')[0]" 2>/dev/null)

# The GOOS/GOARCH pairs each module builds for, as CROSS_<module>. make cross
# builds and vets every pair and fails for a module without a list; make ci runs
# make cross.
CROSS_proto ?= linux/amd64 linux/arm64 darwin/arm64 windows/amd64
CROSS_service ?= linux/amd64 linux/arm64 darwin/arm64
CROSS_desktop ?= linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64

# The iPad and the iPhone run the NSGo client over the desktop's rules
# (ADR-0071). make portable builds proto and PORTABLE_desktop, the desktop's
# packages that import no UI, for each of PORTABLE_TARGETS with CGO off, so a
# dependency or a call that iOS lacks fails CI; make ci runs it. Linking for
# iOS needs cgo and Xcode, on a Mac, and go vet for iOS refuses CGO off, so it
# only builds.
PORTABLE_TARGETS ?= ios/arm64
PORTABLE_desktop ?= ./internal/state ./internal/config ./internal/github

# Where make build writes the commands, relative to the repository root or
# absolute; BUILD_PATH is its absolute form, so a build inside a module writes
# where BUILD_DIR says.
BUILD_DIR ?= build
BUILD_PATH = $(abspath $(BUILD_DIR))

# The tests need no display. Unsetting DISPLAY and WAYLAND_DISPLAY proves it
# on a desktop too: the NSGo client's view tests run on NSGo's headless host.
HEADLESS = env -u DISPLAY -u WAYLAND_DISPLAY

fmt:
	find $(MODULES) -type d -name .nsgo -prune -o -type f -name '*.go' -exec gofmt -s -w {} +

# Fail when gofmt itself fails (a file it cannot parse) or names a file that
# needs formatting; print what it reported either way.
fmt-check:
	@out="$$(find $(MODULES) -type d -name .nsgo -prune -o -type f -name '*.go' -exec gofmt -s -l {} +)" || { echo "gofmt failed"; exit 1; }; \
	if [ -n "$$out" ]; then echo "gofmt: these files need formatting (run make fmt):"; echo "$$out"; exit 1; fi

tidy:
	@for m in $(MODULES); do (cd $$m && $(GOCMD) mod tidy) || exit 1; done

test:
	@for m in $(MODULES); do echo "==> test: $$m"; (cd $$m && $(HEADLESS) $(GOCMD) test ./...) || exit 1; done

# The race detector needs CGO and a C compiler. Each package's run gets
# RACE_TIMEOUT, not go test's 10-minute default: service/internal/api takes
# 5 to 7 minutes under -race on CI's runners and passed 10 on one, which
# failed verify with no test at fault (frostyard/pocketgo#114). The full local
# run keeps that larger default; CI's race shards pass a smaller timeout that
# stays below the race job's limit, checked by test/makefile.test.mjs.
# CI can run the race detector as RACE_SHARDS parallel jobs. make race runs
# shard RACE_SHARD (1-based) of RACE_SHARDS. With one shard it runs everything
# without listing. With more shards, each module lists with -race -short,
# keeps tests, examples and fuzz targets (not benchmarks; names may contain
# Unicode letters, but a line with whitespace is output, not a name), sorts
# with LC_ALL=C, and takes every RACE_SHARDS-th name starting at RACE_SHARD.
# Together the shards run each name once in every package that has it, with
# the same flags and timeout. make ci uses these same defaults.
RACE_TIMEOUT ?= 20m
RACE_SHARDS ?= 1
RACE_SHARD ?= 1

race:
	@if ! awk -v shard='$(RACE_SHARD)' -v shards='$(RACE_SHARDS)' 'BEGIN { exit !(shard ~ /^[1-9][0-9]*$$/ && shards ~ /^[1-9][0-9]*$$/ && shard + 0 <= shards + 0) }'; then \
		echo "race: RACE_SHARD=$(RACE_SHARD) RACE_SHARDS=$(RACE_SHARDS) is not a shard (1 <= RACE_SHARD <= RACE_SHARDS)"; exit 1; \
	fi; \
	for m in $(MODULES); do \
		if [ '$(RACE_SHARDS)' = 1 ]; then \
			echo "==> race: $$m"; \
			(cd $$m && $(HEADLESS) CGO_ENABLED=1 $(GOCMD) test -race -short -timeout $(RACE_TIMEOUT) ./...) || exit 1; \
			continue; \
		fi; \
		echo "==> race: $$m shard $(RACE_SHARD)/$(RACE_SHARDS)"; \
		listing="$$(cd $$m && $(HEADLESS) CGO_ENABLED=1 $(GOCMD) test -race -short -list . ./...)" || { printf '%s\n' "$$listing"; exit 1; }; \
		names="$$(printf '%s\n' "$$listing" | grep -E '^(Test|Example|Fuzz)[^[:space:]]*$$' | LC_ALL=C sort -u | \
			awk -v shard='$(RACE_SHARD)' -v shards='$(RACE_SHARDS)' '(NR-1) % shards == shard-1' | paste -sd '|' -)"; \
		if [ -z "$$names" ]; then echo "==> race: $$m has no tests in this shard"; continue; fi; \
		(cd $$m && $(HEADLESS) CGO_ENABLED=1 $(GOCMD) test -race -short -timeout $(RACE_TIMEOUT) -run "^($$names)$$" ./...) || exit 1; \
	done

# Build and vet every package, tests included, for each module's targets.
# -o /dev/null discards every output, so no command lands in a module.
cross:
	$(foreach m,$(MODULES),$(if $(strip $(CROSS_$(m))),,$(error CROSS_$(m) is not set: give module $(m) its GOOS/GOARCH list in the Makefile)))
	@set -e; $(foreach m,$(MODULES),for target in $(CROSS_$(m)); do \
		goos=$${target%/*}; goarch=$${target#*/}; \
		echo "==> cross: $(m) $$goos/$$goarch"; \
		(cd $(m) && GOOS=$$goos GOARCH=$$goarch $(GOCMD) build -o /dev/null ./... && GOOS=$$goos GOARCH=$$goarch $(GOCMD) vet ./...); \
	done;)

# Build proto and the desktop's UI-free packages for the phone's targets with
# CGO off (ADR-0071).
portable:
	@set -e; for target in $(PORTABLE_TARGETS); do \
		goos=$${target%/*}; goarch=$${target#*/}; \
		echo "==> portable: proto $$goos/$$goarch"; \
		(cd proto && CGO_ENABLED=0 GOOS=$$goos GOARCH=$$goarch $(GOCMD) build -o /dev/null ./...); \
		echo "==> portable: desktop $(PORTABLE_desktop) $$goos/$$goarch"; \
		(cd desktop && CGO_ENABLED=0 GOOS=$$goos GOARCH=$$goarch $(GOCMD) build -o /dev/null $(PORTABLE_desktop)); \
	done

build: daemon

.PHONY: daemon
daemon:
	@mkdir -p "$(BUILD_PATH)"
	cd service && $(GOCMD) build -o "$(BUILD_PATH)/pocketd" ./cmd/pocketd
ifeq ($(HOST_OS),Linux)
build: linux-client
run: linux-client
	"$(BUILD_PATH)/pocket"
app:
	cd desktop/cmd/pocket-nsgo && $(GOCMD) tool nsgo build -target linux_$(NSGO_ARCH)
else
# The desktop client is the NSGo client alone (ADR-0071). Its macOS app links
# through Xcode's tools, as .github/workflows/apple.yml builds it; this
# Makefile builds the client on Linux alone.
run app:
	@echo "make $@ builds the desktop client on Linux alone: on a Mac, run go tool nsgo build -target macos_arm64 in desktop/cmd/pocket-nsgo, as .github/workflows/apple.yml does"; exit 1
endif

# Use the NSGo packager so the executable carries the configured app identity
# and loads its matching terminal library beside it.
linux-client:
	@mkdir -p "$(BUILD_PATH)"
	@set -eu; tmp=$$(mktemp -d); trap 'rm -rf "$$tmp"' EXIT; \
	(cd desktop/cmd/pocket-nsgo && $(GOCMD) tool nsgo build -installers=false -target linux_$(NSGO_ARCH) -output "$$tmp"); \
	set -- "$$tmp"/PocketGo-*-linux_$(NSGO_ARCH); \
	install -m755 "$$1/PocketGo" "$(BUILD_PATH)/pocket"; \
	install -m755 "$$1/libghostty-vt.so" "$(BUILD_PATH)/libghostty-vt.so"

rc:
	GOCMD="$(GOCMD)" packaging/build-rc.sh "$(BUILD_PATH)/rc"

# The NSGo client's suite in a real Linux window, and its kept pictures taken
# again (desktop/internal/nsgoclient/comparison/linux). Xvfb gives the run a
# screen of one size on every host and keeps its windows off the desktop.
# GDK_BACKEND=x11 is what keeps them off: without it GTK tries Wayland first,
# and with WAYLAND_DISPLAY unset it still finds a desktop's compositor at the
# default socket, wayland-0. GTK reads testdata/xdg's settings file last of
# the system's: it stops the text caret's blinking. make nsgo-live writes its
# pictures to desktop/internal/nsgoclient/.nsgo/test/screenshots/linux, which
# git ignores. Neither target is part of make verify or make ci.
NSGO_CLIENT := desktop/internal/nsgoclient
NSGO_LIVE = env -u WAYLAND_DISPLAY GDK_BACKEND=x11 XDG_SESSION_TYPE=x11 \
	XDG_CONFIG_DIRS=/etc/xdg:$(CURDIR)/$(NSGO_CLIENT)/testdata/xdg \
	xvfb-run -a -s "-screen 0 1600x1200x24" $(GOCMD) tool nsgo test -target linux
nsgo-live:
	@command -v xvfb-run >/dev/null 2>&1 || { echo "make nsgo-live needs xvfb-run (the xvfb package)"; exit 1; }
	cd $(NSGO_CLIENT) && $(NSGO_LIVE)

nsgo-pictures:
	@command -v xvfb-run >/dev/null 2>&1 || { echo "make nsgo-pictures needs xvfb-run (the xvfb package)"; exit 1; }
	cd $(NSGO_CLIENT) && $(NSGO_LIVE) -screenshots comparison/linux

# Build pocketd for the target's architecture and install it there as a
# systemd user service (deploy/deploy.sh): over ssh with DEPLOY_HOST=USER@HOST,
# or over incus with DEPLOY_INCUS=REMOTE:INSTANCE, DEPLOY_INCUS_PROJECT and
# DEPLOY_USER. Re-running it upgrades in place; running tasks resume after the
# restart.
deploy:
	@test -n "$(DEPLOY_HOST)$(DEPLOY_INCUS)" || { echo "usage: make deploy DEPLOY_HOST=USER@HOST, or DEPLOY_INCUS=REMOTE:INSTANCE DEPLOY_USER=USER [DEPLOY_INCUS_PROJECT=PROJECT]"; exit 1; }
	GOCMD="$(GOCMD)" DEPLOY_INCUS="$(DEPLOY_INCUS)" DEPLOY_INCUS_PROJECT="$(DEPLOY_INCUS_PROJECT)" DEPLOY_USER="$(DEPLOY_USER)" deploy/deploy.sh $(DEPLOY_HOST)

# Lint each module (fails when the mise.toml pin is missing or the installed
# release differs), as the host and as macOS and Windows.
lint:
	$(MAKE) --no-print-directory lint-version-check
	@for m in $(MODULES); do \
		echo "==> lint: $$m"; \
		(cd $$m && golangci-lint run && GOOS=darwin GOARCH=arm64 golangci-lint run && GOOS=windows GOARCH=amd64 golangci-lint run) || exit 1; \
	done

# Fail unless the installed golangci-lint is the mise.toml pin and was built
# with a Go no older than go.work's go line.
lint-version-check:
	@test -n "$(GOLANGCI_LINT_VERSION)" || { echo "mise.toml pins no golangci-lint"; exit 1; }
	@installed="$$(golangci-lint version --short 2>/dev/null)" || { \
		echo "golangci-lint $(GOLANGCI_LINT_VERSION) is required (not installed; run: mise install)"; exit 1; }; \
	if [ "$$installed" != "$(GOLANGCI_LINT_VERSION)" ]; then \
		echo "expected golangci-lint $(GOLANGCI_LINT_VERSION), found $$installed (run: mise install)"; \
		exit 1; \
	fi; \
	built="$$(golangci-lint version 2>/dev/null | sed -n 's/.*built with go\([0-9]*\.[0-9]*\).*/\1/p')"; \
	if [ -n "$$built" ] && [ "$$(printf '%s\n%s\n' "$(GO_LANGUAGE)" "$$built" | sort -V | head -1)" != "$(GO_LANGUAGE)" ]; then \
		echo "golangci-lint $(GOLANGCI_LINT_VERSION) was built with go$$built, older than go.work's go$(GO_LANGUAGE): bump golangci-lint first"; \
		exit 1; \
	fi

# Check the agent surfaces, docs index, risk paths, corrections and vendored
# copies (needs Node.js 22 or later, from mise.toml).
verify-template:
	@if ! command -v node >/dev/null 2>&1; then echo "FAIL: make verify requires Node.js 22 or later and make; run: mise install"; exit 1; fi
	@if [ "$(NODE_MAJOR)" = "" ] || [ "$(NODE_MAJOR)" -lt 22 ]; then echo "FAIL: make verify requires Node.js 22 or later and make (found: $$(node --version 2>/dev/null || echo missing)); run: mise install"; exit 1; fi
	node scripts/check-template.mjs
	node --test test/*.test.mjs

# The non-mutating static checks: tidy diff, vet, gofmt, the pinned lint and
# the template gate.
verify-static:
	@for m in $(MODULES); do echo "==> verify: $$m go.mod is tidy"; (cd $$m && $(GOCMD) mod tidy -diff) || exit 1; done
	@for m in $(MODULES); do echo "==> verify: $$m go vet"; (cd $$m && $(GOCMD) vet ./...) || exit 1; done
	@echo "==> verify: gofmt"
	$(MAKE) --no-print-directory fmt-check
	@echo "==> verify: lint"
	$(MAKE) --no-print-directory lint
	@echo "==> verify: template, docs index, risk paths, corrections, vendored copies"
	$(MAKE) --no-print-directory verify-template

# The credential-free gate that leaves git status unchanged: verify-static,
# then every test.
verify: verify-static
	@echo "==> verify: tests"
	$(MAKE) --no-print-directory test

# Developer gate: format, then verify, in that order even under make -j.
check:
	$(MAKE) --no-print-directory fmt
	$(MAKE) --no-print-directory verify

# What CI runs, in fail-fast order (make builds prerequisites left to right
# without -j): verify's two parts (verify-static, then test), the corrections
# check against BASE when set (make ci BASE=origin/main), the race detector,
# every module's cross-build and the phone's portability step. CI runs the same
# targets as parallel jobs (.github/workflows/ci.yml), each prerequisite as
# make <target>, checked by core baseline row B12 (core ADR-0073). A prerequisite
# added here needs a job step in ci.yml in the same change; ci has no recipe.
ci: verify-static test corrections-base race cross portable

# .memory/corrections/ is checked against BASE (no record removed or renamed,
# only an empty promoted_to filled, and the old corrections file does not come
# back). Without BASE the check is skipped.
corrections-base:
	@if [ -n "$(BASE)" ]; then node scripts/check-corrections.mjs --base "$(BASE)"; fi
