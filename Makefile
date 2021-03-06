SHELL := /bin/bash
BUILD_DIR = ./ais/setup
BUILD_SRC = $(BUILD_DIR)/aisnode.go

# Do not print enter/leave directory when doing 'make -C DIR <target>'
MAKEFLAGS += --no-print-directory

# Build version, flags, and tags
VERSION = $(shell git rev-parse --short HEAD)
BUILD = $(shell date +%FT%T%z)
BUILD_FLAGS += $(if $(strip $(GORACE)),-race,)
ifdef TAGS
	BUILD_TAGS = "$(CLDPROVIDER) $(TAGS)"
else
	BUILD_TAGS = $(CLDPROVIDER)
endif

# Profiling
# Example usage: MEMPROFILE=/tmp/mem make kill clean deploy <<< $'5\n5\n4\n0'
# Note that MEMPROFILE (memprofile) option requires graceful shutdown (see `kill:`)
ifdef MEMPROFILE
	export PROFILE = -memprofile=$(MEMPROFILE)
	BUILD_SRC = ./ais/setup/profile/aisnode.go
endif
ifdef CPUPROFILE
	export PROFILE += -cpuprofile=$(CPUPROFILE)
	BUILD_SRC = ./ais/setup/profile/aisnode.go
endif

# Intra-cluster networking: two alternative ways to build AIS `transport` package:
# 1) using Go net/http, or
# 2) with a 3rd party github.com/valyala/fasthttp aka "fasthttp"
#
# The second option is the current default.
# To build with net/http, use `nethttp` build tag, for instance:
# TAGS=nethttp make deploy <<< $'5\n5\n4\n0'

ifeq ($(MODE),debug)
	# Debug mode
	GCFLAGS = -gcflags="all=-N -l"
	LDFLAGS = -ldflags "-X 'main.version=$(VERSION)' -X 'main.build=$(BUILD)'"
else
	# Production mode
	GCFLAGS =
	LDFLAGS = -ldflags "-w -s -X 'main.version=$(VERSION)' -X 'main.build=$(BUILD)'"
endif

# Colors
cyan = $(shell { tput setaf 6 || tput AF 6; } 2>/dev/null)
term-reset = $(shell { tput sgr0 || tput me; } 2>/dev/null)
$(call make-lazy,cyan)
$(call make-lazy,term-reset)

.PHONY: all node cli aisfs authn cli-autocomplete

all: node cli aisfs authn ## Build all main binaries

node: ## Build 'aisnode' binary
	@echo "Building aisnode: version=$(VERSION) provider=$(CLDPROVIDER)"
ifneq ($(strip $(GORACE)),)
	@echo "Deploying with race detector, writing reports to $(subst log_path=,,$(GORACE)).<pid>"
endif
	@GORACE=$(GORACE) GODEBUG="madvdontneed=1" \
		go install $(BUILD_FLAGS) -tags=$(BUILD_TAGS) $(GCFLAGS) $(LDFLAGS) $(BUILD_SRC)
	@echo "done."

aisfs: ## Build 'aisfs' binary
	@echo "Building aisfs..."
	@cd fuse && ./install.sh

cli: ## Build CLI ('ais' binary)
	@echo "Building ais CLI..."
	@cd cli && ./install.sh --ignore-autocomplete

cli-autocomplete: ## Add CLI autocompletions
	@echo "Adding CLI autocomplete..."
	@cd cli/autocomplete && ./install.sh

authn: ## Build 'authn' binary
	@echo -n "Building authn... "
	@go install $(BUILD_FLAGS) $(LDFLAGS) ./authn
	@echo "done."

aisloader: ## Build 'aisloader' binary
	@echo -n "Building aisloader... "
	@go install $(LDFLAGS) ./bench/aisloader
	@echo "done."

#
# local deployment (intended for developers)
#
.PHONY: deploy

deploy: ## Build 'aisnode' and deploy the specified numbers of local AIS proxies and targets
	@"$(BUILD_DIR)/deploy.sh"

#
# cleanup local deployment (cached objects, logs, and executables)
#
.PHONY: kill rmcache clean

# when profiling, make sure to let it flush accumulated stats
# e.g., insert sleep prior to SIGKILL or remove it altogether
kill: ## Kill all locally deployed targets and proxies
	@echo -n "Local AIS shutdown... "
	@pkill -SIGINT aisnode 2>/dev/null; true
	@pkill authn 2>/dev/null; true
	@pkill -SIGKILL aisnode 2>/dev/null; true
	@echo "done."

# delete only caches, not logs
rmcache: ## Delete AIS related caches
	@"$(BUILD_DIR)/rmcache.sh"

clean: ## Remove all AIS related files and binaries
	@echo -n "Cleaning... "
	@rm -rf ~/.ais* && \
		rm -rf /tmp/ais* && \
		rm -f $(GOPATH)/bin/ais* # cleans 'ais' (CLI), 'aisnode' (TARGET/PROXY), 'aisfs' (FUSE), 'aisloader' && \
		rm -f $(GOPATH)/pkg/linux_amd64/github.com/NVIDIA/aistore/aisnode.a
	@echo "done."

#
# go modules
#

.PHONY: mod mod-clean mod-tidy

mod: mod-clean mod-tidy ## Do Go modules chores

# cleanup gomod cache
mod-clean: ## Clean modules
	go clean --modcache

mod-tidy: ## Remove unused modules from 'go.mod'
	go mod tidy

# Target for local docker deployment
.PHONY: deploy-docker stop-docker

deploy-docker: ## Deploy AIS cluster inside the dockers
# pass -d=3 because need 3 mountpaths for some tests
	@cd ./deploy/dev/docker && ./deploy_docker.sh -d=3

stop-docker: ## Stop deployed AIS docker cluster
ifeq ($(FLAGS),)
	$(warning missing environment variable: FLAGS="stop docker flags")
endif
	@./deploy/dev/docker/stop_docker.sh $(FLAGS)

#
# tests
#
.PHONY: test-soak test-envcheck test-short test-long test-run test-docker test

# Target for soak test
test-soak: ## Run soaking tests
ifeq ($(FLAGS),)
	$(warning FLAGS="soak test flags" not passed, using defaults)
endif
	@./bench/soaktest/soaktest.sh $(FLAGS)


test-envcheck:
	@$(SHELL) "$(BUILD_DIR)/bootstrap.sh" test-env

test-short: test-envcheck ## Run short tests (requires BUCKET variable to be set)
	@BUCKET=$(BUCKET) AISURL=$(AISURL) $(SHELL) "$(BUILD_DIR)/bootstrap.sh" test-short

test-long: test-envcheck ## Run all (long) tests (requires BUCKET variable to be set)
	@BUCKET=$(BUCKET) AISURL=$(AISURL) $(SHELL) "$(BUILD_DIR)/bootstrap.sh" test-long

test-run: test-envcheck # runs tests matching a specific regex
ifeq ($(RE),)
	$(error missing environment variable: RE="testnameregex")
endif
	@RE=$(RE) BUCKET=$(BUCKET) AISURL=$(AISURL) $(SHELL) "$(BUILD_DIR)/bootstrap.sh" test-run

test-docker: ## Run tests inside docker
	@$(SHELL) "$(BUILD_DIR)/bootstrap.sh" test-docker

ci: spell-check fmt-check lint test-short ## Run CI related checkers and linters (requires BUCKET variable to be set)


# Target for linters
.PHONY: lint-update lint fmt-check fmt-fix spell-check spell-fix cyclo

lint-update: ## Update the linter version (removes previous one and downloads a new one)
	@rm -f $(GOPATH)/bin/golangci-lint
	@curl -sfL "https://install.goreleaser.com/github.com/golangci/golangci-lint.sh" | sh -s -- -b $(GOPATH)/bin latest

lint: ## Run linter on whole project
	@([[ ! -f $(GOPATH)/bin/golangci-lint ]] && curl -sfL "https://install.goreleaser.com/github.com/golangci/golangci-lint.sh" | sh -s -- -b $(GOPATH)/bin latest) || true
	@$(SHELL) "$(BUILD_DIR)/bootstrap.sh" lint

fmt-check: ## Check code formatting
	@$(SHELL) "$(BUILD_DIR)/bootstrap.sh" fmt

fmt-fix: ## Fix code formatting
	@$(SHELL) "$(BUILD_DIR)/bootstrap.sh" fmt --fix

spell-check: ## Run spell checker on the project
	@GOOS="" GO111MODULE=off go get -u github.com/client9/misspell/cmd/misspell
	@$(SHELL) "$(BUILD_DIR)/bootstrap.sh" spell

spell-fix: ## Fix spell checking issues
	@GOOS="" GO111MODULE=off go get -u github.com/client9/misspell/cmd/misspell
	@$(SHELL) "$(BUILD_DIR)/bootstrap.sh" spell --fix


# Misc Targets
.PHONY: numget cpuprof flamegraph code-coverage

# example extracting 'numget' stats out of all local logs
numget:
	@"$(BUILD_DIR)/numget.sh"

# run benchmarks 10 times to generate cpu.prof
cpuprof:
	@go test -v -run=XXX -bench=. -count 10 -cpuprofile=/tmp/cpu.prof

flamegraph: cpuprof
	@go tool pprof -http ":6060" /tmp/cpu.prof

code-coverage:
	@"$(BUILD_DIR)/code_coverage.sh"

# Target for devinit
.PHONY: devinit

# To help with working with a non-github remote
# It replaces existing github.com AIStore remote with the one specified by REMOTE
devinit:
	@$(SHELL) "$(BUILD_DIR)/bootstrap.sh" devinit

.PHONY: help
help:
	@echo "Usage:"
	@echo "  [VAR=foo VAR2=bar...] make [target...]"
	@echo ""
	@echo "Useful commands:"
	@grep -Eh '^[a-zA-Z._-]+:.*?## .*$$' $(MAKEFILE_LIST) | sort | awk 'BEGIN {FS = ":.*?## "}; {printf "  $(cyan)%-30s$(term-reset) %s\n", $$1, $$2}'
	@echo ""
	@echo "Examples:"
	@printf "  $(cyan)%s$(term-reset)\n    %s\n\n" \
		"make deploy" "Deploy cluster locally" \
		"make kill clean" "Stop locally deployed cluster and cleanup all cluster-related data and bucket metadata (but not cluster map)" \
		"make kill deploy <<< $'7\n2\n4\n1'"  "Stop and then deploy (non-interactively) cluster consisting of 7 targets (4 mountpaths each) and 2 proxies" \
		"GORACE='log_path=/tmp/race' make deploy" "Deploy cluster with race detector, write reports to /tmp/race.<PID>" \
		"MODE=debug make deploy" "Deploy cluster with aisnode (AIS target and proxy) executable built with debug info" \
		"BUCKET=tmp make test-short" "Run all short tests" \
		"BUCKET=<existing-cloud-bucket> make test-long" "Run all tests" \
		"BUCKET=tmp make ci" "Run style, lint, and spell checks, as well as all short tests" \
		"MEMPROFILE=/tmp/mem make deploy" "Deploy cluster with memory profiling enabled, write reports to /tmp/mem.<PID> (and make sure to stop gracefully)" \
		"CPUPROFILE=/tmp/cpu make deploy" "Build and deploy cluster instrumented for CPU profiling, write reports to /tmp/cpu.<PID>" \
		"TAGS=nethttp make deploy" "Build 'transport' package with net/http (see transport/README.md) and deploy cluster locally"
