.PHONY: verify verify-static verify-template check ci fmt lint lint-version-check test race cross tidy build corrections-base

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

# The GOOS/GOARCH pairs each module builds for. make cross builds and vets
# every pair; make ci runs make cross.
CROSS_proto ?= linux/amd64 linux/arm64 darwin/arm64 windows/amd64
CROSS_service ?= linux/amd64 linux/arm64 darwin/arm64
CROSS_desktop ?= linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64

# Where make build writes the commands.
BUILD_DIR ?= build

# The tests need no display. Unsetting DISPLAY and WAYLAND_DISPLAY proves it
# on a desktop too.
HEADLESS = env -u DISPLAY -u WAYLAND_DISPLAY

fmt:
	gofmt -s -w $(MODULES)

tidy:
	@for m in $(MODULES); do (cd $$m && $(GOCMD) mod tidy) || exit 1; done

test:
	@for m in $(MODULES); do echo "==> test: $$m"; (cd $$m && $(HEADLESS) $(GOCMD) test ./...) || exit 1; done

# The race detector needs CGO and a C compiler.
race:
	@for m in $(MODULES); do echo "==> race: $$m"; (cd $$m && $(HEADLESS) CGO_ENABLED=1 $(GOCMD) test -race -short ./...) || exit 1; done

# Build and vet every package, tests included, for each module's targets.
# -o /dev/null discards every output, so no command lands in a module.
cross:
	@for m in $(MODULES); do \
		case $$m in proto) targets="$(CROSS_proto)";; service) targets="$(CROSS_service)";; desktop) targets="$(CROSS_desktop)";; esac; \
		for target in $$targets; do \
			goos=$${target%/*}; goarch=$${target#*/}; \
			echo "==> cross: $$m $$goos/$$goarch"; \
			(cd $$m && GOOS=$$goos GOARCH=$$goarch $(GOCMD) build -o /dev/null ./... && GOOS=$$goos GOARCH=$$goarch $(GOCMD) vet ./...) || exit 1; \
		done; \
	done

build:
	@mkdir -p $(BUILD_DIR)
	cd service && $(GOCMD) build -o ../$(BUILD_DIR)/pocketd ./cmd/pocketd
	cd desktop && $(GOCMD) build -o ../$(BUILD_DIR)/pocket ./cmd/pocket

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
	test -z "$$(gofmt -s -l $(MODULES))"
	@echo "==> verify: lint"
	$(MAKE) --no-print-directory lint
	@echo "==> verify: template, docs index, risk paths, corrections, vendored copies"
	$(MAKE) --no-print-directory verify-template

# The credential-free gate that leaves git status unchanged: verify-static,
# then every test.
verify: verify-static
	@echo "==> verify: tests"
	$(MAKE) --no-print-directory test

# Developer gate: format, then verify.
check: fmt verify

# What CI's verify job runs, in fail-fast order: verify, the corrections check
# against BASE when it is set (make ci BASE=origin/main), the race detector and
# the cross-build of every module's targets.
ci: verify
	$(MAKE) --no-print-directory corrections-base
	@echo "==> ci: race detector"
	$(MAKE) --no-print-directory race
	@echo "==> ci: cross-build"
	$(MAKE) --no-print-directory cross
	@echo "==> ci passed"

# .memory/corrections/ is checked against BASE (no record removed or renamed,
# only an empty promoted_to filled, and the old corrections file does not come
# back). Without BASE the check is skipped.
corrections-base:
	@if [ -n "$(BASE)" ]; then node scripts/check-corrections.mjs --base "$(BASE)"; fi
